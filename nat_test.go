// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 DoucheSync contributors

package main

import (
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublicWANValidation(t *testing.T) {
	for _, s := range []string{"10.0.0.1", "192.168.1.1", "172.16.1.1", "100.64.1.1", "127.0.0.1", "169.254.1.1", "0.1.2.3", "203.0.113.1", "224.0.0.1", "::1", "2001:db8::1"} {
		if publicIPv4(net.ParseIP(s)) {
			t.Fatalf("non-public WAN accepted: %s", s)
		}
	}
	if !publicIPv4(net.ParseIP("8.8.8.8")) {
		t.Fatal("public address rejected")
	}
}

func fakePMP(t *testing.T, wan net.IP, invalid bool) (*net.UDPAddr, <-chan []byte) {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan []byte, 20)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			b := make([]byte, 64)
			n, from, err := c.ReadFromUDP(b)
			if err != nil {
				return
			}
			b = b[:n]
			requests <- append([]byte(nil), b...)
			var response []byte
			if n == 2 && b[0] == 0 && b[1] == 0 {
				response = make([]byte, 12)
				response[1] = 128
				copy(response[8:], wan.To4())
			} else if n == 12 && b[0] == 0 && b[1] == 2 {
				response = make([]byte, 16)
				response[1] = 130
				copy(response[8:10], b[4:6])
				copy(response[12:16], b[8:12])
				if binary.BigEndian.Uint32(b[8:12]) != 0 {
					binary.BigEndian.PutUint16(response[10:12], 45000)
				}
			} else {
				t.Errorf("unexpected PMP request: %v", b)
				return
			}
			if invalid {
				response[1] = 129
			}
			c.WriteToUDP(response, from)
		}
	}()
	t.Cleanup(func() { c.Close(); <-done })
	return c.LocalAddr().(*net.UDPAddr), requests
}

func TestNATPMPMappingRenewalAndCleanup(t *testing.T) {
	gw, requests := fakePMP(t, net.ParseIP("8.8.8.8"), false)
	m := &pmpMapping{local: net.ParseIP("127.0.0.1"), gateway: gw, port: 7444}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.renew(ctx); err != nil {
		t.Fatal(err)
	}
	if m.endpoint() != "https://8.8.8.8:45000" || m.refreshAfter() != 30*time.Minute {
		t.Fatal("mapping response not applied")
	}
	if err := m.renew(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.close(ctx); err != nil {
		t.Fatal(err)
	}
	var mappingRequests [][]byte
	for len(requests) > 0 {
		b := <-requests
		if len(b) == 12 {
			mappingRequests = append(mappingRequests, b)
		}
	}
	if len(mappingRequests) != 3 {
		t.Fatalf("got %d map operations", len(mappingRequests))
	}
	first, last := mappingRequests[0], mappingRequests[2]
	if first[1] != 2 || binary.BigEndian.Uint16(first[4:6]) != 7444 || binary.BigEndian.Uint32(first[8:12]) != 3600 {
		t.Fatal("incorrect TCP mapping request")
	}
	if binary.BigEndian.Uint16(last[4:6]) != 7444 || binary.BigEndian.Uint16(last[6:8]) != 0 || binary.BigEndian.Uint32(last[8:12]) != 0 {
		t.Fatal("cleanup was not specific to the client port")
	}
}

func TestNATPMPRejectsPrivateWANAndBadResponses(t *testing.T) {
	for _, tc := range []struct {
		wan     string
		invalid bool
	}{{"100.64.1.1", false}, {"8.8.8.8", true}} {
		gw, requests := fakePMP(t, net.ParseIP(tc.wan), tc.invalid)
		m := &pmpMapping{local: net.ParseIP("127.0.0.1"), gateway: gw, port: 7444}
		if err := m.renew(context.Background()); err == nil {
			t.Fatal("bad gateway accepted")
		}
		if len(requests) != 1 {
			t.Fatal("opened a mapping after an invalid WAN response")
		}
	}
}

type routerFixture struct {
	server    *httptest.Server
	mu        sync.Mutex
	ports     map[string]map[string]string
	permanent bool
	multiple  bool
	actions   []string
}

func soapValues(t *testing.T, body io.Reader) map[string]string {
	t.Helper()
	d := xml.NewDecoder(io.LimitReader(body, 8192))
	values := map[string]string{}
	var stack []string
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Error(err)
			return values
		}
		switch x := token.(type) {
		case xml.StartElement:
			stack = append(stack, x.Name.Local)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				values[stack[len(stack)-1]] += string(x)
			}
		}
	}
	return values
}

func fakeUPnP(t *testing.T, permanent bool) *routerFixture {
	t.Helper()
	f := &routerFixture{ports: map[string]map[string]string{}, permanent: permanent}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/desc.xml" {
			f.mu.Lock()
			multiple := f.multiple
			f.mu.Unlock()
			fmt.Fprint(w, `<root><device><deviceList><device><serviceList>`)
			if multiple {
				fmt.Fprint(w, `<service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><controlURL>/disabled</controlURL></service>`)
			}
			fmt.Fprint(w, `<service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><controlURL>/control</controlURL></service></serviceList></device></deviceList></device></root>`)
			return
		}
		if r.URL.Path == "/disabled" {
			w.WriteHeader(500)
			fmt.Fprint(w, `<Envelope><Body><Fault><detail><UPnPError><errorCode>701</errorCode><errorDescription>disabled WAN</errorDescription></UPnPError></detail></Fault></Body></Envelope>`)
			return
		}
		if r.URL.Path != "/control" || r.Method != http.MethodPost {
			t.Errorf("unexpected router request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
			return
		}
		action := strings.Trim(r.Header.Get("SOAPAction"), `"`)
		action = action[strings.LastIndex(action, "#")+1:]
		args := soapValues(t, r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.actions = append(f.actions, action)
		fault := func(code int) {
			w.WriteHeader(500)
			fmt.Fprintf(w, `<Envelope><Body><Fault><detail><UPnPError><errorCode>%d</errorCode><errorDescription>router fault</errorDescription></UPnPError></detail></Fault></Body></Envelope>`, code)
		}
		response := func(v map[string]string) {
			fmt.Fprintf(w, "<Envelope><Body><%sResponse>", action)
			for k, x := range v {
				fmt.Fprintf(w, "<%s>", k)
				xml.EscapeText(w, []byte(x))
				fmt.Fprintf(w, "</%s>", k)
			}
			fmt.Fprintf(w, "</%sResponse></Body></Envelope>", action)
		}
		switch action {
		case "GetExternalIPAddress":
			response(map[string]string{"NewExternalIPAddress": "8.8.8.8"})
		case "GetSpecificPortMappingEntry":
			v, ok := f.ports[args["NewExternalPort"]]
			if !ok {
				fault(714)
			} else {
				response(v)
			}
		case "AddPortMapping":
			if args["NewProtocol"] != "TCP" || args["NewInternalClient"] != "127.0.0.1" || args["NewInternalPort"] != "7444" || args["NewEnabled"] != "1" {
				t.Errorf("invalid AddPortMapping: %v", args)
			}
			if f.permanent && args["NewLeaseDuration"] != "0" {
				fault(725)
				return
			}
			f.ports[args["NewExternalPort"]] = args
			response(nil)
		case "DeletePortMapping":
			delete(f.ports, args["NewExternalPort"])
			response(nil)
		default:
			t.Errorf("unexpected SOAP action %s", action)
			fault(401)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestUPnPDiscoveryMappingAndCleanup(t *testing.T) {
	f := fakeUPnP(t, false)
	f.mu.Lock()
	f.multiple = true
	f.mu.Unlock()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 2048)
		n, from, e := udp.ReadFromUDP(b)
		if e != nil {
			return
		}
		if !strings.Contains(string(b[:n]), "M-SEARCH") {
			t.Error("missing discovery request")
		}
		udp.WriteToUDP([]byte("HTTP/1.1 200 OK\r\nLOCATION: "+f.server.URL+"/desc.xml\r\nContent-Length: 0\r\n\r\n"), from)
	}()
	t.Cleanup(func() { udp.Close(); <-done })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := discoverUPnP(ctx, net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1"), 7444, "DoucheSync-test", []*net.UDPAddr{udp.LocalAddr().(*net.UDPAddr)})
	if err != nil {
		t.Fatal(err)
	}
	if m.endpoint() != "https://8.8.8.8:7444" {
		t.Fatal(m.endpoint())
	}
	if err = m.renew(ctx); err != nil {
		t.Fatal(err)
	}
	if err = m.close(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ports) != 0 {
		t.Fatal("mapping not deleted")
	}
}

func TestUPnPPermanentLeaseAndOwnership(t *testing.T) {
	f := fakeUPnP(t, true)
	f.ports["7444"] = map[string]string{"NewInternalClient": "127.0.0.2", "NewInternalPort": "7444", "NewPortMappingDescription": "another app"}
	mappings, err := loadUPnP(context.Background(), routerHTTP(net.ParseIP("127.0.0.1")), f.server.URL+"/desc.xml", net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1"), 7444, "DoucheSync-owned")
	if err != nil {
		t.Fatal(err)
	}
	m := mappings[0]
	if err = m.renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !m.permanent || m.external == 7444 {
		t.Fatal("did not handle permanent-only router or occupied port")
	}
	f.mu.Lock()
	f.ports[strconv.Itoa(int(m.external))]["NewPortMappingDescription"] = "replaced by another app"
	f.mu.Unlock()
	if err = m.close(context.Background()); err == nil {
		t.Fatal("deleted a mapping belonging to another app")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ports) != 2 {
		t.Fatal("existing mappings modified")
	}
}

func TestUPnPRejectsOffRouterURLsAndRedirects(t *testing.T) {
	for _, raw := range []string{"http://8.8.8.8/desc.xml", "http://user:password@127.0.0.1/desc.xml", "file:///etc/passwd", "http://127.0.0.1/desc.xml#fragment"} {
		if _, err := routerURL(raw, net.ParseIP("127.0.0.1")); err == nil {
			t.Fatal("unsafe router URL accepted")
		}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.2/elsewhere", 302) }))
	defer s.Close()
	if _, err := loadUPnP(context.Background(), routerHTTP(net.ParseIP("127.0.0.1")), s.URL, net.ParseIP("127.0.0.1"), net.ParseIP("127.0.0.1"), 7444, "test"); err == nil {
		t.Fatal("router redirect followed")
	}
}

type fakeMapping struct{ renewals, deletions int }

func (m *fakeMapping) renew(context.Context) error { m.renewals++; return nil }
func (m *fakeMapping) close(context.Context) error { m.deletions++; return nil }
func (m *fakeMapping) endpoint() string            { return "https://8.8.8.8:45000" }
func (m *fakeMapping) refreshAfter() time.Duration { return time.Minute }

func TestNATManagerRenewalNetworkChangeAndRetry(t *testing.T) {
	created := 0
	var mappings []*fakeMapping
	m := &natManager{create: func(context.Context, net.IP, uint16, string) (portMapping, error) {
		created++
		if created == 1 {
			return nil, errors.New("temporarily unavailable")
		}
		p := &fakeMapping{}
		mappings = append(mappings, p)
		return p, nil
	}}
	ctx := context.Background()
	if _, err := m.ensure(ctx, "https://10.0.0.2:7444"); err == nil {
		t.Fatal("failure hidden")
	}
	if _, err := m.ensure(ctx, "https://10.0.0.2:7444"); err == nil || created != 1 {
		t.Fatal("retry interval ignored")
	}
	m.next = time.Time{}
	if _, err := m.ensure(ctx, "https://10.0.0.2:7444"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ensure(ctx, "https://10.0.0.2:7444"); err != nil || created != 2 {
		t.Fatal("active mapping not cached")
	}
	m.next = time.Time{}
	if _, err := m.ensure(ctx, "https://10.0.0.2:7444"); err != nil || mappings[0].renewals != 1 {
		t.Fatal("mapping not renewed")
	}
	if _, err := m.ensure(ctx, "https://10.0.0.3:7444"); err != nil || mappings[0].deletions != 1 || created != 3 {
		t.Fatal("network change not handled")
	}
	if err := m.close(ctx); err != nil || mappings[1].deletions != 1 {
		t.Fatal("cleanup failed")
	}
}
