// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cacheFixture(t testing.TB, files, size int) *Folder {
	t.Helper()
	f := historyFixture(t)
	f.cfg.MaxFileSize = int64(size + 1)
	body := []byte(strings.Repeat("x", size))
	for i := range files {
		if err := f.root.WriteFile(fmt.Sprintf("file-%02d.bin", i), body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	return f
}

func requireChangeStamp(t testing.TB, f *Folder, p string) {
	t.Helper()
	if !f.hashCache[p].stamp.known {
		t.Skip("filesystem does not expose a usable change timestamp; full hashing remains enabled")
	}
}

func TestIdleScanReadsNoContentsAndKeepsCheckpoint(t *testing.T) {
	f := cacheFixture(t, 5, 1<<20)
	requireChangeStamp(t, f, "file-00.bin")
	initialBytes := f.hashBytes.Load()
	before, err := f.root.Stat(".douchesync/state.json")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err = f.Scan(); err != nil {
			t.Fatal(err)
		}
		if f.lastScan.Full || f.lastScan.Hashed != 0 || f.lastScan.Reused != 5 {
			t.Fatalf("idle scan did work: %+v", f.lastScan)
		}
	}
	after, err := f.root.Stat(".douchesync/state.json")
	if err != nil {
		t.Fatal(err)
	}
	if f.hashBytes.Load() != initialBytes {
		t.Fatal("idle scans read file contents")
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("idle scan rewrote its checkpoint")
	}
}

func TestIdlePeerCyclesReadNoFileContents(t *testing.T) {
	n := newNetwork(t, true)
	put(t, n.dirA, "alpha.bin", strings.Repeat("a", 1<<20))
	put(t, n.dirB, "beta.bin", strings.Repeat("b", 1<<20))
	n.poll(t)
	n.cycle(t)
	n.cycle(t) // populate cache for each just-received path
	for _, c := range []*Client{n.a, n.b} {
		requireChangeStamp(t, folder(c), "alpha.bin")
	}
	initialA, initialB := folder(n.a).hashBytes.Load(), folder(n.b).hashBytes.Load()
	for range 3 {
		n.cycle(t)
	}
	if folder(n.a).hashBytes.Load() != initialA || folder(n.b).hashBytes.Load() != initialB {
		t.Fatal("idle peer checks rehashed unchanged files")
	}
}

func TestIncrementalScanFindsChangesWithRestoredTimestamp(t *testing.T) {
	f := cacheFixture(t, 3, 4)
	old := f.snapshot()["file-00.bin"]
	if err := f.root.WriteFile("file-00.bin", []byte("edit"), 0644); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(0, old.ModTime)
	if err := f.root.Chtimes("file-00.bin", mt, mt); err != nil {
		t.Fatal(err)
	}
	if err := f.root.Remove("file-01.bin"); err != nil {
		t.Fatal(err)
	}
	if err := f.root.WriteFile("added.bin", []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	got := f.snapshot()
	if got["file-00.bin"].Hash != digest([]byte("edit")) || got["file-00.bin"].Clock[f.device] != old.Clock[f.device]+1 {
		t.Fatal("timestamp-preserving edit missed")
	}
	if !got["file-01.bin"].Deleted || got["added.bin"].Hash != digest([]byte("new")) {
		t.Fatal("addition or deletion missed")
	}
	if _, ok := f.hashCache["file-01.bin"]; ok {
		t.Fatal("deleted file retained in hash cache")
	}
	if f.hashCache["file-02.bin"].stamp.known && (f.lastScan.Hashed != 2 || f.lastScan.Reused != 1) {
		t.Fatalf("unchanged file reread: %+v", f.lastScan)
	}
}

func TestAtomicReplacementCannotReuseOldHash(t *testing.T) {
	f := cacheFixture(t, 1, 8)
	old := f.snapshot()["file-00.bin"]
	if err := f.root.WriteFile("replacement.tmp", []byte("replaced"), 0644); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(0, old.ModTime)
	if err := f.root.Chtimes("replacement.tmp", mt, mt); err != nil {
		t.Fatal(err)
	}
	if err := f.root.Rename("replacement.tmp", "file-00.bin"); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	if f.snapshot()["file-00.bin"].Hash != digest([]byte("replaced")) || f.lastScan.Hashed != 1 {
		t.Fatal("replacement with equal size/time reused stale hash")
	}
}

func TestFullVerificationBypassesMetadataCache(t *testing.T) {
	f := cacheFixture(t, 2, 8)
	requireChangeStamp(t, f, "file-00.bin")
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	initialBytes := f.hashBytes.Load()
	f.nextFullScan = time.Now().Add(-time.Second)
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	if !f.lastScan.Full || f.lastScan.Hashed != 2 || f.lastScan.Reused != 0 || f.hashBytes.Load() != initialBytes+16 {
		t.Fatal("scheduled full verification did not read contents")
	}
	if !f.nextFullScan.After(time.Now()) {
		t.Fatal("full verification was not rescheduled")
	}
}

func TestExpiredHashIsVerifiedOnPeerRefresh(t *testing.T) {
	f := cacheFixture(t, 1, 8)
	requireChangeStamp(t, f, "file-00.bin")
	cached := f.hashCache["file-00.bin"]
	cached.verified = time.Now().Add(-2 * defaultFullScanInterval)
	f.hashCache["file-00.bin"] = cached
	initialBytes := f.hashBytes.Load()
	f.mu.Lock()
	err := f.refreshLocked("file-00.bin")
	f.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if f.hashBytes.Load() != initialBytes+8 {
		t.Fatal("expired cache was trusted")
	}
}

func TestRestartVerifiesFilesBeforeCaching(t *testing.T) {
	f := cacheFixture(t, 2, 8)
	old := f.snapshot()
	f.Close()
	reopened, err := openFolder(f.cfg, f.device)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.hashCache) != 0 {
		t.Fatal("unverified cache loaded at restart")
	}
	if err = reopened.Scan(); err != nil {
		t.Fatal(err)
	}
	if !reopened.lastScan.Full || reopened.lastScan.Hashed != 2 || reopened.hashBytes.Load() != 16 {
		t.Fatal("restart skipped content verification")
	}
	for p, e := range old {
		if compareClock(reopened.state.Entries[p].Clock, e.Clock) != 0 {
			t.Fatal("unchanged restart modified version clocks")
		}
	}
}

func TestFullScanIntervalTOML(t *testing.T) {
	d := t.TempDir()
	name := filepath.Join(d, "config.toml")
	for _, interval := range []string{"", "1s", "1h", "24h", "0s", "25h", "bad"} {
		setting := ""
		if interval != "" {
			setting = "full_scan_interval='" + interval + "'\n"
		}
		s := fmt.Sprintf("[client]\ndevice_id='test'\ndiscovery_url='https://sync.example.com'\ndiscovery_token='%s'\n%s[[folders]]\nid='files'\npath='%s'\nsecret='%s'\n", testToken, setting, filepath.ToSlash(d), testSecret)
		if err := os.WriteFile(name, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := readConfig(name, "client")
		if interval == "0s" || interval == "25h" || interval == "bad" {
			if err == nil {
				t.Fatalf("accepted %q", interval)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := duration(cfg.Client.FullScanInterval, defaultFullScanInterval)
		want := defaultFullScanInterval
		if interval != "" {
			want, _ = time.ParseDuration(interval)
		}
		if err != nil || got != want {
			t.Fatalf("interval %q: %s, %v", interval, got, err)
		}
	}
}

func BenchmarkIdleScan(b *testing.B) {
	for _, full := range []bool{true, false} {
		name := "full"
		if !full {
			name = "incremental"
		}
		b.Run(name, func(b *testing.B) {
			b.StopTimer()
			f := cacheFixture(b, 16, 4<<20)
			requireChangeStamp(b, f, "file-00.bin")
			initialBytes := f.hashBytes.Load()
			b.StartTimer()
			for i := 0; i < b.N; i++ {
				if full {
					f.nextFullScan = time.Time{}
				}
				if err := f.Scan(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(f.hashBytes.Load()-initialBytes)/float64(b.N), "content-B/op")
		})
	}
	b.Run("watched", func(b *testing.B) {
		b.StopTimer()
		f := cacheFixture(b, 16, 4<<20)
		w, err := newFolderWatcher(f, make(chan struct{}, 1), defaultRescanInterval)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(w.close)
		if err = w.scanIfNeeded(); err != nil {
			b.Fatal(err)
		}
		initialBytes := f.hashBytes.Load()
		b.StartTimer()
		for range b.N {
			if err = w.scanIfNeeded(); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(f.hashBytes.Load()-initialBytes)/float64(b.N), "content-B/op")
	})
}
