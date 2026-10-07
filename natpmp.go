// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// RFC 6886: requests go only to the selected interface's default gateway.
// A connected UDP socket prevents accepting replies from other endpoints.
func pmpExchange(ctx context.Context, local net.IP, gateway *net.UDPAddr, request []byte, size int) ([]byte, error) {
	c, err := net.DialUDP("udp4", &net.UDPAddr{IP: local}, gateway)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	for attempt := 0; attempt < 4; attempt++ {
		deadline := time.Now().Add((250 * time.Millisecond) << attempt)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		c.SetDeadline(deadline)
		if _, err = c.Write(request); err != nil {
			return nil, err
		}
		b := make([]byte, 64)
		n, e := c.Read(b)
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if ne, ok := e.(net.Error); ok && ne.Timeout() {
				continue
			}
			return nil, e
		}
		if n < 8 || b[0] != 0 || b[1] != request[1]+128 {
			return nil, errors.New("invalid NAT-PMP response header")
		}
		if code := binary.BigEndian.Uint16(b[2:4]); code != 0 {
			return nil, fmt.Errorf("NAT-PMP result code %d", code)
		}
		if n != size {
			return nil, errors.New("invalid NAT-PMP response size")
		}
		return b[:n], nil
	}
	return nil, errors.New("NAT-PMP gateway did not respond")
}

func pmpPortRequest(internal, external uint16, lifetime uint32) []byte {
	b := make([]byte, 12)
	b[1] = 2 // TCP only
	binary.BigEndian.PutUint16(b[4:6], internal)
	binary.BigEndian.PutUint16(b[6:8], external)
	binary.BigEndian.PutUint32(b[8:12], lifetime)
	return b
}

type pmpMapping struct {
	local     net.IP
	gateway   *net.UDPAddr
	port      uint16
	external  uint16
	url       string
	lifetime  time.Duration
	requested bool
}

func (m *pmpMapping) renew(ctx context.Context) error {
	b, err := pmpExchange(ctx, m.local, m.gateway, []byte{0, 0}, 12)
	if err != nil {
		return err
	}
	ip := net.IP(b[8:12])
	if !publicIPv4(ip) {
		return errors.New("router WAN address is not public (possible CGNAT or double NAT)")
	}
	external := m.external
	if external == 0 {
		external = m.port
	}
	m.requested = true
	b, err = pmpExchange(ctx, m.local, m.gateway, pmpPortRequest(m.port, external, 3600), 16)
	if err != nil {
		return err
	}
	external = binary.BigEndian.Uint16(b[10:12])
	seconds := binary.BigEndian.Uint32(b[12:16])
	if binary.BigEndian.Uint16(b[8:10]) != m.port || external == 0 || seconds < 60 {
		return errors.New("NAT-PMP returned an invalid TCP mapping")
	}
	m.external, m.lifetime = external, time.Duration(seconds)*time.Second
	m.url = "https://" + net.JoinHostPort(ip.String(), strconv.Itoa(int(external)))
	return nil
}
func (m *pmpMapping) endpoint() string            { return m.url }
func (m *pmpMapping) refreshAfter() time.Duration { return m.lifetime / 2 }
func (m *pmpMapping) close(ctx context.Context) error {
	_, err := pmpExchange(ctx, m.local, m.gateway, pmpPortRequest(m.port, 0, 0), 16)
	return err
}
