// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type Client struct {
	cfg             ClientConfig
	folders         map[string]*Folder // keyed by opaque room identifier
	fingerprint     string
	server          *http.Server
	listener        net.Listener
	http            *http.Client
	guard           replayGuard
	mu              sync.Mutex
	peers           map[string][]Announcement
	transferTimeout time.Duration
}

func NewClient(cfg Config) (*Client, error) {
	cert, fp, err := peerCertificate()
	if err != nil {
		return nil, err
	}
	timeout, _ := duration(cfg.Client.TransferTimeout, 30*time.Minute)
	c := &Client{cfg: cfg.Client, folders: map[string]*Folder{}, fingerprint: fp, peers: map[string][]Announcement{}, transferTimeout: timeout}
	c.http = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("discovery redirects are forbidden") }}
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	for _, fc := range cfg.Folders {
		f, e := openFolder(fc, c.cfg.DeviceID)
		if e != nil {
			return nil, fmt.Errorf("folder %s: %w", fc.ID, e)
		}
		c.folders[roomID(fc)] = f
	}
	c.server = &http.Server{Handler: c, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}}
	c.listener, err = net.Listen("tcp", c.cfg.Listen)
	if err != nil {
		return nil, err
	}
	go func() {
		err := c.server.Serve(tls.NewListener(c.listener, c.server.TLSConfig))
		if err != nil && err != http.ErrServerClosed {
			log.Printf("peer server: %v", err)
		}
	}()
	log.Printf("device %s listening on %s; advertised as %s", c.cfg.DeviceID, c.listener.Addr(), c.cfg.AdvertiseURL)
	ok = true
	return c, nil
}
func (c *Client) Close() {
	if c.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.server.Shutdown(ctx)
		cancel()
		_ = c.server.Close()
	}
	if c.listener != nil {
		c.listener.Close()
	}
	for _, f := range c.folders {
		f.Close()
	}
}
func (c *Client) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	f, ok := c.folders[r.URL.Query().Get("room")]
	if !ok || !c.guard.authorize(r, f.cfg.Secret) {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch r.URL.Path {
	case "/v1/manifest":
		m, err := f.manifest()
		if err != nil {
			http.Error(w, "folder state unavailable", 503)
			return
		}
		jsonResponse(w, m)
	case "/v1/file":
		p := r.URL.Query().Get("path")
		if validatePath(p) != nil || f.ignored(p) {
			http.Error(w, "invalid path", 400)
			return
		}
		f.mu.Lock()
		entry, exists := f.state.Entries[p]
		healthy := f.healthy
		f.mu.Unlock()
		if !healthy {
			http.Error(w, "folder state unavailable", 503)
			return
		}
		if !exists || entry.Deleted || entry.Missing {
			http.NotFound(w, r)
			return
		}
		if entry.Hash != r.URL.Query().Get("hash") {
			http.Error(w, "version changed; retry next cycle", 409)
			return
		}
		if err := f.checkAncestors(p); err != nil {
			http.Error(w, "invalid path", 400)
			return
		}
		in, err := f.root.Open(filepath.FromSlash(p))
		if err != nil {
			http.Error(w, "file unavailable", 409)
			return
		}
		defer in.Close()
		info, err := in.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
			http.Error(w, "file changed", 409)
			return
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(c.transferTimeout))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
		// Receiving peers verify the complete stream against the requested hash.
		_, _ = io.CopyN(w, in, entry.Size)
	default:
		http.NotFound(w, r)
	}
}
func (c *Client) discoveryRequest(ctx context.Context, method, uri string, body io.Reader) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.cfg.DiscoveryURL+uri, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.cfg.DiscoveryToken)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(r)
}
func decodeLimited(resp *http.Response, limit int64, v any) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return errors.New("response exceeds size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return errors.New("trailing response data")
	}
	return nil
}

// PollDiscovery updates one room at a time. Successfully verified addresses
// remain cached during a discovery outage; the server is never a data relay.
func (c *Client) PollDiscovery(ctx context.Context) error {
	var errs []error
	for room, f := range c.folders {
		a := Announcement{Room: room, Device: c.cfg.DeviceID, URL: c.cfg.AdvertiseURL, Fingerprint: c.fingerprint, Expires: time.Now().Add(120 * time.Second).Unix()}
		a.Proof = mac(f.cfg.Secret, a.signingData())
		b, _ := json.Marshal(a)
		r, err := c.discoveryRequest(ctx, http.MethodPost, "/v1/peers", bytes.NewReader(b))
		if err == nil {
			err = requireStatus(r, 204)
			r.Body.Close()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("register %s: %w", f.cfg.ID, err))
			continue
		}
		r, err = c.discoveryRequest(ctx, http.MethodGet, "/v1/peers?room="+room, nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var peers []Announcement
		err = requireStatus(r, 200)
		if err == nil {
			err = decodeLimited(r, 8<<20, &peers)
		}
		r.Body.Close()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		accepted := []Announcement{}
		seen := map[string]bool{}
		for _, p := range peers {
			if p.Device == c.cfg.DeviceID {
				continue
			}
			if !p.verified(f.cfg) || seen[p.Device] {
				errs = append(errs, fmt.Errorf("rejected invalid/duplicate announcement in folder %s", f.cfg.ID))
				continue
			}
			seen[p.Device] = true
			accepted = append(accepted, p)
		}
		c.mu.Lock()
		c.peers[room] = accepted
		c.mu.Unlock()
	}
	return errors.Join(errs...)
}
func (c *Client) unregister() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for room := range c.folders {
		q := url.Values{"room": {room}, "device": {c.cfg.DeviceID}, "fingerprint": {c.fingerprint}}
		if r, err := c.discoveryRequest(ctx, http.MethodDelete, "/v1/peers?"+q.Encode(), nil); err == nil {
			r.Body.Close()
		}
	}
}
func peerGET(ctx context.Context, h *http.Client, a Announcement, f *Folder, uri string) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL+uri, nil)
	if err != nil {
		return nil, err
	}
	signRequest(r, f.cfg.Secret)
	return h.Do(r)
}
func (c *Client) Cycle(ctx context.Context) error {
	var errs []error
	for room, f := range c.folders {
		if err := f.Scan(); err != nil {
			errs = append(errs, fmt.Errorf("scan %s: %w", f.cfg.ID, err))
			continue
		}
		c.mu.Lock()
		peers := append([]Announcement(nil), c.peers[room]...)
		c.mu.Unlock()
		if len(peers) == 0 {
			log.Printf("[%s] no reachable peers discovered yet", f.cfg.ID)
		}
		for _, a := range peers {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := c.syncPeer(ctx, f, a); err != nil {
				errs = append(errs, fmt.Errorf("folder %s, peer %s: %w", f.cfg.ID, a.Device, err))
			}
		}
	}
	return errors.Join(errs...)
}
func (c *Client) syncPeer(ctx context.Context, f *Folder, a Announcement) error {
	h := pinnedClient(a, c.transferTimeout)
	resp, err := peerGET(ctx, h, a, f, "/v1/manifest?room="+roomID(f.cfg))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err = requireStatus(resp, 200); err != nil {
		return err
	}
	var remote map[string]Entry
	if err = decodeLimited(resp, maxManifestBytes, &remote); err != nil {
		return err
	}
	if remote == nil || len(remote) > maxEntries {
		return errors.New("invalid manifest")
	}
	for p, e := range remote {
		if validatePath(p) != nil {
			return errors.New("peer sent invalid path")
		}
		if err = validateEntry(e, f.cfg.MaxFileSize); err != nil {
			return fmt.Errorf("manifest entry %s: %w", p, err)
		}
		if e.Missing {
			return errors.New("peer sent a local-only missing record")
		}
	}
	var errs []error
	for _, p := range sortedPaths(remote) {
		if f.ignored(p) {
			continue
		}
		r := remote[p]
		if r.Deleted && !f.cfg.SyncDeletes {
			continue
		}
		if err = c.applyEntry(ctx, h, a, f, p, r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}
func downloadEntry(ctx context.Context, h *http.Client, a Announcement, f *Folder, p string, e Entry) (string, error) {
	q := url.Values{"room": {roomID(f.cfg)}, "path": {p}, "hash": {e.Hash}}
	resp, err := peerGET(ctx, h, a, f, "/v1/file?"+q.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err = requireStatus(resp, 200); err != nil {
		return "", err
	}
	name := ".douchesync/transfers/" + randomHex(16)
	out, err := f.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			_ = f.root.Remove(name)
		}
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(resp.Body, e.Size+1))
	if err != nil {
		return "", err
	}
	if n != e.Size || hex.EncodeToString(hash.Sum(nil)) != e.Hash {
		return "", errors.New("download size/hash mismatch; retrying next cycle")
	}
	if err = out.Sync(); err != nil {
		return "", err
	}
	if err = out.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}
func (c *Client) applyEntry(ctx context.Context, h *http.Client, a Announcement, f *Folder, p string, r Entry) error {
	// Refresh the destination before deciding, and again after the network
	// transfer. A change made while downloading is never knowingly overwritten.
	f.mu.Lock()
	if _, exists := f.state.Entries[p]; !exists && len(f.state.Entries) >= maxEntries {
		f.mu.Unlock()
		return errors.New("folder history exceeds 100000 paths")
	}
	if err := f.refreshLocked(p); err != nil {
		f.mu.Unlock()
		return err
	}
	l, exists := f.state.Entries[p]
	if exists && !l.Missing {
		relation := compareClock(l.Clock, r.Clock)
		if relation == 1 {
			f.mu.Unlock()
			return nil
		}
		if relation == 0 {
			f.mu.Unlock()
			if !sameContent(l, r) {
				return errors.New("inconsistent equal version clocks")
			}
			return nil
		}
		if sameContent(l, r) {
			l.Clock = joinClock(l.Clock, r.Clock)
			if len(l.Clock) > 128 {
				f.mu.Unlock()
				return errors.New("version history exceeds 128 devices")
			}
			l.Conflicted = l.Conflicted || r.Conflicted
			f.state.Entries[p] = l
			err := f.saveLocked()
			f.mu.Unlock()
			return err
		}
	}
	f.mu.Unlock()
	tmp := ""
	var err error
	if !r.Deleted {
		tmp, err = downloadEntry(ctx, h, a, f, p, r)
		if err != nil {
			return err
		}
		defer f.root.Remove(tmp)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err = f.refreshLocked(p); err != nil {
		return err
	}
	current, nowExists := f.state.Entries[p]
	if exists != nowExists || (exists && (compareClock(current.Clock, l.Clock) != 0 || !sameContent(current, l))) {
		return errors.New("local file changed during transfer; deferred to next cycle")
	}
	merged := r
	merged.Clock = cloneClock(r.Clock)
	remoteWins := true
	concurrent := false
	if exists && l.Missing {
		merged.Clock = joinClock(l.Clock, r.Clock)
		if !sameContent(l, r) {
			if merged.Clock[f.device] >= ^uint64(0)-1 {
				return errors.New("version counter exhausted")
			}
			merged.Clock[f.device]++
		}
	}
	if exists && !l.Missing {
		concurrent = compareClock(l.Clock, r.Clock) == 2
		merged.Clock = joinClock(l.Clock, r.Clock)
		if concurrent {
			chosen := winner(l, r)
			remoteWins = sameContent(chosen, r)
			clock := merged.Clock
			merged = chosen
			merged.Clock = clock
			merged.Conflicted = true
		}
	}
	if len(merged.Clock) > 128 {
		return errors.New("version history exceeds 128 devices")
	}
	if !remoteWins {
		if tmp != "" {
			dst := archiveName("conflicts", p, r)
			if err = f.root.Rename(tmp, dst); err != nil {
				return err
			}
			meta, _ := json.MarshalIndent(map[string]any{"path": p, "entry": r}, "", "  ")
			if err = f.root.WriteFile(dst+".json", meta, 0600); err != nil {
				return err
			}
			log.Printf("[%s] conflict saved: %s (peer %s)", f.cfg.ID, p, a.Device)
		}
	} else {
		if exists && !l.Deleted && !l.Missing {
			category := "versions"
			if concurrent || r.Conflicted {
				category = "conflicts"
			}
			if err = f.archiveLocal(p, l, category); err != nil {
				return err
			}
		}
		if r.Deleted {
			if exists && !l.Deleted && !l.Missing {
				if err = f.root.Remove(filepath.FromSlash(p)); err != nil {
					return err
				}
			}
			log.Printf("[%s] deleted %s (from %s; previous copy retained)", f.cfg.ID, p, a.Device)
		} else {
			if err = f.root.MkdirAll(filepath.FromSlash(filepath.Dir(p)), 0755); err != nil {
				return err
			}
			if err = f.root.Chmod(tmp, os.FileMode(r.Mode)&0777); err != nil {
				return err
			}
			mt := time.Unix(0, r.ModTime)
			if err = f.root.Chtimes(tmp, mt, mt); err != nil {
				return err
			}
			if err = f.root.Rename(tmp, filepath.FromSlash(p)); err != nil {
				return err
			}
			log.Printf("[%s] received %s (%d bytes) from %s", f.cfg.ID, p, r.Size, a.Device)
		}
	}
	f.state.Entries[p] = merged
	return f.saveLocked()
}
func runClient(ctx context.Context, cfg Config, once bool) error {
	c, err := NewClient(cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	defer c.unregister()
	if err = c.PollDiscovery(ctx); err != nil {
		if once {
			return err
		}
		log.Printf("discovery: %v", err)
	}
	if once {
		return c.Cycle(ctx)
	}
	// Discovery heartbeats run independently of expensive scans/transfers.
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := c.PollDiscovery(ctx); err != nil && ctx.Err() == nil {
					log.Printf("discovery: %v", err)
				}
			}
		}
	}()
	interval, _ := duration(cfg.Client.ScanInterval, 10*time.Second)
	for {
		if err = c.Cycle(ctx); err != nil && ctx.Err() == nil {
			log.Printf("sync: %v", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			<-done
			return nil
		case <-timer.C:
		}
	}
}
