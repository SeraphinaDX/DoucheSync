// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// diagnose only queries discovery and authenticated peer manifest endpoints.
// It can run alongside a client: it neither opens folders/identities nor
// registers, unregisters, scans, downloads files, or changes local state.
func diagnose(ctx context.Context, cfg Config, out io.Writer) error {
	expected := cfg.Client.AdvertiseURL
	if automaticAddress(expected) {
		addressCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		selected, err := resolveAdvertiseURL(addressCtx, cfg.Client, cfg.Client.Listen)
		cancel()
		if err != nil {
			expected = ""
			fmt.Fprintf(out, "Automatic address selection FAILED: %v\n", err)
		} else {
			expected = selected
			_, port, _ := splitListen(cfg.Client.Listen)
			if port == 0 {
				expected = ""
			}
		}
	}
	display := expected
	if display == "" {
		display = "auto (see running client's registration below)"
	}
	fmt.Fprintf(out, "DoucheSync %s\nDevice: %s\nDiscovery: %s\nAdvertised: %s\nNAT traversal configured: %t\nUTC time: %s\n", version, cfg.Client.DeviceID, cfg.Client.DiscoveryURL, display, cfg.Client.NATTraversal, time.Now().UTC().Format(time.RFC3339))
	c := &Client{cfg: cfg.Client, http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("discovery redirects are forbidden") }}}
	var errs []error
	for _, fc := range cfg.Folders {
		room := roomID(fc)
		fmt.Fprintf(out, "\nFolder: %s\nDiscovery group: %s\n", fc.ID, room)
		r, err := c.discoveryRequest(ctx, http.MethodGet, "/v1/peers?room="+room, nil)
		if err == nil {
			err = requireStatus(r, http.StatusOK)
		}
		var peers []Announcement
		if err == nil {
			err = decodeLimited(r, 8<<20, &peers)
		}
		if r != nil {
			r.Body.Close()
		}
		if err != nil {
			fmt.Fprintf(out, "Discovery query FAILED: %v\n", err)
			errs = append(errs, fmt.Errorf("folder %s discovery: %w", fc.ID, err))
			continue
		}
		fmt.Fprintf(out, "Discovery query OK: %d announcement(s)\n", len(peers))
		sort.Slice(peers, func(i, j int) bool { return peers[i].Device < peers[j].Device })
		self, other := 0, 0
		seen := map[string]bool{}
		for _, p := range peers {
			if !p.verified(fc) || seen[p.Device] {
				err := fmt.Errorf("invalid, expired, or duplicate announcement for device %q", p.Device)
				fmt.Fprintf(out, "REJECTED: %v; check clocks and folder settings\n", err)
				errs = append(errs, err)
				continue
			}
			seen[p.Device] = true
			if p.Device == cfg.Client.DeviceID {
				self++
				fmt.Fprintf(out, "Own device registered at %s\n", p.URL)
				for _, u := range p.URLs {
					fmt.Fprintf(out, "Additional advertised address: %s\n", u)
				}
				if expected != "" && p.URL != expected {
					fmt.Fprintln(out, "WARNING: registered address differs from this config; check for another client using this device_id or an old running config")
				}
				continue
			}
			other++
			reachable := false
			var failures []error
			for _, u := range p.endpoints() {
				fmt.Fprintf(out, "Peer %s at %s: ", p.Device, u)
				selected := p
				selected.URL = u
				if err := probePeer(ctx, fc, selected); err != nil {
					fmt.Fprintf(out, "FAILED: %v\n", err)
					failures = append(failures, err)
				} else {
					reachable = true
					fmt.Fprintln(out, "OK (pinned TLS and authenticated manifest request)")
				}
			}
			if !reachable {
				errs = append(errs, fmt.Errorf("folder %s peer %s: %w", fc.ID, p.Device, errors.Join(failures...)))
			}
		}
		if self == 0 {
			fmt.Fprintln(out, "This device is not registered in this group. Keep its client running and check its discovery errors.")
		}
		if other == 0 {
			fmt.Fprintln(out, "No other verified devices in this group. Compare Discovery and Discovery group on both machines; both must match. Device IDs must differ. This command does not register a client.")
		}
	}
	return errors.Join(errs...)
}

func probePeer(ctx context.Context, fc FolderConfig, p Announcement) error {
	h := pinnedClient(p, 15*time.Second)
	defer h.CloseIdleConnections()
	r, err := peerGET(ctx, h, p, &Folder{cfg: fc}, "/v1/manifest?room="+roomID(fc))
	if err != nil {
		return err
	}
	defer r.Body.Close()
	// A successful response proves reachability, the signed certificate pin,
	// and request authentication. No manifest or file needs to be downloaded.
	return requireStatus(r, http.StatusOK)
}
