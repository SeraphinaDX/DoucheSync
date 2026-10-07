// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type upnpService struct {
	Type    string `xml:"serviceType"`
	Control string `xml:"controlURL"`
}
type upnpDevice struct {
	Services []upnpService `xml:"serviceList>service"`
	Devices  []upnpDevice  `xml:"deviceList>device"`
}
type upnpDescription struct {
	Base   string     `xml:"URLBase"`
	Device upnpDevice `xml:"device"`
}

func routerURL(raw string, router net.IP) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || !net.ParseIP(u.Hostname()).Equal(router) {
		return nil, errors.New("UPnP URL must address the discovered default gateway directly")
	}
	return u, nil
}

func routerHTTP(local net.IP) *http.Client {
	d := &net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: local}}
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DialContext: d.DialContext, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("UPnP redirects are forbidden") }}
}

// SSDP responses are accepted only from the selected default gateway. Device
// descriptions and SOAP URLs must remain on that same literal IP; redirects,
// proxy environment variables, and off-router URLs cannot redirect requests.
func discoverUPnP(ctx context.Context, local, gateway net.IP, port uint16, owner string, targets []*net.UDPAddr) (*upnpMapping, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: local})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.SetDeadline(deadline)
	for _, target := range targets {
		request := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 1\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n\r\n"
		_, _ = c.WriteToUDP([]byte(request), target)
	}
	seen := map[string]bool{}
	var last error
	for {
		packet := make([]byte, 8192)
		n, from, err := c.ReadFromUDP(packet)
		if err != nil {
			if last != nil {
				return nil, last
			}
			return nil, errors.New("no compatible UPnP gateway responded")
		}
		if !from.IP.Equal(gateway) {
			continue
		}
		r, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(packet[:n])), nil)
		if err != nil {
			continue
		}
		location := r.Header.Get("Location")
		r.Body.Close()
		if r.StatusCode != 200 || seen[location] {
			continue
		}
		seen[location] = true
		mappings, err := loadUPnP(ctx, routerHTTP(local), location, gateway, local, port, owner)
		if err != nil {
			last = err
			continue
		}
		for _, m := range mappings {
			if err := m.renew(ctx); err == nil {
				return m, nil
			} else {
				last = err
			}
			// A lost SOAP response can follow a successful AddPortMapping.
			// Remove only a mapping whose ownership can be verified.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = m.close(cleanupCtx)
			cancel()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
	}
}

func boundedBody(r *http.Response) ([]byte, error) {
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return nil, errors.New("invalid or oversized router response")
	}
	return b, nil
}

func loadUPnP(ctx context.Context, h *http.Client, location string, gateway, local net.IP, port uint16, owner string) ([]*upnpMapping, error) {
	base, err := routerURL(location, gateway)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	r, err := h.Do(req)
	if err != nil {
		return nil, err
	}
	b, err := boundedBody(r)
	if err != nil {
		return nil, err
	}
	if err = requireStatus(r, 200); err != nil {
		return nil, err
	}
	var desc upnpDescription
	if err = xml.Unmarshal(b, &desc); err != nil {
		return nil, err
	}
	if desc.Base != "" {
		base, err = routerURL(desc.Base, gateway)
		if err != nil {
			return nil, err
		}
	}
	var services []upnpService
	var walk func(upnpDevice)
	walk = func(d upnpDevice) {
		services = append(services, d.Services...)
		for _, child := range d.Devices {
			walk(child)
		}
	}
	walk(desc.Device)
	var mappings []*upnpMapping
	for _, service := range services {
		switch service.Type {
		case "urn:schemas-upnp-org:service:WANIPConnection:1", "urn:schemas-upnp-org:service:WANIPConnection:2", "urn:schemas-upnp-org:service:WANPPPConnection:1":
		default:
			continue
		}
		u, err := url.Parse(service.Control)
		if err != nil {
			continue
		}
		control, err := routerURL(base.ResolveReference(u).String(), gateway)
		if err == nil {
			mappings = append(mappings, &upnpMapping{http: h, control: control.String(), service: service.Type, local: local, port: port, owner: owner})
			if len(mappings) == 8 {
				break
			}
		}
	}
	if len(mappings) != 0 {
		return mappings, nil
	}
	return nil, errors.New("gateway has no safe WANIPConnection or WANPPPConnection service")
}

type soapFault struct {
	code        int
	description string
}

var errMappingOwned = errors.New("UPnP port belongs to another mapping")

func (e *soapFault) Error() string { return fmt.Sprintf("UPnP error %d (%q)", e.code, e.description) }

type soapArg struct{ name, value string }

func (m *upnpMapping) call(ctx context.Context, action string, args ...soapArg) (map[string]string, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:%s xmlns:u="%s">`, action, m.service)
	for _, arg := range args {
		fmt.Fprintf(&b, "<%s>", arg.name)
		_ = xml.EscapeText(&b, []byte(arg.value))
		fmt.Fprintf(&b, "</%s>", arg.name)
	}
	fmt.Fprintf(&b, "</u:%s></s:Body></s:Envelope>", action)
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.control, &b)
	r.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	r.Header.Set("SOAPAction", `"`+m.service+"#"+action+`"`)
	resp, err := m.http.Do(r)
	if err != nil {
		return nil, err
	}
	body, err := boundedBody(resp)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	dec := xml.NewDecoder(bytes.NewReader(body))
	var stack []string
	for {
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			stack = append(stack, token.Name.Local)
			if len(stack) > 64 {
				return nil, errors.New("router XML nesting limit exceeded")
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) != 0 {
				values[stack[len(stack)-1]] += string(token)
			}
		}
	}
	for k, v := range values {
		values[k] = strings.TrimSpace(v)
	}
	if code, _ := strconv.Atoi(values["errorCode"]); code != 0 {
		return nil, &soapFault{code, values["errorDescription"]}
	}
	if err = requireStatus(resp, 200); err != nil {
		return nil, err
	}
	return values, nil
}

type upnpMapping struct {
	http                    *http.Client
	control, service, owner string
	local                   net.IP
	port, external          uint16
	url                     string
	permanent               bool
}

func (m *upnpMapping) checkOwner(ctx context.Context) (bool, error) {
	v, err := m.call(ctx, "GetSpecificPortMappingEntry", soapArg{"NewRemoteHost", ""}, soapArg{"NewExternalPort", strconv.Itoa(int(m.external))}, soapArg{"NewProtocol", "TCP"})
	var fault *soapFault
	if errors.As(err, &fault) && fault.code == 714 {
		return false, nil // No such mapping.
	}
	if err != nil {
		return false, err
	}
	if !net.ParseIP(v["NewInternalClient"]).Equal(m.local) || v["NewInternalPort"] != strconv.Itoa(int(m.port)) || v["NewPortMappingDescription"] != m.owner {
		return false, errMappingOwned
	}
	return true, nil
}

func (m *upnpMapping) renew(ctx context.Context) error {
	v, err := m.call(ctx, "GetExternalIPAddress")
	if err != nil {
		return err
	}
	ip := net.ParseIP(v["NewExternalIPAddress"])
	if !publicIPv4(ip) {
		return errors.New("router WAN address is not public (possible CGNAT or double NAT)")
	}
	if m.external == 0 {
		m.external = m.port
	}
	for attempt := 0; attempt < 4; attempt++ {
		_, err = m.checkOwner(ctx)
		if err == nil {
			break
		}
		if !errors.Is(err, errMappingOwned) {
			return err
		}
		var b [2]byte
		if _, err = rand.Read(b[:]); err != nil {
			return err
		}
		m.external = 49152 + binary.BigEndian.Uint16(b[:])%16384
	}
	if err != nil {
		return err
	}
	lease := "3600"
	if m.permanent {
		lease = "0"
	}
	args := []soapArg{{"NewRemoteHost", ""}, {"NewExternalPort", strconv.Itoa(int(m.external))}, {"NewProtocol", "TCP"}, {"NewInternalPort", strconv.Itoa(int(m.port))}, {"NewInternalClient", m.local.String()}, {"NewEnabled", "1"}, {"NewPortMappingDescription", m.owner}, {"NewLeaseDuration", lease}}
	_, err = m.call(ctx, "AddPortMapping", args...)
	var fault *soapFault
	if errors.As(err, &fault) && fault.code == 725 {
		args[len(args)-1].value = "0"
		_, err = m.call(ctx, "AddPortMapping", args...)
		m.permanent = err == nil
	}
	if err != nil {
		return err
	}
	m.url = "https://" + net.JoinHostPort(ip.String(), strconv.Itoa(int(m.external)))
	return nil
}
func (m *upnpMapping) endpoint() string            { return m.url }
func (m *upnpMapping) refreshAfter() time.Duration { return 60 * time.Second }
func (m *upnpMapping) close(ctx context.Context) error {
	if m.external == 0 {
		return nil
	}
	owned, err := m.checkOwner(ctx)
	if err != nil || !owned {
		return err
	}
	_, err = m.call(ctx, "DeletePortMapping", soapArg{"NewRemoteHost", ""}, soapArg{"NewExternalPort", strconv.Itoa(int(m.external))}, soapArg{"NewProtocol", "TCP"})
	return err
}
