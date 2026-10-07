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
	"strings"
	"time"
)

func automaticAddress(s string) bool { return s == "" || s == "auto" }

func splitListen(s string) (string, int, error) {
	host, port, err := net.SplitHostPort(s)
	n, pe := strconv.Atoi(port)
	if err != nil || pe != nil || n < 0 || n > 65535 {
		return "", 0, fmt.Errorf("listen must be host:port with a numeric port (0..65535): %q", s)
	}
	return host, n, nil
}

type interfaceIP struct {
	name string
	ip   net.IP
}

func localAddresses(preferred string) ([]interfaceIP, error) {
	nics, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	found := preferred == ""
	var ips []interfaceIP
	for _, nic := range nics {
		if preferred != "" && nic.Name != preferred {
			continue
		}
		found = true
		if nic.Flags&net.FlagUp == 0 || nic.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := nic.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				ips = append(ips, interfaceIP{nic.Name, ip})
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("advertise_interface %q does not exist", preferred)
	}
	return ips, nil
}

// Ask the kernel which source address it would use to reach discovery. UDP
// connect sends no application packets; DNS may still be needed. Restrict the
// result to eligible local interfaces, and prefer IPv4 for ordinary LANs.
func routeAddress(ctx context.Context, discovery string, candidates []interfaceIP) net.IP {
	if len(candidates) == 0 {
		return nil
	}
	u, err := url.Parse(discovery)
	if err != nil {
		return nil
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return nil
	}
	for _, family := range []string{"udp4", "udp6"} {
		for i, ip := range ips {
			if i >= 16 || (family == "udp4") != (ip.IP.To4() != nil) {
				continue
			}
			d := net.Dialer{Timeout: time.Second}
			conn, err := d.DialContext(ctx, family, net.JoinHostPort(ip.String(), port))
			if err != nil {
				continue
			}
			local := conn.LocalAddr().(*net.UDPAddr).IP
			conn.Close()
			for _, candidate := range candidates {
				if local.Equal(candidate.ip) {
					return local
				}
			}
		}
	}
	return nil
}

func chooseAddress(candidates []interfaceIP, routed net.IP) (net.IP, error) {
	for _, c := range candidates {
		if c.ip.Equal(routed) {
			return c.ip, nil
		}
	}
	best := 10
	var chosen net.IP
	ambiguous := false
	for _, c := range candidates {
		rank := 3
		if c.ip.To4() != nil {
			rank = 1
		}
		if c.ip.IsPrivate() {
			rank--
		}
		if rank < best {
			best, chosen, ambiguous = rank, c.ip, false
		} else if rank == best && !c.ip.Equal(chosen) {
			ambiguous = true
		}
	}
	if chosen == nil || ambiguous {
		return nil, errors.New("cannot select a unique local address; set advertise_interface or an explicit advertise_url")
	}
	return chosen, nil
}

func resolveAdvertiseURL(ctx context.Context, cfg ClientConfig, bound string) (string, error) {
	if !automaticAddress(cfg.AdvertiseURL) {
		return cfg.AdvertiseURL, nil
	}
	host, port, err := splitListen(bound)
	if err != nil {
		return "", err
	}
	if strings.Contains(host, "%") {
		return "", errors.New("scoped IPv6 listen address requires an explicit advertise_url")
	}
	ip := net.ParseIP(host)
	if ip == nil && host != "" {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addrs) == 0 {
			return "", fmt.Errorf("cannot resolve listen host %q", host)
		}
		ip = addrs[0].IP
	}
	if ip != nil && ip.IsLinkLocalUnicast() {
		return "", errors.New("link-local listen address requires an explicit advertise_url")
	}
	if ip == nil || ip.IsUnspecified() {
		candidates, err := localAddresses(cfg.AdvertiseInterface)
		if err != nil {
			return "", err
		}
		hasIPv4 := false
		for _, c := range candidates {
			if c.ip.To4() != nil {
				hasIPv4 = true
			}
		}
		if (ip != nil && ip.To4() != nil) || (cfg.NATTraversal && hasIPv4) {
			filtered := candidates[:0]
			for _, c := range candidates {
				if c.ip.To4() != nil {
					filtered = append(filtered, c)
				}
			}
			candidates = filtered
		}
		ip, err = chooseAddress(candidates, routeAddress(ctx, cfg.DiscoveryURL, candidates))
		if err != nil {
			return "", err
		}
	}
	return "https://" + net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}
