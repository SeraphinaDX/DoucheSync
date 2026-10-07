// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func defaultGateway(ctx context.Context, local net.IP) (net.IP, error) {
	ips, err := localAddresses("")
	if err != nil {
		return nil, err
	}
	nic := ""
	for _, ip := range ips {
		if ip.ip.Equal(local) {
			nic = ip.name
			break
		}
	}
	var text []byte
	switch runtime.GOOS {
	case "linux":
		text, err = os.ReadFile("/proc/net/route")
	case "windows":
		text, err = exec.CommandContext(ctx, "route", "print", "-4").Output()
	case "darwin":
		text, err = exec.CommandContext(ctx, "/sbin/route", "-n", "get", "-inet", "default").Output()
	default:
		return nil, errors.New("automatic NAT gateway discovery supports Linux, Windows, and macOS")
	}
	if err != nil {
		return nil, err
	}
	return parseGateway(runtime.GOOS, string(text), nic, local)
}

func parseGateway(platform, text, nic string, local net.IP) (net.IP, error) {
	var chosen net.IP
	best := int64(1 << 62)
	darwinNic := ""
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		switch platform {
		case "linux":
			if len(f) < 8 || f[0] != nic || f[1] != "00000000" || f[7] != "00000000" {
				continue
			}
			flags, fe := strconv.ParseUint(f[3], 16, 32)
			metric, me := strconv.ParseInt(f[6], 10, 64)
			g, ge := strconv.ParseUint(f[2], 16, 32)
			if fe == nil && me == nil && ge == nil && flags&3 == 3 && metric < best {
				chosen = net.IPv4(byte(g), byte(g>>8), byte(g>>16), byte(g>>24))
				best = metric
			}
		case "windows":
			if len(f) != 5 || f[0] != "0.0.0.0" || f[1] != "0.0.0.0" || !net.ParseIP(f[3]).Equal(local) {
				continue
			}
			metric, err := strconv.ParseInt(f[4], 10, 64)
			ip := net.ParseIP(f[2])
			if err == nil && ip != nil && metric < best {
				chosen, best = ip, metric
			}
		case "darwin":
			if len(f) == 2 && f[0] == "gateway:" {
				chosen = net.ParseIP(f[1])
			}
			if len(f) == 2 && f[0] == "interface:" {
				darwinNic = f[1]
			}
		}
	}
	if chosen == nil || chosen.To4() == nil || chosen.IsUnspecified() || (platform == "darwin" && darwinNic != nic) {
		return nil, errors.New("no IPv4 default gateway for the selected interface")
	}
	return chosen.To4(), nil
}
