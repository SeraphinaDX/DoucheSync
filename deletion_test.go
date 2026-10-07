// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historyFixture(t testing.TB) *Folder {
	t.Helper()
	f, err := openFolder(FolderConfig{ID: "history", Path: t.TempDir(), Secret: testSecret, SyncDeletes: true, MaxFileSize: 1 << 20}, "receiver")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func readSavedState(t testing.TB, f *Folder) State {
	t.Helper()
	b, err := f.root.ReadFile(".douchesync/state.json")
	if err != nil {
		t.Fatal(err)
	}
	var s State
	if err = json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func tombstone(e Entry) Entry {
	return Entry{Deleted: true, Clock: joinClock(e.Clock, Clock{"sender": 2})}
}

func TestDeleteHistoryReplayRetainsCopiesAndClocks(t *testing.T) {
	f := historyFixture(t)
	if err := f.root.WriteFile("gone.txt", []byte("retained content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	old := f.snapshot()["gone.txt"]
	r := tombstone(old)
	if err := (&Client{}).applyEntry(context.Background(), nil, Announcement{Device: "sender"}, f, "gone.txt", r); err != nil {
		t.Fatal(err)
	}
	if readSavedState(t, f).Entries["gone.txt"].Deleted {
		t.Fatal("test did not leave a pending update before checkpoint")
	}
	if f.pendingUpdates != 1 {
		t.Fatal("deletion was not durably recorded")
	}
	if err := f.root.WriteFile(".douchesync/transfers/history-orphan", []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	f.Close()
	reopened, err := openFolder(f.cfg, f.device)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := reopened.snapshot()["gone.txt"]
	if !got.Deleted || compareClock(got.Clock, r.Clock) != 0 {
		t.Fatalf("history lost after restart: %+v", got)
	}
	if _, err := reopened.root.Stat("gone.txt"); !os.IsNotExist(err) {
		t.Fatal("deleted file reappeared")
	}
	b, err := reopened.root.ReadFile(archiveName("versions", "gone.txt", old))
	if err != nil || string(b) != "retained content" {
		t.Fatal("recovery copy lost")
	}
	if _, err := reopened.root.Stat(".douchesync/transfers/history-orphan"); !os.IsNotExist(err) {
		t.Fatal("orphan record was not cleaned")
	}
	if reopened.pendingUpdates != 0 {
		t.Fatal("replayed history not checkpointed")
	}
}

func TestHistoryReplaySkipsCheckpointedRecords(t *testing.T) {
	for _, counter := range []uint64{2, 3} {
		t.Run(fmt.Sprint(counter), func(t *testing.T) {
			f := historyFixture(t)
			f.state.Entries["gone.txt"] = Entry{Deleted: true, Clock: Clock{"sender": 1}}
			if err := f.saveLocked(); err != nil {
				t.Fatal(err)
			}
			f.state.Entries["gone.txt"] = Entry{Deleted: true, Clock: Clock{"sender": 2}}
			if err := f.persistEntryLocked("gone.txt", true); err != nil {
				t.Fatal(err)
			}
			name := ".douchesync/updates/" + digest([]byte("gone.txt")) + ".json"
			b, err := f.root.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			// Simulate a crash after checkpoint rename, before removal of records.
			f.state.Entries["gone.txt"] = Entry{Deleted: true, Clock: Clock{"sender": counter}}
			if err = f.saveLocked(); err != nil {
				t.Fatal(err)
			}
			if err = f.root.WriteFile(name, b, 0600); err != nil {
				t.Fatal(err)
			}
			f.Close()
			reopened, err := openFolder(f.cfg, f.device)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if reopened.state.Entries["gone.txt"].Clock["sender"] != counter {
				t.Fatal("old record rolled back the checkpoint")
			}
		})
	}
}

func TestRejectDamagedHistoryUpdates(t *testing.T) {
	for _, kind := range []string{"truncated", "marker", "filename", "clock", "unknown field", "directory", "conflicting clock"} {
		t.Run(kind, func(t *testing.T) {
			f := historyFixture(t)
			record := historyUpdate{Version: 1, Marker: f.state.Marker, Path: "gone.txt", Entry: Entry{Deleted: true, Clock: Clock{"sender": 2}}}
			name := ".douchesync/updates/" + digest([]byte(record.Path)) + ".json"
			switch kind {
			case "marker":
				record.Marker = "wrong"
			case "filename":
				name = ".douchesync/updates/" + strings.Repeat("0", 64) + ".json"
			case "clock":
				record.Entry.Clock = Clock{"sender": 0}
			case "conflicting clock":
				f.state.Entries[record.Path] = Entry{Deleted: true, Clock: Clock{"receiver": 1}}
				if err := f.saveLocked(); err != nil {
					t.Fatal(err)
				}
			}
			b, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "truncated" {
				b = b[:len(b)-1]
			}
			if kind == "unknown field" {
				b = append(b[:len(b)-1], []byte(",\"extra\":true}")...)
			}
			if kind == "directory" {
				err = f.root.Mkdir(name, 0700)
			} else {
				err = f.root.WriteFile(name, b, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			reopened, err := openFolder(f.cfg, f.device)
			if err == nil {
				reopened.Close()
				t.Fatal("damaged pending history accepted")
			}
		})
	}
}

func TestFailedDeleteHistoryWriteKeepsRecoveryCopy(t *testing.T) {
	f := historyFixture(t)
	if err := f.root.WriteFile("gone.txt", []byte("recover me"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	old := f.snapshot()["gone.txt"]
	if err := f.root.Rename(".douchesync/updates", ".douchesync/updates.saved"); err != nil {
		t.Fatal(err)
	}
	if err := f.root.WriteFile(".douchesync/updates", []byte("obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (&Client{}).applyEntry(context.Background(), nil, Announcement{}, f, "gone.txt", tombstone(old)); err == nil {
		t.Fatal("history failure ignored")
	}
	if _, err := f.manifest(); err == nil {
		t.Fatal("unsaved history exposed")
	}
	b, err := f.root.ReadFile(archiveName("versions", "gone.txt", old))
	if err != nil || string(b) != "recover me" {
		t.Fatal("recovery copy lost after failed history write")
	}
	if err = f.root.Remove(".douchesync/updates"); err != nil {
		t.Fatal(err)
	}
	if err = f.root.Rename(".douchesync/updates.saved", ".douchesync/updates"); err != nil {
		t.Fatal(err)
	}
	if err = f.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.manifest(); err != nil {
		t.Fatal("history did not recover")
	}
}

func TestDeletionsBeforeDownloads(t *testing.T) {
	n := newNetwork(t, true)
	n.a.cfg.ParallelTransfers = 1
	put(t, n.dirB, "z-gone.txt", "recovery copy")
	n.poll(t)
	n.cycle(t)
	if err := os.Remove(filepath.Join(n.dirB, "z-gone.txt")); err != nil {
		t.Fatal(err)
	}
	put(t, n.dirB, "a-slow.txt", "download me")
	if err := folder(n.b).Scan(); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/file" {
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		n.b.ServeHTTP(w, r)
	}))
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- n.a.syncPeer(ctx, folder(n.a), Announcement{Device: "beta", URL: s.URL, Fingerprint: digest(s.Certificate().Raw)})
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("download did not start")
	}
	_, statErr := os.Stat(filepath.Join(n.dirA, "z-gone.txt"))
	saved := readSavedState(t, folder(n.a))
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancellation ignored")
	}
	if !os.IsNotExist(statErr) || !saved.Entries["z-gone.txt"].Deleted {
		t.Fatal("slow download delayed deletion or its checkpoint")
	}
	if folder(n.a).pendingUpdates != 0 {
		t.Fatal("deletions not checkpointed before downloads")
	}
}

func TestBulkDeletesCheckpointAndRestart(t *testing.T) {
	f := historyFixture(t)
	count := historyCheckpointInterval + 8
	for i := range count {
		if err := f.root.WriteFile(fmt.Sprintf("file-%03d.txt", i), []byte("retained"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	for i := range count {
		p := fmt.Sprintf("file-%03d.txt", i)
		if err := (&Client{}).applyEntry(context.Background(), nil, Announcement{}, f, p, tombstone(f.state.Entries[p])); err != nil {
			t.Fatal(err)
		}
	}
	if f.pendingUpdates != 8 {
		t.Fatalf("periodic checkpoint failed: %d", f.pendingUpdates)
	}
	saved := readSavedState(t, f)
	if !saved.Entries["file-000.txt"].Deleted || saved.Entries["file-135.txt"].Deleted {
		t.Fatal("unexpected checkpoint boundary")
	}
	f.Close()
	reopened, err := openFolder(f.cfg, f.device)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for i := range count {
		p := fmt.Sprintf("file-%03d.txt", i)
		if !reopened.state.Entries[p].Deleted {
			t.Fatalf("deletion lost: %s", p)
		}
		if _, err := reopened.root.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("file reappeared: %s", p)
		}
	}
}

// This isolates history persistence: 128 tombstones in a 10,000-path history.
// It excludes discovery, scanning and retaining file contents; both modes
// fsync every mutation and produce the same final state.
func BenchmarkDeleteHistory(b *testing.B) {
	for _, incremental := range []bool{false, true} {
		name := "full_snapshot_per_delete"
		if incremental {
			name = "incremental_records"
		}
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				f := historyFixture(b)
				for j := range 10000 {
					f.state.Entries[fmt.Sprintf("file-%05d.txt", j)] = Entry{Deleted: true, Clock: Clock{"sender": 1}}
				}
				if err := f.saveLocked(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				for j := range historyCheckpointInterval {
					p := fmt.Sprintf("file-%05d.txt", j)
					f.state.Entries[p] = Entry{Deleted: true, Clock: Clock{"sender": 2}}
					if err := f.persistEntryLocked(p, incremental); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				f.Close()
				b.StartTimer()
			}
		})
	}
}
