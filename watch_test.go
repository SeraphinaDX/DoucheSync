// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func watchFixture(t *testing.T) (*Folder, *folderWatcher) {
	t.Helper()
	f := cacheFixture(t, 2, 1024)
	f.cfg.Ignore = []string{"ignored"}
	if err := f.root.Mkdir("ignored", 0700); err != nil {
		t.Fatal(err)
	}
	w, err := newFolderWatcher(f, make(chan struct{}, 1), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.close)
	if err = w.scanIfNeeded(); err != nil {
		t.Fatal(err)
	}
	return f, w
}

func waitWatch(t *testing.T, w *folderWatcher, before uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for w.generation.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("filesystem event was not received")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcherSkipsIdleScanAndPrivateHistory(t *testing.T) {
	f, w := watchFixture(t)
	before := w.generation.Load()
	// Unwatched private writes must not repeatedly wake the client. Removing
	// the marker also proves an idle peer poll never enters Folder.Scan.
	marker, err := f.root.ReadFile(".douchesync/identity")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.root.Remove(".douchesync/identity"); err != nil {
		t.Fatal(err)
	}
	if err = f.root.WriteFile("ignored/noise", []byte("noise"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if w.generation.Load() != before {
		t.Fatal("private/ignored writes woke the watcher")
	}
	if err = w.scanIfNeeded(); err != nil {
		t.Fatalf("idle poll performed a directory scan: %v", err)
	}
	w.nextRescan = time.Time{}
	if err = w.scanIfNeeded(); err == nil {
		t.Fatal("safety rescan did not check folder identity")
	}
	if err = f.root.WriteFile(".douchesync/identity", marker, 0600); err != nil {
		t.Fatal(err)
	}
	if err = w.scanIfNeeded(); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherNewSubtreesReplacementAndDeletion(t *testing.T) {
	f, w := watchFixture(t)
	before := w.generation.Load()
	if err := f.root.MkdirAll("new/deep", 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.root.WriteFile("new/deep/file", []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, w, before)
	if err := w.scanIfNeeded(); err != nil {
		t.Fatal(err)
	}
	old := f.snapshot()["new/deep/file"]
	if old.Hash == "" {
		t.Fatal("new subtree contents were missed")
	}
	before = w.generation.Load()
	if err := f.root.WriteFile(".douchesync/transfers/replacement", []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.root.Rename(".douchesync/transfers/replacement", "new/deep/file"); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, w, before)
	if err := w.scanIfNeeded(); err != nil {
		t.Fatal(err)
	}
	if f.snapshot()["new/deep/file"].Hash == old.Hash {
		t.Fatal("atomic replacement was missed")
	}
	before = w.generation.Load()
	if err := f.root.Remove("new/deep/file"); err != nil {
		t.Fatal(err)
	}
	waitWatch(t, w, before)
	if err := w.scanIfNeeded(); err != nil {
		t.Fatal(err)
	}
	if !f.snapshot()["new/deep/file"].Deleted {
		t.Fatal("delete event did not produce a tombstone")
	}
}

func TestWatcherSafetyRescanCatchesUnnotifiedWrite(t *testing.T) {
	f, w := watchFixture(t)
	// Remove just this directory watch to model a filesystem that doesn't
	// notify remote writes (for example a network mount).
	if err := w.native.Remove(filepath.Clean(f.cfg.Path)); err != nil {
		t.Fatal(err)
	}
	if err := f.root.WriteFile("unnotified", []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	w.nextRescan = time.Time{}
	if err := w.scanIfNeeded(); err != nil {
		t.Fatal(err)
	}
	if f.snapshot()["unnotified"].Hash == "" {
		t.Fatal("safety scan missed the unnotified write")
	}
}

func TestWatcherOverflowFallsBackToPolling(t *testing.T) {
	f, w := watchFixture(t)
	w.failed(fsnotify.ErrEventOverflow)
	if !w.fallback.Load() {
		t.Fatal("overflow did not enable polling")
	}
	if err := w.scanIfNeeded(); err != nil {
		t.Fatal(err)
	}
	// Polling must remain active even without another notification.
	if err := w.native.Remove(filepath.Clean(f.cfg.Path)); err != nil && !errors.Is(err, fsnotify.ErrNonExistentWatch) {
		t.Fatal(err)
	}
	if err := f.root.WriteFile("fallback", []byte("found"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.scanIfNeeded(); err != nil {
		t.Fatal(err)
	}
	if f.snapshot()["fallback"].Hash == "" {
		t.Fatal("fallback polling skipped the change")
	}
}

func TestWatchConfigDefaultsDisableAndRescanValidation(t *testing.T) {
	for _, settings := range []string{"", "watch = false\nrescan_interval = \"1s\"\n"} {
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.toml")
		body := "[client]\ndevice_id = \"config-test\"\nlisten = \"127.0.0.1:0\"\ndiscovery_url = \"https://discovery.example\"\ndiscovery_token = \"" + testToken + "\"\n" + settings + "[[folders]]\nid = \"test\"\npath = \"" + filepath.ToSlash(dir) + "\"\nsecret = \"" + testSecret + "\"\n"
		if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := readConfig(cfgPath, "client")
		if err != nil {
			t.Fatal(err)
		}
		if settings == "" && cfg.Client.Watch != nil {
			t.Fatal("omitted watch must use the enabled default")
		}
		if settings != "" && (cfg.Client.Watch == nil || *cfg.Client.Watch) {
			t.Fatal("watch=false ignored")
		}
		if settings == "" {
			bad := strings.Replace(body, "[client]\n", "[client]\nrescan_interval = \"0s\"\n", 1)
			if err = os.WriteFile(cfgPath, []byte(bad), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = readConfig(cfgPath, "client"); err == nil {
				t.Fatal("invalid rescan interval accepted")
			}
		}
	}
	if _, err := duration("0s", defaultRescanInterval); err == nil {
		t.Fatal("invalid rescan accepted")
	}
}
