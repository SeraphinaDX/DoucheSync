// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutomaticAddressSelection(t *testing.T) {
	ip := func(s string) net.IP { return net.ParseIP(s) }
	lan := interfaceIP{"ethernet", ip("10.0.0.2")}
	vpn := interfaceIP{"vpn", ip("10.8.0.2")}
	v6 := interfaceIP{"ethernet", ip("2001:db8::2")}
	for _, tc := range []struct {
		name       string
		candidates []interfaceIP
		route      net.IP
		want       string
	}{
		{"single LAN", []interfaceIP{lan, v6}, nil, "10.0.0.2"},
		{"routed LAN", []interfaceIP{vpn, lan}, lan.ip, "10.0.0.2"},
		{"routed VPN", []interfaceIP{lan, vpn}, vpn.ip, "10.8.0.2"},
		{"ambiguous", []interfaceIP{lan, vpn}, nil, ""},
		{"no address", nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chooseAddress(tc.candidates, tc.route)
			if tc.want == "" {
				if err == nil {
					t.Fatal("expected an actionable selection error")
				}
				return
			}
			if err != nil || got.String() != tc.want {
				t.Fatalf("%v: %v", got, err)
			}
		})
	}
	for _, tc := range []struct{ bound, want string }{{"127.0.0.1:12345", "https://127.0.0.1:12345"}, {"[2001:db8::2]:12345", "https://[2001:db8::2]:12345"}} {
		got, err := resolveAdvertiseURL(context.Background(), ClientConfig{}, tc.bound)
		if err != nil || got != tc.want {
			t.Fatalf("%q: %v", got, err)
		}
	}
	if _, err := localAddresses("__douchesync_nonexistent_interface__"); err == nil {
		t.Fatal("unknown interface accepted")
	}
	got, err := resolveAdvertiseURL(context.Background(), ClientConfig{AdvertiseURL: "https://vpn.example:7444"}, "invalid")
	if err != nil || got != "https://vpn.example:7444" {
		t.Fatal("explicit override was not preserved")
	}
}

func TestAutomaticAddressesTransfer(t *testing.T) {
	n := newNetwork(t, false)
	n.a.Close()
	n.b.Close()
	n.cfgA.Client.Listen, n.cfgA.Client.AdvertiseURL = "127.0.0.1:0", ""
	n.cfgB.Client.Listen, n.cfgB.Client.AdvertiseURL = "127.0.0.1:0", "auto"
	var err error
	n.a, err = NewClient(n.cfgA)
	if err != nil {
		t.Fatal(err)
	}
	n.b, err = NewClient(n.cfgB)
	if err != nil {
		t.Fatal(err)
	}
	put(t, n.dirA, "automatic.txt", "automatic endpoint")
	n.poll(t)
	n.cycle(t)
	if get(t, n.dirB, "automatic.txt") != "automatic endpoint" {
		t.Fatal("automatic endpoints did not transfer")
	}
	if n.a.localURL != "https://"+n.a.listener.Addr().String() || strings.HasSuffix(n.a.localURL, ":0") {
		t.Fatal("actual allocated listener port was not advertised")
	}
	var out bytes.Buffer
	if err = diagnose(context.Background(), n.cfgA, &out); err != nil || !strings.Contains(out.String(), "OK (pinned TLS") {
		t.Fatalf("diagnosis: %v\n%s", err, out.String())
	}
}

func TestAutomaticAddressTOML(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "config.toml")
	base := fmt.Sprintf("[client]\ndevice_id='desktop'\nlisten=':7444'\ndiscovery_url='https://sync.example.com'\ndiscovery_token='%s'\n%s\n[[folders]]\nid='docs'\npath='%s'\nsecret='%s'\n", testToken, "%s", filepath.ToSlash(d), testSecret)
	for _, extra := range []string{"", "advertise_url='auto'\nnat_traversal=true", "advertise_interface='ethernet'"} {
		os.WriteFile(path, []byte(fmt.Sprintf(base, extra)), 0600)
		if _, err := readConfig(path, "client"); err != nil {
			t.Fatal(err)
		}
	}
	for _, extra := range []string{"advertise_url='http://10.0.0.2:7444'", "advertise_url='https://10.0.0.2:7444'\nnat_traversal=true"} {
		os.WriteFile(path, []byte(fmt.Sprintf(base, extra)), 0600)
		if _, err := readConfig(path, "client"); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	for _, listen := range []string{"7444", ":-1", ":65536", ":http"} {
		if _, _, err := splitListen(listen); err == nil {
			t.Fatal("invalid listen accepted")
		}
	}
}

func TestGatewayParsers(t *testing.T) {
	local := net.ParseIP("10.0.0.2")
	for _, tc := range []struct{ platform, text, nic string }{
		{"linux", "Iface Destination Gateway Flags RefCnt Use Metric Mask\neth0 00000000 0100000A 0003 0 0 100 00000000\nvpn0 00000000 0100080A 0003 0 0 1 00000000", "eth0"},
		{"windows", "Network Destination Netmask Gateway Interface Metric\n0.0.0.0 0.0.0.0 10.8.0.1 10.8.0.2 1\n0.0.0.0 0.0.0.0 10.0.0.1 10.0.0.2 20", "Ethernet"},
		{"darwin", " gateway: 10.0.0.1\n interface: en0", "en0"},
	} {
		gw, err := parseGateway(tc.platform, tc.text, tc.nic, local)
		if err != nil || gw.String() != "10.0.0.1" {
			t.Fatalf("%s: %v %v", tc.platform, gw, err)
		}
	}
	if _, err := parseGateway("linux", "", "eth0", local); err == nil {
		t.Fatal("missing gateway accepted")
	}
}

func announceAlternative(t *testing.T, n *testNetwork, primary string, alternatives []string) Announcement {
	t.Helper()
	a := Announcement{Room: roomID(n.cfgB.Folders[0]), Device: "beta", URL: primary, URLs: alternatives, Fingerprint: n.b.fingerprint, Expires: time.Now().Add(time.Minute).Unix()}
	a.Proof = mac(testSecret, a.signingData())
	b, _ := json.Marshal(a)
	r, err := n.b.discoveryRequest(context.Background(), http.MethodPost, "/v1/peers", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if err = requireStatus(r, 204); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAlternativeEndpointTransferAndPinning(t *testing.T) {
	n := newNetwork(t, false)
	put(t, n.dirB, "fallback.txt", "direct alternative transfer")
	if err := folder(n.b).Scan(); err != nil {
		t.Fatal(err)
	}
	a := announceAlternative(t, n, "https://127.0.0.1:1", []string{n.b.localURL})
	if !a.verified(n.cfgA.Folders[0]) {
		t.Fatal("signed alternatives rejected")
	}
	tampered := a
	tampered.URLs = []string{n.a.localURL}
	if tampered.verified(n.cfgA.Folders[0]) {
		t.Fatal("alternative was not covered by signature")
	}
	if err := n.a.PollDiscovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := n.a.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if get(t, n.dirA, "fallback.txt") != "direct alternative transfer" {
		t.Fatal("fallback did not transfer")
	}
	if n.a.preferred[a.Room+"/beta"] != n.b.localURL {
		t.Fatal("working endpoint not remembered")
	}
	bad := a
	bad.URLs = []string{n.a.localURL}
	bad.Proof = mac(testSecret, bad.signingData())
	if err := n.a.syncPeer(context.Background(), folder(n.a), bad); err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("alternative bypassed pinning: %v", err)
	}
	var out bytes.Buffer
	if err := diagnose(context.Background(), n.cfgA, &out); err != nil || !strings.Contains(out.String(), "OK (pinned TLS") {
		t.Fatalf("working alternative diagnosed as failed: %v\n%s", err, out.String())
	}
}

func TestRejectUnsafeAlternativeEndpoints(t *testing.T) {
	a := Announcement{URL: "https://10.0.0.2:7444"}
	for _, urls := range [][]string{{a.URL}, {"http://10.0.0.2:7444"}, {"https://user:password@10.0.0.2:7444"}, {"https://10.0.0.3:7444", "https://10.0.0.4:7444", "https://10.0.0.5:7444", "https://10.0.0.6:7444"}} {
		a.URLs = urls
		if a.validEndpoints() {
			t.Fatal("unsafe alternatives accepted")
		}
	}
}

func TestClientNATAnnouncementsAndLANFallback(t *testing.T) {
	n := newNetwork(t, false)
	n.a.cfg.AdvertiseURL = "auto"
	// Simulate an already-established WAN mapping. Router wire protocols are
	// exercised separately; this test makes no requests outside the test LAN.
	n.a.nat = &natManager{localURL: n.a.localURL, mapping: &fakeMapping{}, next: time.Now().Add(time.Minute)}
	put(t, n.dirA, "LAN.txt", "LAN stays usable with a WAN mapping")
	if err := folder(n.a).Scan(); err != nil {
		t.Fatal(err)
	}
	n.poll(t)
	a := n.b.peers[roomID(n.cfgA.Folders[0])][0]
	if a.URL != n.a.localURL || len(a.URLs) != 1 || !a.verified(n.cfgB.Folders[0]) {
		t.Fatal("NAT endpoints not registered and signed")
	}
	if err := n.b.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if get(t, n.dirB, "LAN.txt") != "LAN stays usable with a WAN mapping" {
		t.Fatal("WAN advertisement broke LAN sync")
	}
	// Failed mapping refresh withdraws only the WAN endpoint, retaining LAN
	// discovery. Loopback cannot create a real router mapping in this fixture.
	n.a.nat.next = time.Time{}
	if err := n.a.PollDiscovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := n.b.PollDiscovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	a = n.b.peers[roomID(n.cfgA.Folders[0])][0]
	if len(a.URLs) != 0 || a.URL != n.a.localURL {
		t.Fatal("LAN fallback did not withdraw unavailable WAN endpoint")
	}
}
