// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func parallelDeleteFixture(t testing.TB, workers, files, size int) (*Client, *Folder, Announcement) {
	t.Helper()
	f := historyFixture(t)
	f.cfg.MaxFileSize = int64(size + 1)
	payload := []byte(strings.Repeat("x", size))
	for i := range files {
		if err := f.root.WriteFile(fmt.Sprintf("file-%02d.bin", i), payload, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	remote := f.snapshot()
	for p, e := range remote {
		remote[p] = tombstone(e)
	}
	c := &Client{cfg: ClientConfig{ParallelDeletes: workers}, transferTimeout: 10 * time.Second}
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.guard.authorize(r, f.cfg.Secret) {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.URL.Path != "/v1/manifest" {
			http.Error(w, "unexpected file download", 500)
			return
		}
		json.NewEncoder(w).Encode(remote)
	}))
	t.Cleanup(s.Close)
	return c, f, Announcement{Device: "sender", URL: s.URL, Fingerprint: digest(s.Certificate().Raw)}
}

func archivePreparations(t *testing.T, f *Folder) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.cfg.Path, ".douchesync", "transfers"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "archive-") {
			n++
		}
	}
	return n
}

func TestParallelDeletionPreparationsOverlapAndStayBounded(t *testing.T) {
	for _, workers := range []int{1, 3} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			c, f, a := parallelDeleteFixture(t, workers, 9, 8<<20)
			originals := f.snapshot()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- c.syncPeer(ctx, f, a) }()
			peak := 0
			tick := time.NewTicker(100 * time.Microsecond)
			defer tick.Stop()
		monitor:
			for {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
					break monitor
				case <-tick.C:
					n := archivePreparations(t, f)
					peak = max(peak, n)
				case <-ctx.Done():
					t.Fatal("deletion batch timed out")
				}
			}
			if peak > workers || peak == 0 || (workers > 1 && peak < 2) {
				t.Fatalf("active recovery preparations: peak %d, limit %d", peak, workers)
			}
			for p, e := range originals {
				if _, err := f.root.Stat(p); !os.IsNotExist(err) {
					t.Fatalf("file not deleted: %s", p)
				}
				in, err := f.root.Open(archiveName("versions", p, e))
				if err != nil {
					t.Fatal(err)
				}
				info, err := in.Stat()
				in.Close()
				if err != nil || info.Size() != e.Size {
					t.Fatalf("recovery copy missing: %s", p)
				}
				if !f.state.Entries[p].Deleted || !readSavedState(t, f).Entries[p].Deleted {
					t.Fatalf("deletion not checkpointed: %s", p)
				}
			}
			checkNoTransfers(t, f)
		})
	}
}

// Observe a real archive while its copy is still near the beginning, rather
// than substituting a mock for file IO or adding a test hook to production.
func waitForArchiveCopy(t *testing.T, f *Folder, p string, size int, done <-chan error) {
	t.Helper()
	prefix := "archive-" + digest([]byte(p)) + "-"
	limit := time.NewTimer(10 * time.Second)
	defer limit.Stop()
	tick := time.NewTicker(100 * time.Microsecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("batch ended before copy observation: %v", err)
		case <-limit.C:
			t.Fatal("archive copy was not observed")
		case <-tick.C:
			entries, err := os.ReadDir(filepath.Join(f.cfg.Path, ".douchesync", "transfers"))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !strings.HasPrefix(entry.Name(), prefix) {
					continue
				}
				info, err := entry.Info()
				if err == nil && info.Size() < int64(size/4) {
					return
				}
			}
		}
	}
}

func TestDeletionCopyDoesNotHoldFolderLock(t *testing.T) {
	c, f, a := parallelDeleteFixture(t, 1, 1, 32<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.syncPeer(ctx, f, a) }()
	waitForArchiveCopy(t, f, "file-00.bin", 32<<20, done)
	if !f.mu.TryLock() {
		cancel()
		<-done
		t.Fatal("recovery copy holds the folder lock")
	}
	f.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEditDuringParallelDeletionIsPreserved(t *testing.T) {
	c, f, a := parallelDeleteFixture(t, 3, 4, 32<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.syncPeer(ctx, f, a) }()
	waitForArchiveCopy(t, f, "file-00.bin", 32<<20, done)
	if err := f.root.WriteFile("file-00.bin", []byte("edited during deletion"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("changed file was not deferred")
	}
	b, err := f.root.ReadFile("file-00.bin")
	if err != nil || string(b) != "edited during deletion" {
		t.Fatal("concurrent local edit deleted")
	}
	for i := 1; i < 4; i++ {
		if _, err := f.root.Stat(fmt.Sprintf("file-%02d.bin", i)); !os.IsNotExist(err) {
			t.Fatal("independent deletion blocked by local edit")
		}
	}
	checkNoTransfers(t, f)
}

func TestParallelDeletionCancellationCleansArchives(t *testing.T) {
	c, f, a := parallelDeleteFixture(t, 3, 6, 32<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.syncPeer(ctx, f, a) }()
	waitForArchiveCopy(t, f, "file-00.bin", 32<<20, done)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not reported: %v", err)
	}
	checkNoTransfers(t, f)
	for p, e := range f.snapshot() {
		if e.Deleted {
			continue
		}
		info, err := f.root.Stat(p)
		if err != nil || info.Size() != 32<<20 {
			t.Fatalf("canceled deletion damaged %s", p)
		}
	}
}

func TestParallelDeletesTOML(t *testing.T) {
	d := t.TempDir()
	name := filepath.Join(d, "config.toml")
	for _, n := range []int{0, 1, 4, 32, -1, 33} {
		s := fmt.Sprintf("[client]\ndevice_id='test'\ndiscovery_url='https://sync.example.com'\ndiscovery_token='%s'\nparallel_deletes=%d\n[[folders]]\nid='files'\npath='%s'\nsecret='%s'\n", testToken, n, filepath.ToSlash(d), testSecret)
		if err := os.WriteFile(name, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := readConfig(name, "client")
		if n < 0 || n > 32 {
			if err == nil {
				t.Fatalf("accepted %d", n)
			}
			continue
		}
		want := n
		if want == 0 {
			want = 4
		}
		if err != nil || cfg.Client.ParallelDeletes != want {
			t.Fatalf("%d: %v, %d", n, err, cfg.Client.ParallelDeletes)
		}
	}
}

// Actual deletion phase: TLS manifest, full content checks, retained copies,
// fsync, removal, compact history records and final checkpoint. Setup and the
// initial scan are excluded. This is a local disk workload, not a WAN test.
func BenchmarkParallelDeleteBatch(b *testing.B) {
	for _, workers := range []int{1, 4} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				c, f, a := parallelDeleteFixture(b, workers, 16, 4<<20)
				b.StartTimer()
				if err := c.syncPeer(context.Background(), f, a); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				f.Close()
				b.StartTimer()
			}
		})
	}
}
