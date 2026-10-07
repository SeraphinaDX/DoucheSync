// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

type peerLease struct {
	a    Announcement
	seen time.Time
}
type Discovery struct {
	cfg   ServerConfig
	ttl   time.Duration
	mu    sync.Mutex
	peers map[string]peerLease
}

func NewDiscovery(cfg ServerConfig) *Discovery {
	ttl, _ := duration(cfg.PeerTTL, 90*time.Second)
	return &Discovery{cfg: cfg, ttl: ttl, peers: map[string]peerLease{}}
}
func (d *Discovery) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-DoucheSync-Version", version)
	if !hmac.Equal([]byte(r.Header.Get("Authorization")), []byte("Bearer "+d.cfg.Token)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/v1/peers" {
		http.NotFound(w, r)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, p := range d.peers {
		if now.Sub(p.seen) > d.ttl || p.a.Expires <= now.Unix() {
			delete(d.peers, k)
		}
	}
	switch r.Method {
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		var a Announcement
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			http.Error(w, "invalid announcement", 400)
			return
		}
		var trailing any
		if dec.Decode(&trailing) != io.EOF {
			http.Error(w, "trailing data", 400)
			return
		}
		_, he := hex.DecodeString(a.Fingerprint)
		_, re := hex.DecodeString(a.Room)
		_, pe := hex.DecodeString(a.Proof)
		if !validID.MatchString(a.Device) || !a.validEndpoints() || len(a.Room) != 64 || len(a.Fingerprint) != 64 || len(a.Proof) != 64 || he != nil || re != nil || pe != nil || a.Expires <= now.Unix() || a.Expires > now.Add(3*time.Minute).Unix() {
			http.Error(w, "invalid announcement", 400)
			return
		}
		k := a.Room + "/" + a.Device
		if old, ok := d.peers[k]; ok && old.a.Fingerprint != a.Fingerprint {
			http.Error(w, "device ID is already online; choose unique device IDs (or wait for its lease to expire)", 409)
			return
		}
		if _, ok := d.peers[k]; !ok && len(d.peers) >= d.cfg.MaxPeers {
			http.Error(w, "peer capacity reached", 503)
			return
		}
		d.peers[k] = peerLease{a: a, seen: now}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		room := r.URL.Query().Get("room")
		if len(room) != 64 {
			http.Error(w, "invalid room", 400)
			return
		}
		out := []Announcement{}
		for _, p := range d.peers {
			if p.a.Room == room {
				out = append(out, p.a)
			}
		}
		jsonResponse(w, out)
	case http.MethodDelete:
		k := r.URL.Query().Get("room") + "/" + r.URL.Query().Get("device")
		if p, ok := d.peers[k]; ok && hmac.Equal([]byte(p.a.Fingerprint), []byte(r.URL.Query().Get("fingerprint"))) {
			delete(d.peers, k)
		}
		w.WriteHeader(204)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", 405)
	}
}
func runDiscovery(ctx context.Context, cfg ServerConfig) error {
	s := &http.Server{Addr: cfg.Listen, Handler: NewDiscovery(cfg), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdown)
	}()
	log.Printf("discovery listening on %s; peer registry is in memory only", cfg.Listen)
	var err error
	if cfg.TLSCert != "" {
		err = s.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = s.ListenAndServe()
	}
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
