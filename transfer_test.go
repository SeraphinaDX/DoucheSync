// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func transferFixture(t testing.TB, workers, files int, serveFile http.HandlerFunc) (*Client, *Folder, Announcement) {
	t.Helper()
	cfg := Config{Client: ClientConfig{DeviceID: "receiver", IdentityDir: t.TempDir(), Listen: "127.0.0.1:0", ParallelTransfers: workers}, Folders: []FolderConfig{{ID: "files", Path: t.TempDir(), Secret: testSecret, MaxFileSize: 1 << 20}}}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	f := folder(c)
	manifest := map[string]Entry{}
	for i := range files {
		manifest[fmt.Sprintf("file-%02d.txt", i)] = Entry{Hash: digest([]byte("payload")), Size: 7, Mode: 0644, Clock: Clock{"sender": 1}}
	}
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.guard.authorize(r, testSecret) {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.URL.Path == "/v1/manifest" {
			json.NewEncoder(w).Encode(manifest)
			return
		}
		serveFile(w, r)
	}))
	t.Cleanup(s.Close)
	return c, f, Announcement{Device: "sender", URL: s.URL, Fingerprint: digest(s.Certificate().Raw)}
}

func TestParallelTransfersBounded(t *testing.T) {
	for _, workers := range []int{1, 3} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			started := make(chan struct{}, 12)
			gate := make(chan struct{})
			var active, peak atomic.Int32
			c, f, a := transferFixture(t, workers, 12, func(w http.ResponseWriter, r *http.Request) {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				started <- struct{}{}
				select {
				case <-gate:
					io.WriteString(w, "payload")
				case <-r.Context().Done():
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- c.syncPeer(ctx, f, a) }()
			for range workers {
				select {
				case <-started:
				case <-ctx.Done():
					close(gate)
					t.Fatal("configured workers did not overlap")
				}
			}
			close(gate)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if peak.Load() != int32(workers) {
				t.Fatalf("peak %d, want %d", peak.Load(), workers)
			}
			for i := range 12 {
				b, err := os.ReadFile(filepath.Join(f.cfg.Path, fmt.Sprintf("file-%02d.txt", i)))
				if err != nil || string(b) != "payload" {
					t.Fatalf("file %d: %q, %v", i, b, err)
				}
			}
		})
	}
}

func TestParallelCancellationCleansTransfers(t *testing.T) {
	started := make(chan struct{}, 2)
	c, f, a := transferFixture(t, 2, 8, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.syncPeer(ctx, f, a) }()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("workers did not start")
		}
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancellation not reported")
	}
	checkNoTransfers(t, f)
	if len(f.state.Entries) != 0 {
		t.Fatal("canceled files installed")
	}
}

func checkNoTransfers(t *testing.T, f *Folder) {
	t.Helper()
	files, err := os.ReadDir(filepath.Join(f.cfg.Path, ".douchesync", "transfers"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary transfers remain: %v, %v", files, err)
	}
}

func TestParallelFailuresAndLocalEditsStayIsolated(t *testing.T) {
	var destination string
	gate := make(chan struct{})
	started := make(chan struct{}, 3)
	c, f, a := transferFixture(t, 3, 3, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if p == "file-00.txt" {
			if err := os.WriteFile(filepath.Join(destination, p), []byte("local edit"), 0644); err != nil {
				t.Error(err)
			}
		}
		started <- struct{}{}
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		if p == "file-01.txt" {
			io.WriteString(w, "corrupt")
			return
		}
		io.WriteString(w, "payload")
	})
	destination = f.cfg.Path
	if err := os.WriteFile(filepath.Join(destination, "file-00.txt"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.syncPeer(ctx, f, a) }()
	for range 3 {
		select {
		case <-started:
		case <-ctx.Done():
			close(gate)
			t.Fatal("workers did not overlap")
		}
	}
	close(gate)
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "local file changed") || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("missing failures: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(destination, "file-00.txt"))
	if err != nil || string(b) != "local edit" {
		t.Fatal("local edit overwritten")
	}
	if _, err := os.Stat(filepath.Join(destination, "file-01.txt")); !os.IsNotExist(err) {
		t.Fatal("corrupt file installed")
	}
	b, err = os.ReadFile(filepath.Join(destination, "file-02.txt"))
	if err != nil || string(b) != "payload" {
		t.Fatal("independent transfer failed")
	}
	checkNoTransfers(t, f)
}

func TestPeerConnectionsReused(t *testing.T) {
	c, f, a := transferFixture(t, 1, 8, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "payload") })
	var requests, reused atomic.Int32
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		requests.Add(1)
		if info.Reused {
			reused.Add(1)
		}
	}})
	if err := c.syncPeer(ctx, f, a); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 9 || reused.Load() != 8 {
		t.Fatalf("requests=%d reused=%d", requests.Load(), reused.Load())
	}
}

func TestParallelTransfersTOML(t *testing.T) {
	d := t.TempDir()
	name := filepath.Join(d, "config.toml")
	for _, n := range []int{0, 1, 4, 32, -1, 33} {
		s := fmt.Sprintf("[client]\ndevice_id='test'\ndiscovery_url='https://sync.example.com'\ndiscovery_token='%s'\nparallel_transfers=%d\n[[folders]]\nid='files'\npath='%s'\nsecret='%s'\n", testToken, n, filepath.ToSlash(d), testSecret)
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
		if err != nil || cfg.Client.ParallelTransfers != want {
			t.Fatalf("%d: %v, %d", n, err, cfg.Client.ParallelTransfers)
		}
	}
}

// Each request waits 50 ms before serving a seven-byte file. This measures a
// latency-bound batch, including TLS, hash verification, fsync and state writes;
// it is not a bandwidth benchmark or a comparison with another sync product.
func BenchmarkSmallFileBatch(b *testing.B) {
	for _, workers := range []int{1, 4} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				c, f, a := transferFixture(b, workers, 16, func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-time.After(50 * time.Millisecond):
						io.WriteString(w, "payload")
					case <-r.Context().Done():
					}
				})
				b.StartTimer()
				if err := c.syncPeer(context.Background(), f, a); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				c.Close()
				b.StartTimer()
			}
		})
	}
}
