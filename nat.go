// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"
)

type portMapping interface {
	renew(context.Context) error
	close(context.Context) error
	endpoint() string
	refreshAfter() time.Duration
}

func publicIPv4(ip net.IP) bool {
	v := ip.To4()
	if v == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Shared address space and special-use ranges cannot be advertised as WAN.
	for _, block := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4"} {
		_, prefix, _ := net.ParseCIDR(block)
		if prefix.Contains(v) {
			return false
		}
	}
	return true
}

func createMapping(ctx context.Context, local net.IP, port uint16, owner string) (portMapping, error) {
	gw, err := defaultGateway(ctx, local)
	if err != nil {
		return nil, err
	}
	pmp := &pmpMapping{local: local, gateway: &net.UDPAddr{IP: gw, Port: 5351}, port: port}
	pmpCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err = pmp.renew(pmpCtx)
	cancel()
	if err == nil {
		return pmp, nil
	}
	pmpErr := err
	if pmp.requested {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = pmp.close(cleanupCtx)
		cancel()
	}
	m, err := discoverUPnP(ctx, local, gw, port, owner, []*net.UDPAddr{{IP: gw, Port: 1900}, {IP: net.IPv4(239, 255, 255, 250), Port: 1900}})
	if err != nil {
		return nil, fmt.Errorf("NAT-PMP: %v; UPnP: %w", pmpErr, err)
	}
	return m, nil
}

type natManager struct {
	mu       sync.Mutex
	mapping  portMapping
	localURL string
	next     time.Time
	owner    string
	create   func(context.Context, net.IP, uint16, string) (portMapping, error)
	problem  error
}

func (m *natManager) ensure(ctx context.Context, localURL string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.localURL != localURL {
		if m.mapping != nil {
			_ = m.mapping.close(ctx)
		}
		m.localURL, m.mapping, m.next, m.problem = localURL, nil, time.Time{}, nil
	}
	if time.Now().Before(m.next) {
		if m.problem != nil {
			return "", m.problem
		}
		if m.mapping != nil {
			return m.mapping.endpoint(), nil
		}
		return "", nil
	}
	u, err := url.Parse(localURL)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(u.Hostname())
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil || port == 0 || ip == nil {
		return "", errors.New("NAT mapping needs a concrete local address and port")
	}
	if publicIPv4(ip) {
		return "", nil // Already directly addressed; no IPv4 NAT to map.
	}
	if ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return "", errors.New("automatic router mapping requires a non-loopback IPv4 address")
	}
	if m.mapping == nil {
		create := m.create
		if create == nil {
			create = createMapping
		}
		m.mapping, err = create(ctx, ip, uint16(port), m.owner)
	} else {
		err = m.mapping.renew(ctx)
	}
	m.problem = err
	if err != nil {
		m.next = time.Now().Add(60 * time.Second)
		return "", err
	}
	// Refresh at least once per minute to recover from router restarts and WAN
	// changes, and sooner for gateways granting short leases.
	delay := min(m.mapping.refreshAfter(), 60*time.Second)
	m.next = time.Now().Add(delay)
	return m.mapping.endpoint(), nil
}

func (m *natManager) close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mapping == nil {
		return nil
	}
	err := m.mapping.close(ctx)
	m.mapping = nil
	return err
}
