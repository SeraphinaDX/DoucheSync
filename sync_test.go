// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "discovery-test-token-0123456789abcdef"
const testSecret = "folder-test-secret-0123456789abcdef01"

type testNetwork struct {
	server     *httptest.Server
	a, b       *Client
	dirA, dirB string
	cfgA, cfgB Config
}

func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	return address
}
func newNetwork(t *testing.T, deletes bool) *testNetwork {
	t.Helper()
	n := &testNetwork{server: httptest.NewServer(NewDiscovery(ServerConfig{Token: testToken, MaxPeers: 100})), dirA: t.TempDir(), dirB: t.TempDir()}
	config := func(id, dir string) Config {
		addr := freeAddress(t)
		return Config{Client: ClientConfig{DeviceID: id, IdentityDir: t.TempDir(), Listen: addr, AdvertiseURL: "https://" + addr, DiscoveryURL: n.server.URL, DiscoveryToken: testToken, AllowHTTPDiscovery: true}, Folders: []FolderConfig{{ID: "documents", Path: dir, Secret: testSecret, SyncDeletes: deletes, MaxFileSize: 10 << 20}}}
	}
	n.cfgA = config("alpha", n.dirA)
	n.cfgB = config("beta", n.dirB)
	var err error
	n.a, err = NewClient(n.cfgA)
	if err != nil {
		t.Fatal(err)
	}
	n.b, err = NewClient(n.cfgB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.a.Close(); n.b.Close(); n.server.Close() })
	return n
}
func (n *testNetwork) poll(t *testing.T) {
	t.Helper()
	for _, c := range []*Client{n.a, n.b, n.a} {
		if err := c.PollDiscovery(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
func (n *testNetwork) cycle(t *testing.T) {
	t.Helper()
	for _, c := range []*Client{n.a, n.b, n.a} {
		if err := c.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
func put(t *testing.T, dir, name, text string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}
func get(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func folder(c *Client) *Folder {
	for _, f := range c.folders {
		return f
	}
	panic("no folder")
}

func TestBidirectionalUpdatesAndRestart(t *testing.T) {
	n := newNetwork(t, true)
	put(t, n.dirA, "nested/space & 日本語.txt", "hello from alpha")
	put(t, n.dirB, "from-beta.txt", "hello from beta")
	put(t, n.dirA, "zero.txt", "")
	n.poll(t)
	n.cycle(t)
	if get(t, n.dirB, "nested/space & 日本語.txt") != "hello from alpha" || get(t, n.dirA, "from-beta.txt") != "hello from beta" || get(t, n.dirB, "zero.txt") != "" {
		t.Fatal("initial files did not converge")
	}
	put(t, n.dirB, "nested/space & 日本語.txt", "edited on beta")
	n.cycle(t)
	if get(t, n.dirA, "nested/space & 日本語.txt") != "edited on beta" {
		t.Fatal("update not synchronized")
	}
	// Restart both clients with their existing history and saved TLS identities.
	n.a.unregister()
	n.b.unregister()
	n.a.Close()
	n.b.Close()
	var err error
	n.a, err = NewClient(n.cfgA)
	if err != nil {
		t.Fatal(err)
	}
	n.b, err = NewClient(n.cfgB)
	if err != nil {
		t.Fatal(err)
	}
	n.poll(t)
	n.cycle(t)
	if get(t, n.dirA, "nested/space & 日本語.txt") != "edited on beta" {
		t.Fatal("restart lost state")
	}
}
func TestConcurrentEditsKeepBothCopies(t *testing.T) {
	n := newNetwork(t, true)
	put(t, n.dirA, "draft.txt", "base")
	n.poll(t)
	n.cycle(t)
	put(t, n.dirA, "draft.txt", "alpha edit")
	put(t, n.dirB, "draft.txt", "beta edit")
	if err := folder(n.a).Scan(); err != nil {
		t.Fatal(err)
	}
	if err := folder(n.b).Scan(); err != nil {
		t.Fatal(err)
	}
	n.cycle(t)
	a, b := get(t, n.dirA, "draft.txt"), get(t, n.dirB, "draft.txt")
	if a != b {
		t.Fatalf("conflict did not converge: %q / %q", a, b)
	}
	loser := "alpha edit"
	if a == loser {
		loser = "beta edit"
	}
	for _, dir := range []string{n.dirA, n.dirB} {
		files, err := os.ReadDir(filepath.Join(dir, ".douchesync", "conflicts"))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".json") && get(t, filepath.Join(dir, ".douchesync", "conflicts"), file.Name()) == loser {
				found = true
			}
		}
		if !found {
			t.Fatalf("losing conflict missing in %s", dir)
		}
	}
	if compareClock(folder(n.a).snapshot()["draft.txt"].Clock, folder(n.b).snapshot()["draft.txt"].Clock) != 0 {
		t.Fatal("clocks did not converge")
	}
}
func TestDeletionAndOfflineEdit(t *testing.T) {
	n := newNetwork(t, true)
	put(t, n.dirA, "gone.txt", "base")
	n.poll(t)
	n.cycle(t)
	if err := os.Remove(filepath.Join(n.dirA, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	n.cycle(t)
	if _, err := os.Stat(filepath.Join(n.dirB, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("deletion did not propagate")
	}
	files, _ := os.ReadDir(filepath.Join(n.dirB, ".douchesync", "versions"))
	if len(files) == 0 {
		t.Fatal("deleted version was not retained")
	}
	put(t, n.dirB, "gone.txt", "recreated")
	n.cycle(t)
	if get(t, n.dirA, "gone.txt") != "recreated" {
		t.Fatal("recreation failed")
	}
	// A delete and an independent edit made before synchronization preserve the edit.
	os.Remove(filepath.Join(n.dirA, "gone.txt"))
	put(t, n.dirB, "gone.txt", "offline edit")
	if err := folder(n.a).Scan(); err != nil {
		t.Fatal(err)
	}
	if err := folder(n.b).Scan(); err != nil {
		t.Fatal(err)
	}
	n.cycle(t)
	if get(t, n.dirA, "gone.txt") != "offline edit" || get(t, n.dirB, "gone.txt") != "offline edit" {
		t.Fatal("delete won over concurrent edit")
	}
}
func TestDefaultRestoresLocalDeletions(t *testing.T) {
	n := newNetwork(t, false)
	put(t, n.dirA, "keep.txt", "keep")
	n.poll(t)
	n.cycle(t)
	os.Remove(filepath.Join(n.dirB, "keep.txt"))
	n.cycle(t)
	if get(t, n.dirB, "keep.txt") != "keep" {
		t.Fatal("default mode should restore a missing file from a peer")
	}
}
func TestDeleteAndRecreateWithoutDeletionSync(t *testing.T) {
	n := newNetwork(t, false)
	put(t, n.dirA, "keep.txt", "original")
	n.poll(t)
	n.cycle(t)
	os.Remove(filepath.Join(n.dirA, "keep.txt"))
	if err := folder(n.a).Scan(); err != nil {
		t.Fatal(err)
	}
	put(t, n.dirA, "keep.txt", "new local contents")
	n.cycle(t)
	if get(t, n.dirB, "keep.txt") != "new local contents" {
		t.Fatal("missing-file history was lost")
	}
}
func TestSimultaneousTransfers(t *testing.T) {
	n := newNetwork(t, true)
	put(t, n.dirA, "alpha.bin", strings.Repeat("a", 1<<20))
	put(t, n.dirB, "beta.bin", strings.Repeat("b", 1<<20))
	if err := folder(n.a).Scan(); err != nil {
		t.Fatal(err)
	}
	if err := folder(n.b).Scan(); err != nil {
		t.Fatal(err)
	}
	n.poll(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, c := range []*Client{n.a, n.b} {
		wg.Add(1)
		go func(c *Client) { defer wg.Done(); errs <- c.Cycle(ctx) }(c)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if get(t, n.dirA, "beta.bin") != strings.Repeat("b", 1<<20) || get(t, n.dirB, "alpha.bin") != strings.Repeat("a", 1<<20) {
		t.Fatal("simultaneous transfer failed")
	}
}
func TestEditDuringDownloadIsDeferred(t *testing.T) {
	n := newNetwork(t, true)
	put(t, n.dirA, "file.txt", "original")
	if err := folder(n.a).Scan(); err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		put(t, n.dirA, "file.txt", "edited during transfer")
		io.WriteString(w, "remote")
	}))
	defer remote.Close()
	a := Announcement{URL: remote.URL, Fingerprint: digest(remote.Certificate().Raw)}
	e := Entry{Hash: digest([]byte("remote")), Size: 6, Mode: 0644, Clock: Clock{"other": 1}}
	if err := n.a.applyEntry(context.Background(), pinnedClient(a, 5*time.Second), a, folder(n.a), "file.txt", e); err == nil {
		t.Fatal("edit during transfer not deferred")
	}
	if get(t, n.dirA, "file.txt") != "edited during transfer" {
		t.Fatal("local edit overwritten")
	}
}
func TestIgnoreDirectoryOnReceive(t *testing.T) {
	n := newNetwork(t, false)
	folder(n.b).cfg.Ignore = []string{"cache", "*.tmp"}
	put(t, n.dirA, "cache/nested/data.txt", "ignore me")
	put(t, n.dirA, "scratch.tmp", "ignore me")
	put(t, n.dirA, "keep.txt", "keep me")
	n.poll(t)
	n.cycle(t)
	for _, p := range []string{"cache/nested/data.txt", "scratch.tmp"} {
		if _, err := os.Stat(filepath.Join(n.dirB, filepath.FromSlash(p))); !os.IsNotExist(err) {
			t.Fatal("ignored file received")
		}
	}
	if get(t, n.dirB, "keep.txt") != "keep me" {
		t.Fatal("normal file ignored")
	}
}
func TestSymlinkCannotAliasPrivateStorage(t *testing.T) {
	n := newNetwork(t, false)
	if err := os.Symlink(".douchesync", filepath.Join(n.dirA, "alias")); err != nil {
		t.Skip(err)
	}
	if err := folder(n.a).refreshLocked("alias/state.json"); err == nil {
		t.Fatal("metadata alias not rejected")
	}
}
func TestDiscoveryOutageUsesCachedPeers(t *testing.T) {
	n := newNetwork(t, false)
	n.poll(t)
	n.server.Close()
	put(t, n.dirA, "outage.txt", "direct transfer")
	if err := n.a.PollDiscovery(context.Background()); err == nil {
		t.Fatal("expected discovery outage")
	}
	n.cycle(t)
	if get(t, n.dirB, "outage.txt") != "direct transfer" {
		t.Fatal("cached peer failed")
	}
}
func TestAuthenticationAndCertificatePin(t *testing.T) {
	n := newNetwork(t, false)
	n.poll(t)
	f := folder(n.b)
	a := Announcement{URL: n.cfgB.Client.AdvertiseURL, Fingerprint: n.b.fingerprint}
	h := pinnedClient(a, time.Second*5)
	uri := "/v1/manifest?room=" + roomID(f.cfg)
	req, _ := http.NewRequest("GET", a.URL+uri, nil)
	resp, err := h.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("unsigned request accepted")
	}
	signRequest(req, f.cfg.Secret)
	resp, err = h.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("signed request rejected")
	}
	resp, err = h.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("replayed request accepted")
	}
	a.Fingerprint = strings.Repeat("0", 64)
	if _, err = pinnedClient(a, time.Second*5).Get(a.URL + uri); err == nil {
		t.Fatal("wrong certificate pin accepted")
	}
	resp, err = http.Get(n.server.URL + "/v1/peers?room=" + roomID(f.cfg))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal("discovery leaked peers without token")
	}
	valid := Announcement{Room: roomID(f.cfg), Device: "alpha", URL: a.URL, Fingerprint: n.b.fingerprint, Expires: time.Now().Add(time.Minute).Unix()}
	valid.Proof = mac(f.cfg.Secret, valid.signingData())
	if !valid.verified(f.cfg) {
		t.Fatal("valid announcement rejected")
	}
	valid.URL = "https://attacker.invalid:1234"
	if valid.verified(f.cfg) {
		t.Fatal("tampered discovery endpoint accepted")
	}
}
func TestTraversalAndSymlinks(t *testing.T) {
	n := newNetwork(t, false)
	n.poll(t)
	f := folder(n.b)
	a := Announcement{URL: n.cfgB.Client.AdvertiseURL, Fingerprint: n.b.fingerprint}
	h := pinnedClient(a, time.Second*5)
	for _, p := range []string{"../outside", "/etc/passwd", ".douchesync/state.json", "nested/../../outside", `C:\Windows\test`, "nested/NUL.txt"} {
		q := url.Values{"room": {roomID(f.cfg)}, "path": {p}, "hash": {strings.Repeat("0", 64)}}
		r, err := peerGET(context.Background(), h, a, f, "/v1/file?"+q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Errorf("path %q was not rejected: %d", p, r.StatusCode)
		}
	}
	outside := t.TempDir()
	put(t, outside, "secret.txt", "secret")
	if err := os.Symlink(outside, filepath.Join(n.dirA, "link")); err != nil {
		t.Skip(err)
	}
	if err := folder(n.a).Scan(); err != nil {
		t.Fatal(err)
	}
	if len(folder(n.a).snapshot()) != 0 {
		t.Fatal("symlink was scanned")
	}
	if _, err := folder(n.a).root.Open("link/secret.txt"); err == nil {
		t.Fatal("os.Root allowed escape")
	}
}
func TestBadDownloadNeverReplacesDestination(t *testing.T) {
	n := newNetwork(t, false)
	put(t, n.dirA, "file.txt", "safe original")
	if err := folder(n.a).Scan(); err != nil {
		t.Fatal(err)
	}
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "tampered") }))
	defer bad.Close()
	a := Announcement{URL: bad.URL, Fingerprint: digest(bad.Certificate().Raw)}
	e := Entry{Hash: digest([]byte("expected")), Size: 8, Mode: 0644, Clock: Clock{"other": 1}}
	if err := n.a.applyEntry(context.Background(), pinnedClient(a, 5*time.Second), a, folder(n.a), "file.txt", e); err == nil {
		t.Fatal("bad hash accepted")
	}
	if get(t, n.dirA, "file.txt") != "safe original" {
		t.Fatal("tampered download replaced local data")
	}
	files, _ := os.ReadDir(filepath.Join(n.dirA, ".douchesync", "transfers"))
	if len(files) != 0 {
		t.Fatal("failed transfer temporary file leaked")
	}
}
func TestMultipleFoldersStayIsolated(t *testing.T) {
	n := newNetwork(t, false)
	secondA, secondB := t.TempDir(), t.TempDir()
	for _, pair := range []struct {
		c   *Client
		dir string
	}{{n.a, secondA}, {n.b, secondB}} {
		cfg := FolderConfig{ID: "photos", Path: pair.dir, Secret: "second-folder-secret-0123456789abcdef", MaxFileSize: 10 << 20}
		f, err := openFolder(cfg, pair.c.cfg.DeviceID)
		if err != nil {
			t.Fatal(err)
		}
		pair.c.folders[roomID(cfg)] = f
	}
	put(t, n.dirA, "same.txt", "documents")
	put(t, secondA, "same.txt", "photos")
	n.poll(t)
	n.cycle(t)
	if get(t, n.dirB, "same.txt") != "documents" || get(t, secondB, "same.txt") != "photos" {
		t.Fatal("folders mixed")
	}
}
func TestFolderLockAndMissingState(t *testing.T) {
	n := newNetwork(t, false)
	if f, err := openFolder(n.cfgA.Folders[0], "alpha"); err == nil {
		f.Close()
		t.Fatal("second process accessed locked folder")
	}
	n.a.Close()
	os.Remove(filepath.Join(n.dirA, ".douchesync", "state.json"))
	if f, err := openFolder(n.cfgA.Folders[0], "alpha"); err == nil {
		f.Close()
		t.Fatal("missing state was silently reset")
	}
}
func TestFailedStateWriteDoesNotServeNewHistory(t *testing.T) {
	n := newNetwork(t, false)
	f := folder(n.a)
	statePath := filepath.Join(n.dirA, ".douchesync", "state.json")
	if err := os.Rename(statePath, statePath+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, n.dirA, "new.txt", "new contents")
	if err := f.Scan(); err == nil {
		t.Fatal("failed state write not detected")
	}
	if _, err := f.manifest(); err == nil {
		t.Fatal("unsaved version history was exposed")
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(statePath+".saved", statePath); err != nil {
		t.Fatal(err)
	}
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manifest(); err != nil {
		t.Fatal("folder did not recover after state storage recovered")
	}
}
func TestUnstableScanNeverDeletesOtherFiles(t *testing.T) {
	n := newNetwork(t, true)
	put(t, n.dirA, "tracked.txt", "keep")
	put(t, n.dirA, "missing.txt", "tracked")
	f := folder(n.a)
	if err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(n.dirA, "missing.txt"))
	// A size-limit failure must stop the scan before the missing-file loop.
	put(t, n.dirA, "too-big.bin", strings.Repeat("x", 1024))
	f.cfg.MaxFileSize = 100
	if err := f.Scan(); err == nil {
		t.Fatal("expected file-size scan error")
	}
	if f.snapshot()["missing.txt"].Deleted {
		t.Fatal("incomplete scan produced a deletion")
	}
}
func TestClockRelations(t *testing.T) {
	for _, tc := range []struct {
		a, b Clock
		want int
	}{{Clock{"a": 1}, Clock{"a": 1}, 0}, {Clock{"a": 2}, Clock{"a": 1}, 1}, {Clock{"a": 1}, Clock{"a": 2}, -1}, {Clock{"a": 2, "b": 1}, Clock{"a": 1, "b": 2}, 2}} {
		if got := compareClock(tc.a, tc.b); got != tc.want {
			t.Fatalf("got %d want %d", got, tc.want)
		}
	}
}
func TestConfigValidation(t *testing.T) {
	d := t.TempDir()
	cfg := filepath.Join(d, "config.toml")
	base := fmt.Sprintf("[client]\ndevice_id = 'desktop'\nlisten = '127.0.0.1:7444'\nadvertise_url = 'https://127.0.0.1:7444'\ndiscovery_url = 'http://127.0.0.1:7443'\ndiscovery_token = '%s'\nallow_http_discovery = true\n[[folders]]\nid = 'docs'\npath = '%s'\nsecret = '%s'\n", testToken, filepath.ToSlash(d), testSecret)
	if err := os.WriteFile(cfg, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(cfg, "client"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{base + "unknown_option = true\n", strings.Replace(base, testSecret, "short", 1), strings.Replace(base, "allow_http_discovery = true", "allow_http_discovery = false", 1), strings.Replace(base, "path = '"+filepath.ToSlash(d)+"'", "path = '/no/such/folder'", 1)} {
		os.WriteFile(cfg, []byte(text), 0600)
		if _, err := readConfig(cfg, "client"); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
func TestDiscoveryDoesNotExposeFiles(t *testing.T) {
	n := newNetwork(t, false)
	put(t, n.dirA, "private-name.txt", "private bytes")
	n.poll(t)
	n.cycle(t)
	req, _ := http.NewRequest("GET", n.server.URL+"/v1/peers?room="+roomID(n.cfgA.Folders[0]), nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "private") || strings.Contains(string(body), n.dirA) {
		t.Fatal("discovery response includes file data")
	}
	var peers []map[string]json.RawMessage
	if err = json.Unmarshal(body, &peers); err != nil {
		t.Fatal(err)
	}
	for _, p := range peers {
		if len(p) != 6 {
			t.Fatal("unexpected discovery metadata")
		}
	}
	req, _ = http.NewRequest("GET", n.server.URL+"/v1/file", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal("server exposes file route")
	}
}
