// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func mac(key, data string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}
func macEqual(a, b string) bool {
	x, e1 := hex.DecodeString(a)
	y, e2 := hex.DecodeString(b)
	return e1 == nil && e2 == nil && hmac.Equal(x, y)
}
func roomID(f FolderConfig) string { return mac(f.Secret, "douchesync-v1-room\n"+f.ID) }

// Announcements contain no paths, filenames, file metadata, or file contents.
// The per-folder proof authenticates the endpoint AND certificate fingerprint,
// so even an altered discovery response cannot redirect a client to an impostor.
type Announcement struct {
	Room        string   `json:"room"`
	Device      string   `json:"device"`
	URL         string   `json:"url"`
	URLs        []string `json:"urls,omitempty"` // signed alternative endpoints
	Fingerprint string   `json:"fingerprint"`
	Expires     int64    `json:"expires"`
	Proof       string   `json:"proof"`
}

func (a Announcement) signingData() string {
	data := []any{"douchesync-v1-peer", a.Room, a.Device, a.URL, a.Fingerprint, a.Expires}
	if len(a.URLs) != 0 {
		data = []any{"douchesync-v2-peer", a.Room, a.Device, a.URL, a.URLs, a.Fingerprint, a.Expires}
	}
	b, _ := json.Marshal(data)
	return string(b)
}
func (a Announcement) verified(f FolderConfig) bool {
	return a.Room == roomID(f) && validID.MatchString(a.Device) && a.validEndpoints() && len(a.Fingerprint) == 64 && a.Expires > time.Now().Unix() && a.Expires <= time.Now().Add(3*time.Minute).Unix() && macEqual(a.Proof, mac(f.Secret, a.signingData()))
}

func (a Announcement) endpoints() []string {
	return append([]string{a.URL}, a.URLs...)
}

func (a Announcement) validEndpoints() bool {
	if len(a.URLs) > 3 {
		return false
	}
	seen := map[string]bool{}
	for _, u := range a.endpoints() {
		if endpoint(u, "https") != nil || seen[u] {
			return false
		}
		seen[u] = true
	}
	return true
}
func peerCertificate() (tls.Certificate, string, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "DoucheSync peer"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, digest(der), nil
}
func pinnedClient(a Announcement, timeout time.Duration) *http.Client {
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Normal PKI validation is replaced with a signed, exact certificate pin.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("peer did not present a certificate")
			}
			actual := digest(cs.PeerCertificates[0].Raw)
			if !macEqual(actual, a.Fingerprint) {
				return fmt.Errorf("peer certificate fingerprint mismatch for %s at %s (discovery: %s; received: %s); check advertise_url and device_id", a.Device, a.URL, a.Fingerprint, actual)
			}
			return nil
		},
	}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, DisableKeepAlives: true}
	if len(a.URLs) != 0 {
		tr.DialContext = (&net.Dialer{Timeout: 2 * time.Second}).DialContext
		tr.TLSHandshakeTimeout = 3 * time.Second
		tr.ResponseHeaderTimeout = 10 * time.Second
	}
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("peer redirects are forbidden") }}
}
func signRequest(req *http.Request, secret string) {
	ts, nonce := strconv.FormatInt(time.Now().Unix(), 10), randomHex(16)
	req.Header.Set("X-DoucheSync-Time", ts)
	req.Header.Set("X-DoucheSync-Nonce", nonce)
	req.Header.Set("X-DoucheSync-MAC", mac(secret, req.Method+"\n"+req.URL.RequestURI()+"\n"+ts+"\n"+nonce))
}

type replayGuard struct {
	mu     sync.Mutex
	nonces map[string]int64
}

func (g *replayGuard) authorize(r *http.Request, secret string) bool {
	ts := r.Header.Get("X-DoucheSync-Time")
	t, err := strconv.ParseInt(ts, 10, 64)
	now := time.Now().Unix()
	nonce := r.Header.Get("X-DoucheSync-Nonce")
	if err != nil || t < now-60 || t > now+60 || len(nonce) != 32 || !macEqual(r.Header.Get("X-DoucheSync-MAC"), mac(secret, r.Method+"\n"+r.URL.RequestURI()+"\n"+ts+"\n"+nonce)) {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.nonces == nil {
		g.nonces = make(map[string]int64)
	}
	for n, expiry := range g.nonces {
		if expiry < now {
			delete(g.nonces, n)
		}
	}
	k := mac(secret, nonce)
	if _, exists := g.nonces[k]; exists || len(g.nonces) >= 20000 {
		return false
	}
	g.nonces[k] = now + 120
	return true
}
func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func requireStatus(r *http.Response, want int) error {
	if r.StatusCode != want {
		return fmt.Errorf("HTTP %d (%s)", r.StatusCode, http.StatusText(r.StatusCode))
	}
	return nil
}
