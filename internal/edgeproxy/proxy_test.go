package edgeproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestRefusalObservationRequiresAuthentication(t *testing.T) {
	p := &Proxy{policy: map[string]bool{"api.openai.com:443": false}, credential: "dummy-secret"}
	refused := p.Refused("auth.openai.com")
	for _, authenticated := range []bool{false, true, true} {
		r := &http.Request{Method: http.MethodConnect, RequestURI: "auth.openai.com:443", Host: "auth.openai.com:443", URL: &url.URL{Host: "auth.openai.com:443"}, Header: make(http.Header)}
		if authenticated {
			r.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("edge:"+p.Credential())))
		}
		w := httptest.NewRecorder()
		p.serve(w, r)
		want := http.StatusProxyAuthRequired
		if authenticated {
			want = http.StatusForbidden
		}
		if w.Code != want {
			t.Fatal("wrong refusal", w.Code)
		}
		select {
		case <-refused:
			if !authenticated {
				t.Fatal("untrusted process triggered refresh fact")
			}
		default:
			if authenticated {
				t.Fatal("authenticated refresh refusal not latched")
			}
		}
	}
}

func fakeProxy(t *testing.T, destinations []Destination, ips ...string) (*Proxy, *atomic.Int32) {
	t.Helper()
	testenv.RequireLoopback(t)
	var dials atomic.Int32
	addresses := make([]netip.Addr, len(ips))
	for i, ip := range ips {
		addresses[i] = netip.MustParseAddr(ip)
	}
	p, err := newProxy(destinations, func(context.Context, string) ([]netip.Addr, error) { return addresses, nil }, func(_ context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if network != "tcp" || address != net.JoinHostPort(addresses[0].Unmap().String(), "443") {
			return nil, fmt.Errorf("unexpected pinned destination")
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			b := make([]byte, 4)
			if _, err := io.ReadFull(server, b); err == nil {
				_, _ = server.Write(b)
			}
			_, _ = io.Copy(io.Discard, server)
		}()
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, &dials
}

func TestDefaultHostsRefuseSharedSpecialRanges(t *testing.T) {
	for _, ip := range []string{"100.64.0.1", "100.127.255.254", "198.18.0.1", "198.19.255.254"} {
		p, dials := fakeProxy(t, []Destination{{Host: "provider.example", Port: 443}}, ip)
		endpoint, _ := url.Parse(p.Endpoint())
		conn, err := net.Dial("tcp", endpoint.Host)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "CONNECT provider.example:443 HTTP/1.1\r\nHost: provider.example:443\r\nProxy-Authorization: Basic %s\r\n\r\n", base64.StdEncoding.EncodeToString([]byte("edge:"+p.Credential())))
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		conn.Close()
		if err != nil || response.StatusCode != 403 || dials.Load() != 0 {
			t.Fatal("special destination admitted", ip, err)
		}
	}
}

func TestOneWayProviderStreamKeepsTunnelAlive(t *testing.T) {
	testenv.RequireLoopback(t)
	idle := 300 * time.Millisecond
	p, err := newProxy([]Destination{{Host: "provider.example", Port: 443}}, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.8")}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			for range 15 {
				time.Sleep(60 * time.Millisecond)
				if _, err := server.Write([]byte("x")); err != nil {
					return
				}
			}
		}()
		return client, nil
	}, idle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	conn, reader, response := request(t, p, p.credential, "CONNECT", "provider.example:443", "")
	defer conn.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	bytes := make([]byte, 15)
	if _, err := io.ReadFull(reader, bytes); err != nil {
		t.Fatalf("one-way stream cut off: %v", err)
	}
}

func TestValidatedAddressFallback(t *testing.T) {
	testenv.RequireLoopback(t)
	var dials atomic.Int32
	p, err := newProxy([]Destination{{Host: "provider.example", Port: 443}}, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("2606:4700::1111"), netip.MustParseAddr("203.0.113.8")}, nil
	}, func(_ context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if address == "[2606:4700::1111]:443" {
			return nil, errors.New("unreachable synthetic family")
		}
		if address != "203.0.113.8:443" {
			t.Error("unvalidated dial", address)
		}
		client, server := net.Pipe()
		go func() { defer server.Close(); _, _ = io.Copy(io.Discard, server) }()
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	conn, _, response := request(t, p, p.credential, "CONNECT", "provider.example:443", "")
	defer conn.Close()
	if response.StatusCode != 200 || dials.Load() != 2 {
		t.Fatalf("fallback: %d %d", response.StatusCode, dials.Load())
	}
}

func request(t *testing.T, p *Proxy, credential, method, target, extra string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", p.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	auth := ""
	if credential != "" {
		auth = "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("edge:"+credential)) + "\r\n"
	}
	host := target
	if absolute, err := url.Parse(target); err == nil && absolute.Host != "" {
		host = absolute.Host
	}
	if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: %s\r\n%s%s\r\n", method, target, host, auth, extra); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	response, err := http.ReadResponse(r, &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	return conn, r, response
}

func TestTunnelAndClose(t *testing.T) {
	p, dials := fakeProxy(t, []Destination{{Host: "provider.example", Port: 443}}, "203.0.113.8")
	conn, reader, response := request(t, p, p.credential, "CONNECT", "provider.example:443", "")
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	var b [4]byte
	if _, err := io.ReadFull(reader, b[:]); err != nil || string(b[:]) != "ping" {
		t.Fatalf("tunnel: %q %v", b, err)
	}
	if dials.Load() != 1 {
		t.Fatal("missing upstream dial")
	}
	p.Close()
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("tunnel survived close")
	}
	if conn, err := net.DialTimeout("tcp", p.listener.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("listener survived close")
	}
	select {
	case <-p.Failed():
		t.Fatal("normal close reported failure")
	default:
	}
}

func TestRequestRefusals(t *testing.T) {
	p, dials := fakeProxy(t, []Destination{{Host: "provider.example", Port: 443}}, "203.0.113.8")
	for _, tc := range []struct {
		name, credential, method, target, extra string
		status                                  int
	}{
		{"missing", "", "CONNECT", "provider.example:443", "", 407},
		{"wrong", "wrong", "CONNECT", "provider.example:443", "", 407},
		{"host", p.credential, "CONNECT", "other.example:443", "", 403},
		{"port", p.credential, "CONNECT", "provider.example:80", "", 403},
		{"port spelling", p.credential, "CONNECT", "provider.example:0443", "", 403},
		{"wildcard", p.credential, "CONNECT", "*.example:443", "", 403},
		{"get", p.credential, "GET", "http://provider.example:443/", "", 403},
		{"body", p.credential, "CONNECT", "provider.example:443", "Content-Length: 1\r\n", 403},
		{"duplicate auth", p.credential, "CONNECT", "provider.example:443", "Proxy-Authorization: wrong\r\n", 407},
		{"headers", p.credential, "CONNECT", "provider.example:443", "X-Large: " + strings.Repeat("a", headerLimit) + "\r\n", 431},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, response := request(t, p, tc.credential, tc.method, tc.target, tc.extra)
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("status %d, want %d", response.StatusCode, tc.status)
			}
			body, _ := io.ReadAll(response.Body)
			if strings.Contains(string(body), p.credential) || strings.Contains(string(body), tc.target) {
				t.Fatal("refusal leaked secret or destination")
			}
		})
	}
	if dials.Load() != 0 {
		t.Fatal("refused request reached upstream")
	}
}

func TestResolvedAddresses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ips     []string
		private bool
		status  int
	}{
		{"public", []string{"203.0.113.8"}, false, 200},
		{"private denied", []string{"10.0.0.2"}, false, 403},
		{"private explicit", []string{"10.0.0.2"}, true, 200},
		{"ula denied", []string{"fd00::2"}, false, 403},
		{"ula explicit", []string{"fd00::2"}, true, 200},
		{"loopback", []string{"127.0.0.1"}, true, 403},
		{"mapped loopback", []string{"::ffff:127.0.0.1"}, true, 403},
		{"ipv6 loopback", []string{"::1"}, true, 403},
		{"link local", []string{"169.254.1.1"}, true, 403},
		{"v6 link local", []string{"fe80::1"}, true, 403},
		{"unspecified", []string{"0.0.0.0"}, true, 403},
		{"v6 unspecified", []string{"::"}, true, 403},
		{"multicast", []string{"224.0.0.1"}, true, 403},
		{"mixed", []string{"203.0.113.8", "127.0.0.1"}, true, 403},
		{"empty", nil, false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, dials := fakeProxy(t, []Destination{{Host: "provider.example", Port: 443, Private: tc.private}}, tc.ips...)
			conn, _, response := request(t, p, p.credential, "CONNECT", "provider.example:443", "")
			defer conn.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("status %d, want %d", response.StatusCode, tc.status)
			}
			if tc.status == 403 && dials.Load() != 0 {
				t.Fatal("denied address dialed")
			}
		})
	}
}

func TestInvalidPolicy(t *testing.T) {
	for _, host := range []string{"", "*.example", "provider.example:443", "http://provider.example", "a..b", "-a.example", "a-.example", "a.example.", "a/b", "a@b", "127.0.0.1", strings.Repeat("a", 64) + ".example"} {
		if p, err := New([]Destination{{Host: host, Port: 443}}); err != ErrPolicy {
			if p != nil {
				p.Close()
			}
			t.Errorf("host %q: %v", host, err)
		}
	}
	for _, policy := range [][]Destination{nil, {{Host: "provider.example"}}, {{Host: "provider.example", Port: 443}, {Host: "provider.example", Port: 443, Private: true}}} {
		if p, err := New(policy); err != ErrPolicy {
			if p != nil {
				p.Close()
			}
			t.Fatal("invalid policy accepted")
		}
	}
}

func TestCrossSessionCredentials(t *testing.T) {
	a, _ := fakeProxy(t, []Destination{{Host: "provider.example", Port: 443}}, "203.0.113.8")
	b, dials := fakeProxy(t, []Destination{{Host: "provider.example", Port: 443}}, "203.0.113.8")
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			conn, _, response := request(t, b, a.credential, "CONNECT", "provider.example:443", "")
			defer conn.Close()
			defer response.Body.Close()
			if response.StatusCode != 407 {
				t.Errorf("cross-session status %d", response.StatusCode)
			}
		})
	}
	workers.Wait()
	if dials.Load() != 0 {
		t.Fatal("cross-session request dialed")
	}
	for range 16 {
		workers.Go(b.Close)
	}
	workers.Wait()
}

func TestUnexpectedListenerFailure(t *testing.T) {
	p, _ := fakeProxy(t, []Destination{{Host: "provider.example", Port: 443}}, "203.0.113.8")
	_ = p.listener.Close()
	select {
	case <-p.Failed():
	case <-time.After(3 * time.Second):
		t.Fatal("listener failure not reported")
	}
	p.Close()
}

func TestCloseCancelsAdmittedResolution(t *testing.T) {
	testenv.RequireLoopback(t)
	entered := make(chan struct{})
	settled := make(chan struct{})
	p, err := newProxy([]Destination{{Host: "provider.example", Port: 443}}, func(ctx context.Context, _ string) ([]netip.Addr, error) {
		close(entered)
		<-ctx.Done()
		close(settled)
		return nil, ctx.Err()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	conn, err := net.DialTimeout("tcp", p.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte("edge:" + p.credential))
	_, err = fmt.Fprintf(conn, "CONNECT provider.example:443 HTTP/1.1\r\nHost: provider.example:443\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("resolution not admitted")
	}
	p.Close()
	select {
	case <-settled:
	default:
		t.Fatal("close returned before resolver settled")
	}
}

func TestHandlerPanicFailsClosed(t *testing.T) {
	testenv.RequireLoopback(t)
	p, err := newProxy([]Destination{{Host: "provider.example", Port: 443}}, func(context.Context, string) ([]netip.Addr, error) { panic("synthetic credential must not be logged") }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	conn, err := net.DialTimeout("tcp", p.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte("edge:" + p.credential))
	if _, err := fmt.Fprintf(conn, "CONNECT provider.example:443 HTTP/1.1\r\nHost: provider.example:443\r\nProxy-Authorization: Basic %s\r\n\r\n", auth); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Failed():
	case <-time.After(3 * time.Second):
		t.Fatal("panic not reported")
	}
	p.Close()
}

func TestHostnameValidation(t *testing.T) {
	for _, host := range []string{"provider.example", "PROVIDER.EXAMPLE", "a-b.example"} {
		got, err := hostname(host)
		if err != nil || got != strings.ToLower(host) {
			t.Fatalf("hostname %q: %q %v", host, got, err)
		}
	}
	for _, host := range []string{"*.example", "a..b", "a.example.", "127.0.0.1", "[::1]", "a/b", "a\n.example", "a@b", "-a.example", "a-.example"} {
		if _, err := hostname(host); err != ErrPolicy {
			t.Fatalf("accepted %q", host)
		}
	}
}

type panicWriteConn struct{ net.Conn }

func (panicWriteConn) Write([]byte) (int, error) { panic("synthetic tunnel failure") }

func TestTunnelWorkerPanicFailsClosed(t *testing.T) {
	testenv.RequireLoopback(t)
	p, err := newProxy([]Destination{{Host: "provider.example", Port: 443}}, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.8")}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() { defer server.Close(); _, _ = io.Copy(io.Discard, server) }()
		return panicWriteConn{client}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	conn, _, response := request(t, p, p.credential, "CONNECT", "provider.example:443", "")
	defer conn.Close()
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	_, _ = conn.Write([]byte("trigger"))
	select {
	case <-p.Failed():
	case <-time.After(3 * time.Second):
		t.Fatal("tunnel panic not reported")
	}
	p.Close()
}

func TestIdleTunnelSettles(t *testing.T) {
	p, _ := fakeProxy(t, []Destination{{Host: "provider.example", Port: 443}}, "203.0.113.8")
	p.idle = 20 * time.Millisecond
	conn, reader, response := request(t, p, p.credential, "CONNECT", "provider.example:443", "")
	defer conn.Close()
	if response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("idle tunnel remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("only client deadline expired")
	}
	p.Close()
}

func TestPolicyIsFrozen(t *testing.T) {
	destinations := []Destination{{Host: "provider.example", Port: 443}}
	p, dials := fakeProxy(t, destinations, "10.0.0.2")
	destinations[0].Private = true
	_, _, response := request(t, p, p.credential, "CONNECT", "provider.example:443", "")
	defer response.Body.Close()
	if response.StatusCode != 403 || dials.Load() != 0 {
		t.Fatal("caller mutation widened policy")
	}
}
