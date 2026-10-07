// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRestartKeepsCachedPeerWorking(t *testing.T) {
	n := newNetwork(t, false)
	put(t, n.dirA, "before.txt", "before restart")
	n.poll(t)
	n.cycle(t)
	room := roomID(n.cfgA.Folders[0])
	cached := n.b.peers[room][0]
	oldFingerprint := n.a.fingerprint
	// Simulate a lost connection/crash: the discovery lease and the other
	// client's cached certificate stay in place while alpha restarts.
	n.a.Close()
	var err error
	n.a, err = NewClient(n.cfgA)
	if err != nil {
		t.Fatal(err)
	}
	put(t, n.dirA, "after.txt", "after restart")
	if err = folder(n.a).Scan(); err != nil {
		t.Fatal(err)
	}
	if err = n.b.syncPeer(context.Background(), folder(n.b), cached); err != nil {
		t.Fatalf("cached peer stopped working after restart: %v", err)
	}
	if n.a.fingerprint != oldFingerprint {
		t.Fatal("certificate changed after restart")
	}
	if get(t, n.dirB, "after.txt") != "after restart" {
		t.Fatal("file not transferred after restart")
	}
	if err = n.a.PollDiscovery(context.Background()); err != nil {
		t.Fatalf("old discovery lease blocked restarted client: %v", err)
	}
}

func TestIdentityCannotBeUsedByTwoProcesses(t *testing.T) {
	cfg := ClientConfig{DeviceID: "desktop", IdentityDir: t.TempDir()}
	first, err := openPeerIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := openPeerIdentity(cfg); err == nil {
		second.Close()
		t.Fatal("identity was used by two clients at once")
	}
}

func TestDamagedIdentityIsNotReplaced(t *testing.T) {
	dir := t.TempDir()
	cfg := ClientConfig{DeviceID: "desktop", IdentityDir: dir}
	identity, err := openPeerIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	identity.Close()
	file := filepath.Join(dir, "desktop.pem")
	if err = os.WriteFile(file, []byte("broken identity"), 0600); err != nil {
		t.Fatal(err)
	}
	if identity, err = openPeerIdentity(cfg); err == nil {
		identity.Close()
		t.Fatal("damaged identity silently replaced")
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "broken identity" {
		t.Fatal("damaged identity was overwritten")
	}
}

func TestIdentityUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses filesystem ACLs instead of Unix permissions")
	}
	dir := t.TempDir()
	cfg := ClientConfig{DeviceID: "desktop", IdentityDir: dir}
	identity, err := openPeerIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	identity.Close()
	file := filepath.Join(dir, "desktop.pem")
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("private key permissions: %v", info.Mode().Perm())
	}
	if err = os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if identity, err = openPeerIdentity(cfg); err == nil {
		identity.Close()
		t.Fatal("publicly readable private key was accepted")
	}
}
