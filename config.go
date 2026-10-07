// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

// DoucheSync is a peer-to-peer folder synchronizer. The discovery process never
// receives file manifests or contents; those endpoints exist only on clients.
package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const version = "0.2.0"
const maxManifestBytes = 64 << 20
const maxEntries = 100000

type Config struct {
	Server  ServerConfig   `toml:"server"`
	Client  ClientConfig   `toml:"client"`
	Folders []FolderConfig `toml:"folders"`
}
type ServerConfig struct {
	Listen   string `toml:"listen"`
	Token    string `toml:"token"`
	TLSCert  string `toml:"tls_cert"`
	TLSKey   string `toml:"tls_key"`
	PeerTTL  string `toml:"peer_ttl"`
	MaxPeers int    `toml:"max_peers"`
}
type ClientConfig struct {
	DeviceID           string `toml:"device_id"`
	IdentityDir        string `toml:"identity_dir"`
	Listen             string `toml:"listen"`
	AdvertiseURL       string `toml:"advertise_url"`
	AdvertiseInterface string `toml:"advertise_interface"`
	NATTraversal       bool   `toml:"nat_traversal"`
	DiscoveryURL       string `toml:"discovery_url"`
	DiscoveryToken     string `toml:"discovery_token"`
	AllowHTTPDiscovery bool   `toml:"allow_http_discovery"`
	ScanInterval       string `toml:"scan_interval"`
	TransferTimeout    string `toml:"transfer_timeout"`
}
type FolderConfig struct {
	ID          string   `toml:"id"`
	Path        string   `toml:"path"`
	Secret      string   `toml:"secret"`
	SyncDeletes bool     `toml:"sync_deletes"`
	Ignore      []string `toml:"ignore"`
	MaxFileSize int64    `toml:"max_file_size"`
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func configPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(d, "douchesync", "config.toml")
}
func expandPath(s string) (string, error) {
	if s == "~" || strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		s = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(s[1:], "/"), `\`))
	}
	return filepath.Abs(s)
}
func duration(s string, fallback time.Duration) (time.Duration, error) {
	if s == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Second || d > 24*time.Hour {
		return 0, fmt.Errorf("invalid duration %q (use 1s through 24h)", s)
	}
	return d, nil
}
func endpoint(s string, scheme string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != scheme || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("expected %s://host:port, got %q", scheme, s)
	}
	return nil
}
func readConfig(name, mode string) (Config, error) {
	var c Config
	meta, err := toml.DecodeFile(name, &c)
	if err != nil {
		return c, err
	}
	if extra := meta.Undecoded(); len(extra) > 0 {
		return c, fmt.Errorf("unknown TOML keys: %v", extra)
	}
	if mode == "server" {
		if c.Server.Listen == "" {
			c.Server.Listen = "127.0.0.1:7443"
		}
		if len(c.Server.Token) < 32 {
			return c, errors.New("server.token must contain at least 32 characters; run douchesync keygen")
		}
		if (c.Server.TLSCert == "") != (c.Server.TLSKey == "") {
			return c, errors.New("provide both tls_cert and tls_key")
		}
		if c.Server.TLSCert == "" {
			host, _, err := net.SplitHostPort(c.Server.Listen)
			if err != nil || (host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback())) {
				return c, errors.New("plain HTTP server must bind loopback; use TLS certificates or a local HTTPS reverse proxy")
			}
		}
		if c.Server.MaxPeers == 0 {
			c.Server.MaxPeers = 1000
		}
		if c.Server.MaxPeers < 1 || c.Server.MaxPeers > 10000 {
			return c, errors.New("max_peers must be 1..10000")
		}
		ttl, e := duration(c.Server.PeerTTL, 90*time.Second)
		if e != nil {
			return c, e
		}
		if ttl < 30*time.Second || ttl > 2*time.Minute {
			return c, errors.New("peer_ttl must be 30s..2m")
		}
		return c, err
	}
	if !validID.MatchString(c.Client.DeviceID) {
		return c, errors.New("client.device_id must be 1..64 letters, digits, underscores or hyphens, unique per machine")
	}
	c.Client.IdentityDir, err = identityPath(c.Client)
	if err != nil {
		return c, fmt.Errorf("client identity directory: %w", err)
	}
	if c.Client.Listen == "" {
		c.Client.Listen = ":7444"
	}
	if _, _, err = splitListen(c.Client.Listen); err != nil {
		return c, err
	}
	if !automaticAddress(c.Client.AdvertiseURL) {
		if err = endpoint(c.Client.AdvertiseURL, "https"); err != nil {
			return c, err
		}
		if c.Client.NATTraversal {
			return c, errors.New("nat_traversal requires automatic advertise_url (omit it or set it to 'auto')")
		}
	}
	c.Client.AdvertiseURL = strings.TrimRight(c.Client.AdvertiseURL, "/")
	u, err := url.Parse(c.Client.DiscoveryURL)
	if err != nil {
		return c, err
	}
	if u.Scheme == "http" && !c.Client.AllowHTTPDiscovery {
		return c, errors.New("discovery_url must use HTTPS (allow_http_discovery is for trusted networks/local testing)")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return c, errors.New("discovery_url must use http or https")
	}
	if err = endpoint(c.Client.DiscoveryURL, u.Scheme); err != nil {
		return c, err
	}
	c.Client.DiscoveryURL = strings.TrimRight(c.Client.DiscoveryURL, "/")
	if len(c.Client.DiscoveryToken) < 32 {
		return c, errors.New("discovery_token must contain at least 32 characters")
	}
	if _, err = duration(c.Client.ScanInterval, 10*time.Second); err != nil {
		return c, err
	}
	if _, err = duration(c.Client.TransferTimeout, 30*time.Minute); err != nil {
		return c, err
	}
	if len(c.Folders) == 0 || len(c.Folders) > 64 {
		return c, errors.New("configure 1..64 [[folders]] sections")
	}
	ids := map[string]bool{}
	for i := range c.Folders {
		f := &c.Folders[i]
		if !validID.MatchString(f.ID) || ids[f.ID] {
			return c, errors.New("folder IDs must be valid and unique")
		}
		ids[f.ID] = true
		if f.Path == "" {
			return c, fmt.Errorf("folder %s requires path", f.ID)
		}
		if len(f.Secret) < 32 {
			return c, fmt.Errorf("folder %s secret needs at least 32 characters", f.ID)
		}
		f.Path, err = expandPath(f.Path)
		if err != nil {
			return c, err
		}
		// Require existing roots: a missing/unmounted disk must not look like a mass deletion.
		info, e := os.Stat(f.Path)
		if e != nil || !info.IsDir() {
			return c, fmt.Errorf("folder %s must already exist as a directory: %s", f.ID, f.Path)
		}
		f.Path, err = filepath.EvalSymlinks(f.Path)
		if err != nil {
			return c, err
		}
		if f.MaxFileSize == 0 {
			f.MaxFileSize = 10 << 30
		}
		if f.MaxFileSize < 1 || f.MaxFileSize > 1<<50 {
			return c, errors.New("max_file_size must be 1..1125899906842624 bytes")
		}
		for _, pattern := range f.Ignore {
			if _, e := filepath.Match(pattern, "test"); e != nil {
				return c, fmt.Errorf("invalid ignore pattern %q", pattern)
			}
		}
		for j := 0; j < i; j++ {
			other := c.Folders[j].Path
			if f.Path == other || strings.HasPrefix(f.Path, other+string(os.PathSeparator)) || strings.HasPrefix(other, f.Path+string(os.PathSeparator)) {
				return c, errors.New("shared folder roots cannot overlap")
			}
		}
	}
	return c, nil
}
