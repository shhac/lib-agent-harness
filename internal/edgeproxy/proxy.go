// Package edgeproxy provides the credentialed CONNECT transport for a harness
// edge. It is not a sandbox: callers must prove that direct egress is denied.
package edgeproxy

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const headerLimit = 8192

var (
	ErrPolicy      = errors.New("edge proxy: invalid destination policy")
	ErrUnavailable = errors.New("edge proxy: transport unavailable")
)

// Destination authorizes one exact DNS hostname and TCP port. Private permits
// RFC1918/ULA addresses only for an explicitly caller-authorized destination.
// Loopback, link-local, multicast and unspecified addresses are never admitted.
type Destination struct {
	Host    string
	Port    uint16
	Private bool
}

// ParseDestination accepts an exact DNS name, optionally followed by a port.
// It is shared by session normalization and listener policy construction.
func ParseDestination(value string, private bool) (Destination, error) {
	host, port := value, "443"
	if strings.Contains(value, ":") {
		var err error
		host, port, err = net.SplitHostPort(value)
		if err != nil {
			return Destination{}, ErrPolicy
		}
	}
	host, err := hostname(host)
	n, e := strconv.ParseUint(port, 10, 16)
	if err != nil || e != nil || n == 0 || strconv.FormatUint(n, 10) != port {
		return Destination{}, ErrPolicy
	}
	return Destination{Host: host, Port: uint16(n), Private: private}, nil
}

// Credential is secret material for session output scrubbing only.
func (p *Proxy) Credential() string { return p.credential }

// NewCanary constructs a disposable proxy whose sole special destination is
// a fake provider. The override is confined to this listener, with fresh auth,
// and cannot change any credentialed session's DNS or dial policy.
func NewCanary(destinations []Destination, authority string, fakeAddress string) (*Proxy, error) {
	d, err := ParseDestination(authority, false)
	if err != nil {
		return nil, ErrPolicy
	}
	if host, _, err := net.SplitHostPort(fakeAddress); err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return nil, ErrPolicy
	}
	destinations = append(append([]Destination(nil), destinations...), d)
	allowed := map[string]bool{}
	ports := map[string]bool{}
	for _, dest := range destinations {
		allowed[dest.Host] = true
		ports[strconv.Itoa(int(dest.Port))] = true
	}
	return newProxy(destinations, func(ctx context.Context, host string) ([]netip.Addr, error) {
		if allowed[host] {
			return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
		}
		return nil, ErrPolicy // canaries must never resolve or dial real providers
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil || host != "192.0.2.1" || !ports[port] {
			return nil, ErrPolicy
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, fakeAddress)
	})
}

// Proxy owns its listener and every accepted tunnel. Endpoint carries a secret:
// it belongs only in the child environment, never in persisted references.
type Proxy struct {
	listener    net.Listener
	server      *http.Server
	policy      map[string]bool
	credential  string
	ctx         context.Context
	cancel      context.CancelFunc
	resolve     func(context.Context, string) ([]netip.Addr, error)
	dial        func(context.Context, string, string) (net.Conn, error)
	idle        time.Duration
	mu          sync.Mutex
	closing     bool
	connections map[net.Conn]struct{}
	workers     sync.WaitGroup
	once        sync.Once
	done        chan struct{}
	failed      chan struct{}
	failOnce    sync.Once
	refusals    map[string]chan struct{}
}

// Refused subscribes to an authenticated policy refusal for an exact host.
// It carries no request text or credentials and is latched for this listener.
func (p *Proxy) Refused(host string) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refusals == nil {
		p.refusals = make(map[string]chan struct{})
	}
	if p.refusals[host] == nil {
		p.refusals[host] = make(chan struct{})
	}
	return p.refusals[host]
}

func (p *Proxy) recordRefusal(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch := p.refusals[host]; ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

// New opens a fresh loopback listener with a fresh per-launch credential. The
// policy is copied and validated before the listener is opened.
func New(destinations []Destination) (*Proxy, error) {
	return newProxy(destinations, nil, nil)
}

// Hooks are deliberately unexported. Tests resolve public synthetic names and
// route their already-validated IPs to a local fake, without external traffic.
func newProxy(destinations []Destination, resolve func(context.Context, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error), idleOverride ...time.Duration) (*Proxy, error) {
	policy := make(map[string]bool)
	for _, d := range destinations {
		host, err := hostname(d.Host)
		if err != nil || d.Port == 0 {
			return nil, ErrPolicy
		}
		key := net.JoinHostPort(host, strconv.Itoa(int(d.Port)))
		if old, exists := policy[key]; exists && old != d.Private {
			return nil, ErrPolicy
		}
		policy[key] = d.Private
	}
	if len(policy) == 0 {
		return nil, ErrPolicy
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, ErrUnavailable
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Proxy{listener: listener, policy: policy, credential: base64.RawURLEncoding.EncodeToString(secret), ctx: ctx, cancel: cancel, idle: time.Minute, connections: make(map[net.Conn]struct{}), done: make(chan struct{}), failed: make(chan struct{})}
	if len(idleOverride) > 0 {
		p.idle = idleOverride[0]
	}
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	p.resolve = resolve
	p.dial = dial
	if p.dial == nil {
		p.dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	p.server = &http.Server{Handler: http.HandlerFunc(p.serve), MaxHeaderBytes: headerLimit, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		err := p.server.Serve(listener)
		p.mu.Lock()
		unexpected := !p.closing && err != nil
		p.mu.Unlock()
		if unexpected {
			p.fail()
		}
	}()
	return p, nil
}

func hostname(s string) (string, error) {
	if len(s) == 0 || len(s) > 253 || strings.HasSuffix(s, ".") || net.ParseIP(s) != nil {
		return "", ErrPolicy
	}
	s = strings.ToLower(s)
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrPolicy
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", ErrPolicy
			}
		}
	}
	return s, nil
}

// Endpoint returns a credentialed proxy URL. It must be scrubbed from diagnostics.
func (p *Proxy) Endpoint() string {
	return (&url.URL{Scheme: "http", Host: p.listener.Addr().String(), User: url.UserPassword("edge", p.credential)}).String()
}

// Failed closes for an unexpected listener failure or handler panic. It never
// carries raw errors, addresses or credentials. Owners must stop the CLI on failure.
func (p *Proxy) Failed() <-chan struct{} { return p.failed }

func (p *Proxy) fail() {
	p.failOnce.Do(func() { close(p.failed); go p.Close() })
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	// Refused requests must not keep a connection alive or let net/http drain
	// an attacker-declared body before flushing the structural refusal.
	w.Header().Set("Connection", "close")
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		http.Error(w, "closed", http.StatusServiceUnavailable)
		return
	}
	p.workers.Add(1)
	p.mu.Unlock()
	defer p.workers.Done()
	defer func() {
		if recover() != nil {
			p.fail()
		}
	}()
	headerBytes := len(r.RequestURI) + len(r.Method) + len(r.Proto) + 8
	for name, values := range r.Header {
		for _, value := range values {
			headerBytes += len(name) + len(value) + 4
		}
	}
	if headerBytes > headerLimit {
		http.Error(w, "headers_refused", http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("edge:"+p.credential))
	credentials := r.Header.Values("Proxy-Authorization")
	if len(credentials) != 1 || subtle.ConstantTimeCompare([]byte(credentials[0]), []byte(expected)) != 1 {
		w.Header().Set("Proxy-Authenticate", `Basic realm="edge"`)
		http.Error(w, "credential_required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method != http.MethodConnect || r.ContentLength > 0 || len(r.TransferEncoding) != 0 || r.URL.Scheme != "" || r.URL.User != nil || r.URL.Path != "" || r.URL.RawQuery != "" || r.Host != r.RequestURI {
		http.Error(w, "connect_required", http.StatusForbidden)
		return
	}
	host, port, err := net.SplitHostPort(r.RequestURI)
	canonical, hostErr := hostname(host)
	n, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || hostErr != nil || portErr != nil || n == 0 || strconv.FormatUint(n, 10) != port {
		http.Error(w, "destination_refused", http.StatusForbidden)
		return
	}
	private, allowed := p.policy[net.JoinHostPort(canonical, port)]
	if !allowed {
		p.recordRefusal(canonical)
		http.Error(w, "destination_refused", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()
	ips, err := p.resolve(ctx, canonical)
	if err != nil || len(ips) == 0 {
		http.Error(w, "resolution_refused", http.StatusForbidden)
		return
	}
	// Refuse mixed answers rather than trying the public subset. Pin the dial
	// to a validated numeric IP so a second lookup cannot rebind the hostname.
	for _, ip := range ips {
		ip = ip.Unmap()
		if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || (!private && (ip.IsPrivate() || specialAddress(ip))) {
			http.Error(w, "address_refused", http.StatusForbidden)
			return
		}
	}
	var upstream net.Conn
	for _, ip := range ips {
		upstream, err = p.dial(ctx, "tcp", net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil || upstream == nil {
		http.Error(w, "upstream_unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "transport_unavailable", http.StatusServiceUnavailable)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return
	}
	p.connections[client] = struct{}{}
	p.connections[upstream] = struct{}{}
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.connections, client); delete(p.connections, upstream); p.mu.Unlock() }()
	activity := &tunnelActivity{connections: []net.Conn{client, upstream}, idle: p.idle}
	activity.touch()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	settled := make(chan struct{})
	go func() {
		defer func() {
			if recover() != nil {
				p.fail()
			}
			_ = upstream.Close()
			_ = client.Close()
			close(settled)
		}()
		_, _ = io.Copy(activeConn{upstream, activity}, activeReader{buffered, activity})
	}()
	// Settle both directions even if the handler's copy panics. Close must
	// never return while an admitted tunnel worker is still running.
	defer func() {
		_ = client.Close()
		_ = upstream.Close()
		<-settled
	}()
	_, _ = io.Copy(activeConn{client, activity}, activeConn{upstream, activity})
}

// These unicast ranges are not public provider destinations. Explicit caller
// hosts may use shared/private deployments; defaults must not reach them.
func specialAddress(ip netip.Addr) bool {
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "2001:db8::/32", "2001::/32", "2002::/16"} {
		if netip.MustParsePrefix(cidr).Contains(ip) {
			return true
		}
	}
	return false
}

// One shared deadline represents tunnel inactivity, not directional silence.
// Serializing refreshes prevents an older timestamp overwriting a newer one.
type tunnelActivity struct {
	mu          sync.Mutex
	connections []net.Conn
	idle        time.Duration
}

func (a *tunnelActivity) touch() {
	a.mu.Lock()
	defer a.mu.Unlock()
	deadline := time.Now().Add(a.idle)
	for _, c := range a.connections {
		_ = c.SetDeadline(deadline)
	}
}

type activeConn struct {
	net.Conn
	activity *tunnelActivity
}

func (c activeConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.activity.touch()
	}
	return n, err
}
func (c activeConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.activity.touch()
	}
	return n, err
}

type activeReader struct {
	io.Reader
	activity *tunnelActivity
}

func (r activeReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if n > 0 {
		r.activity.touch()
	}
	return n, err
}

// Close stops admission, cancels resolution/dials, closes hijacked tunnels and
// waits for every admitted handler to settle. Concurrent calls are safe.
func (p *Proxy) Close() {
	p.once.Do(func() {
		p.mu.Lock()
		p.closing = true
		p.cancel()
		for conn := range p.connections {
			_ = conn.Close()
		}
		p.mu.Unlock()
		_ = p.server.Close()
		_ = p.listener.Close()
		p.workers.Wait()
		close(p.done)
	})
	<-p.done
}
