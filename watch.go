// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

const defaultRescanInterval = 5 * time.Minute

// The event reader never hashes files or waits for Folder.mu. Events arriving
// during a scan remain pending for the following cycle. Only the cycle worker
// reconciles directory watches and advances scanned/nextRescan.
type folderWatcher struct {
	f          *Folder
	native     *fsnotify.Watcher
	wake       chan<- struct{}
	done       chan struct{}
	generation atomic.Uint64
	fallback   atomic.Bool
	mu         sync.Mutex
	paths      map[string]struct{}
	clearCache bool
	scanned    uint64
	interval   time.Duration
	nextRescan time.Time
}

func (c *Client) startWatchers() error {
	interval, err := duration(c.cfg.RescanInterval, defaultRescanInterval)
	if err != nil {
		return err
	}
	if c.cfg.Watch != nil && !*c.cfg.Watch {
		log.Printf("filesystem watching disabled; using periodic scans")
		return nil
	}
	c.wake = make(chan struct{}, 1)
	c.watchers = map[*Folder]*folderWatcher{}
	for _, f := range c.folders {
		w, err := newFolderWatcher(f, c.wake, interval)
		if err != nil {
			log.Printf("[%s] filesystem watcher unavailable: %v; using periodic scans", f.cfg.ID, err)
			continue
		}
		c.watchers[f] = w
		log.Printf("[%s] filesystem watching enabled; safety rescan every %s", f.cfg.ID, interval)
	}
	return nil
}

func newFolderWatcher(f *Folder, wake chan<- struct{}, interval time.Duration) (*folderWatcher, error) {
	native, err := fsnotify.NewBufferedWatcher(256)
	if err != nil {
		return nil, err
	}
	w := &folderWatcher{f: f, native: native, wake: wake, done: make(chan struct{}), interval: interval, paths: map[string]struct{}{}}
	go w.readEvents()
	if err = w.reconcile(); err != nil {
		w.close()
		return nil, err
	}
	return w, nil
}

func (w *folderWatcher) close() {
	_ = w.native.Close()
	<-w.done
}

func (w *folderWatcher) notify(p string) {
	w.mu.Lock()
	if len(w.paths) >= maxEntries || p == "." {
		w.clearCache = true
		clear(w.paths)
	} else if !w.clearCache {
		w.paths[p] = struct{}{}
	}
	w.generation.Add(1)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *folderWatcher) failed(err error) {
	if !w.fallback.Swap(true) {
		log.Printf("[%s] filesystem watcher: %v; using periodic scans", w.f.cfg.ID, err)
	}
	w.notify(".")
}

func (w *folderWatcher) readEvents() {
	defer close(w.done)
	for {
		select {
		case event, ok := <-w.native.Events:
			if !ok {
				return
			}
			p, err := filepath.Rel(w.f.cfg.Path, event.Name)
			if err != nil || p == ".." || strings.HasPrefix(p, ".."+string(os.PathSeparator)) {
				continue
			}
			p = filepath.ToSlash(p)
			if p != "." && (validatePath(p) != nil || w.f.ignored(p)) {
				continue
			}
			if p == "." && event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				w.failed(errors.New("watched root moved or disappeared"))
			} else {
				w.notify(p)
			}
		case err, ok := <-w.native.Errors:
			if !ok {
				return
			}
			w.failed(err) // Includes queue overflow: never trust missed notifications.
		}
	}
}

// fsnotify watches directories non-recursively. Add all existing directories
// before scanning so files moved into a new subtree are covered immediately.
// Symlinks, ignored subtrees, and private state/history are never watched.
func (w *folderWatcher) reconcile() error {
	known := map[string]bool{}
	for _, p := range w.native.WatchList() {
		known[p] = true
	}
	desired := map[string]bool{}
	err := fs.WalkDir(w.f.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != "." && (validatePath(p) != nil || w.f.ignored(p)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		absolute := filepath.Join(w.f.cfg.Path, filepath.FromSlash(p))
		desired[absolute] = true
		if !known[absolute] {
			return w.native.Add(absolute)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for p := range known {
		if !desired[p] {
			if err := w.native.Remove(p); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
				return err
			}
		}
	}
	return nil
}

func (w *folderWatcher) scanIfNeeded() error {
	w.f.mu.Lock()
	needsScan := !w.f.healthy || !time.Now().Before(w.f.nextFullScan)
	w.f.mu.Unlock()
	if !needsScan && !w.fallback.Load() && time.Now().Before(w.nextRescan) && w.scanned == w.generation.Load() {
		return nil
	}
	w.mu.Lock()
	generation := w.generation.Load()
	paths, clearCache := w.paths, w.clearCache
	w.paths, w.clearCache = map[string]struct{}{}, false
	w.mu.Unlock()
	w.f.mu.Lock()
	if clearCache {
		clear(w.f.hashCache)
	} else {
		for cached := range w.f.hashCache {
			for ancestor := cached; ancestor != "."; ancestor = path.Dir(ancestor) {
				if _, changed := paths[ancestor]; changed {
					delete(w.f.hashCache, cached)
					break
				}
			}
		}
	}
	w.f.mu.Unlock()
	if !w.fallback.Load() {
		if err := w.reconcile(); err != nil {
			w.failed(err)
		}
	}
	if err := w.f.Scan(); err != nil {
		return err
	}
	w.scanned = generation
	w.nextRescan = time.Now().Add(w.interval)
	return nil
}
