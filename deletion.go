// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// Downloads and deletion preparation use fixed worker pools. Only the short
// file/history commit holds the folder lock; copying and hashing do not.
func runFileJobs(ctx context.Context, paths []string, workers int, apply func(string) error) []error {
	errs := make([]error, len(paths))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(workers, len(paths)) {
		wg.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if err := apply(paths[i]); err != nil {
					errs[i] = fmt.Errorf("%s: %w", paths[i], err)
				}
			}
		})
	}
queue:
	for i := range paths {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break queue
		}
	}
	close(jobs)
	wg.Wait()
	return errs
}

type contextReader struct {
	ctx context.Context
	in  io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.in.Read(b)
}

func unchangedEntry(before Entry, existed bool, after Entry, present bool) bool {
	return existed == present && (!existed || (before.Missing == after.Missing && sameContent(before, after) && compareClock(before.Clock, after.Clock) == 0))
}

// Deletion has its own preparation path. Hash the destination and retain a
// verified private copy concurrently, then revalidate both state and the live
// file before committing. Independent files do not block each other's IO.
func (c *Client) applyDeletion(ctx context.Context, a Announcement, f *Folder, p string, r Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	before, existed := f.state.Entries[p]
	healthy := f.healthy
	full := !existed && len(f.state.Entries) >= maxEntries
	f.mu.Unlock()
	if !healthy {
		return errors.New("folder history unavailable; retry after a successful scan")
	}
	if full {
		return errors.New("folder history exceeds 100000 paths")
	}
	if err := f.checkAncestors(p); err != nil {
		return err
	}
	info, err := f.root.Lstat(filepath.FromSlash(p))
	var live *Entry
	if err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("destination is not a regular file")
		}
		e, _, err := f.hashFileObserved(ctx, p)
		if err != nil {
			return err
		}
		live = &e
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f.mu.Lock()
	current, present := f.state.Entries[p]
	if !f.healthy || !unchangedEntry(before, existed, current, present) {
		f.mu.Unlock()
		return errors.New("local history changed during deletion preparation; deferred to next cycle")
	}
	if _, exists := f.state.Entries[p]; !exists && len(f.state.Entries) >= maxEntries {
		f.mu.Unlock()
		return errors.New("folder history exceeds 100000 paths")
	}
	if err = f.observeLocked(p, live); err == nil {
		err = f.persistObservationLocked(p, before, existed)
	}
	if err != nil {
		f.mu.Unlock()
		return err
	}
	l, exists := f.state.Entries[p]
	if exists && !l.Missing {
		relation := compareClock(l.Clock, r.Clock)
		if relation == 1 || relation == 0 {
			f.mu.Unlock()
			if relation == 0 && !sameContent(l, r) {
				return errors.New("inconsistent equal version clocks")
			}
			return nil
		}
	}
	merged := r
	merged.Clock = cloneClock(r.Clock)
	remoteWins := true
	if exists {
		merged.Clock = joinClock(l.Clock, r.Clock)
		if l.Missing && !sameContent(l, r) {
			if merged.Clock[f.device] >= ^uint64(0)-1 {
				f.mu.Unlock()
				return errors.New("version counter exhausted")
			}
			merged.Clock[f.device]++
		} else if !l.Missing && compareClock(l.Clock, r.Clock) == 2 {
			// A concurrent edit survives a delete. Joining the clocks makes this
			// decision durable and prevents another peer's stale delete from winning.
			chosen := winner(l, r)
			remoteWins = sameContent(chosen, r)
			clock := merged.Clock
			merged = chosen
			merged.Clock = clock
			merged.Conflicted = true
		}
	}
	if len(merged.Clock) > 128 {
		f.mu.Unlock()
		return errors.New("version history exceeds 128 devices")
	}
	if !remoteWins || !exists || l.Deleted || l.Missing {
		if err = ctx.Err(); err == nil {
			f.state.Entries[p] = merged
			err = f.persistEntryLocked(p, true)
		}
		f.mu.Unlock()
		return err
	}
	f.mu.Unlock()
	tmp, source, err := f.prepareDeleteArchive(ctx, p, l)
	if err != nil {
		return err
	}
	defer f.root.Remove(tmp)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return err
	}
	current, present = f.state.Entries[p]
	if !f.healthy || !unchangedEntry(l, exists, current, present) {
		return errors.New("local history changed during deletion preparation; deferred to next cycle")
	}
	if err = f.checkAncestors(p); err != nil {
		return err
	}
	latest, err := f.root.Lstat(filepath.FromSlash(p))
	if err != nil {
		return err
	}
	if !latest.Mode().IsRegular() || !os.SameFile(source, latest) || latest.Size() != source.Size() || !latest.ModTime().Equal(source.ModTime()) || latest.Mode().Perm() != source.Mode().Perm() {
		return errors.New("local file changed during deletion preparation; deferred to next cycle")
	}
	category := "versions"
	if r.Conflicted {
		category = "conflicts"
	}
	dst := archiveName(category, p, l)
	if err = f.root.Rename(tmp, dst); err != nil {
		return err
	}
	meta, _ := json.MarshalIndent(map[string]any{"path": p, "entry": l}, "", "  ")
	if err = f.root.WriteFile(dst+".json", meta, 0600); err != nil {
		return err
	}
	if err = f.root.Remove(filepath.FromSlash(p)); err != nil {
		return err
	}
	f.state.Entries[p] = merged
	if err = f.persistEntryLocked(p, true); err != nil {
		return err
	}
	log.Printf("[%s] deleted %s (from %s; previous copy retained)", f.cfg.ID, p, a.Device)
	return nil
}

func (f *Folder) prepareDeleteArchive(ctx context.Context, p string, e Entry) (string, os.FileInfo, error) {
	in, err := f.root.Open(filepath.FromSlash(p))
	if err != nil {
		return "", nil, err
	}
	defer in.Close()
	tmp := ".douchesync/transfers/archive-" + digest([]byte(p)) + "-" + randomHex(16)
	out, err := f.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", nil, err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			_ = f.root.Remove(tmp)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(contextReader{ctx: ctx, in: in}, e.Size+1))
	if err != nil {
		return "", nil, err
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.Hash {
		return "", nil, errors.New("local file changed while saving version")
	}
	if err = ctx.Err(); err != nil {
		return "", nil, err
	}
	if err = out.Sync(); err != nil {
		return "", nil, err
	}
	if err = out.Close(); err != nil {
		return "", nil, err
	}
	latest, source, err := f.hashFileObserved(ctx, p)
	if err != nil {
		return "", nil, err
	}
	if !sameContent(latest, e) {
		return "", nil, errors.New("local file changed while saving version")
	}
	ok = true
	return tmp, source, nil
}
