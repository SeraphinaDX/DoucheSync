// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func snapshotFiles(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, root := range roots {
		if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				info, err := d.Info()
				if err != nil {
					return err
				}
				// Empty files have a known digest. Windows mandatory locks
				// forbid ReadFile even on the client's empty lock files.
				if info.Size() == 0 {
					files[path] = digest(nil)
					return nil
				}
				b, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				files[path] = digest(b)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func TestDiagnoseRegisteredPeersReadOnly(t *testing.T) {
	n := newNetwork(t, false)
	put(t, n.dirB, "not-downloaded.txt", "remote content")
	n.poll(t)
	before := snapshotFiles(t, n.dirA, n.dirB, n.cfgA.Client.IdentityDir, n.cfgB.Client.IdentityDir)
	d := n.server.Config.Handler.(*Discovery)
	d.mu.Lock()
	lease := d.peers[roomID(n.cfgA.Folders[0])+"/alpha"]
	d.mu.Unlock()
	var out bytes.Buffer
	if err := diagnose(context.Background(), n.cfgA, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Own device registered") || !strings.Contains(out.String(), "Peer beta at") || !strings.Contains(out.String(), "OK (pinned TLS") {
		t.Fatal(out.String())
	}
	if strings.Contains(out.String(), testToken) || strings.Contains(out.String(), testSecret) {
		t.Fatal("diagnostic exposed a secret")
	}
	if !reflect.DeepEqual(before, snapshotFiles(t, n.dirA, n.dirB, n.cfgA.Client.IdentityDir, n.cfgB.Client.IdentityDir)) {
		t.Fatal("diagnostic changed folder or identity files")
	}
	d.mu.Lock()
	after := d.peers[roomID(n.cfgA.Folders[0])+"/alpha"]
	d.mu.Unlock()
	if !reflect.DeepEqual(lease, after) {
		t.Fatal("diagnostic refreshed registration")
	}
}

func TestDiagnoseMissingGroupAndBadToken(t *testing.T) {
	n := newNetwork(t, false)
	n.poll(t)
	cfg := n.cfgA
	cfg.Folders = append([]FolderConfig(nil), cfg.Folders...)
	cfg.Folders[0].Secret = "different-folder-secret-0123456789abcdef"
	var out bytes.Buffer
	if err := diagnose(context.Background(), cfg, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0 announcement(s)") || !strings.Contains(out.String(), "No other verified devices") || !strings.Contains(out.String(), roomID(cfg.Folders[0])) {
		t.Fatal(out.String())
	}
	cfg.Client.DiscoveryToken = "wrong-discovery-token-0123456789abcdef"
	out.Reset()
	if err := diagnose(context.Background(), cfg, &out); err == nil || !strings.Contains(out.String(), "HTTP 401") {
		t.Fatalf("expected authentication failure: %v\n%s", err, out.String())
	}
}

func TestDiagnosePinnedPeerMismatch(t *testing.T) {
	n := newNetwork(t, false)
	// A signed announcement points beta at alpha's HTTPS server: discovery
	// succeeds but the exact certificate pin must still reject the endpoint.
	n.b.cfg.AdvertiseURL = n.a.cfg.AdvertiseURL
	n.poll(t)
	var out bytes.Buffer
	if err := diagnose(context.Background(), n.cfgA, &out); err == nil || !strings.Contains(out.String(), "fingerprint mismatch") {
		t.Fatalf("expected pin mismatch: %v\n%s", err, out.String())
	}
}

func TestDiagnoseRejectsInvalidAnnouncement(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/peers" {
			t.Errorf("diagnostic made a mutating/unexpected request: %s %s", r.Method, r.URL.Path)
		}
		jsonResponse(w, []Announcement{{Device: "untrusted", URL: "https://127.0.0.1:1"}})
	}))
	defer s.Close()
	cfg := Config{Client: ClientConfig{DeviceID: "alpha", DiscoveryURL: s.URL, DiscoveryToken: testToken}, Folders: []FolderConfig{{ID: "documents", Secret: testSecret}}}
	var out bytes.Buffer
	if err := diagnose(context.Background(), cfg, &out); err == nil || !strings.Contains(out.String(), "REJECTED") {
		t.Fatalf("expected invalid announcement: %v\n%s", err, out.String())
	}
	if calls != 1 {
		t.Fatalf("expected only a discovery query, got %d requests", calls)
	}
}
