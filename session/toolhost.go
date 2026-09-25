package session

// The tool channel's lifetime: its private listener, credential and lease, from
// creation to close. What it speaks is in toolhost_mcp.go, and whether a call
// may run is decided in toolhost_admission.go.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// toolHost owns the private listener and the tool channel's lifetime.
//
// Calls execute one at a time. A native turn will happily ask for several at
// once, and running them concurrently would make "after the work was reported"
// an ambiguous claim: a write or a test could still be in flight when a closing
// tool decides the assignment is finished, and cancelling it afterwards does not
// undo it. Serializing costs some parallelism and buys a boundary the caller can
// actually rely on, including for the evidence it collects.
type toolHost struct {
	cfg       ToolHost
	listener  net.Listener
	socket    string
	socketDir string
	secret    []byte
	lease     *os.File
	tools     map[string]ToolDefinition
	// gate serializes execution across every connection.
	gate chan struct{}

	mu      sync.Mutex
	closed  bool
	probing bool
	stopped bool
	running int
	// pending holds every admitted call, queued or executing, keyed by the
	// connection and request that asked for it.
	pending map[string]*hostedCall
	// paused suspends admission without ending the work, which is what an
	// interrupt needs; closed ends it, which is what a closing tool does.
	paused bool
	// generation moves when the channel reopens, so a call queued before a pause
	// cannot run against the work that follows it.
	generation uint64
	// connections numbers harness connections, because request identifiers are
	// only unique within one.
	connections uint64
	// settled is closed exactly while nothing is outstanding. A caller that has
	// paused the channel waits on it before describing the workspace, because a
	// cancelled call is not a stopped one until its handler has actually returned.
	settled chan struct{}
	// listed records that a harness connected, authenticated and asked for this
	// session's tools. Where a harness defers MCP tools and never puts them in a
	// request, this is the positive evidence that they were actually offered.
	listed bool

	// Wiring supplied by the session that owns this host. Both are nil for a
	// host serving a capability probe, which has no session and no turn.
	onRefusal  func(tool, reason string)
	activeTurn func() string
	done       chan struct{}
	wg         sync.WaitGroup
}

// newToolHost opens the tool channel under the assignment lease. A caller that
// already holds the lease passes it, and the host owns it from then on; a nil
// lease is taken here. Either way, a host that fails to open releases it.
func newToolHost(cfg ToolHost, lease *os.File) (_ *toolHost, err error) {
	defer func() {
		if err != nil {
			_ = lease.Close()
		}
	}()
	if err = cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.MaxResultBytes <= 0 {
		cfg.MaxResultBytes = 64 << 10
	}
	// A local socket path has a hard length limit far below what an ordinary
	// application state directory reaches, so the listener gets its own short
	// owner-only directory. The durable files — the channel credential and the
	// bridge lock that outlives a crash — stay in the caller's directory.
	socketDir, err := shortPrivateDir()
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(socketDir, "t.sock")
	settled := make(chan struct{})
	close(settled)
	// The assignment lease is taken before anything is launched, so a second
	// process cannot drive this assignment during the window before a bridge
	// exists. It is released when the host closes, or by the operating system if
	// this process dies.
	if lease == nil {
		if lease, err = holdLease(filepath.Join(cfg.Dir, "session.lease")); err != nil {
			_ = os.RemoveAll(socketDir)
			return nil, err
		}
	}
	h := &toolHost{cfg: cfg, socket: socket, socketDir: socketDir, lease: lease, tools: map[string]ToolDefinition{}, pending: map[string]*hostedCall{}, gate: make(chan struct{}, 1), done: make(chan struct{}), settled: settled}
	for _, t := range cfg.Tools {
		h.tools[t.Name] = t
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, errors.New("tool host credential unavailable")
	}
	h.secret = []byte(hex.EncodeToString(secret[:]))
	if err = writePrivate(filepath.Join(cfg.Dir, "t.secret"), h.secret); err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.RemoveAll(socketDir)
		return nil, errors.New("tool host could not open its private channel")
	}
	if err = os.Chmod(socket, 0600); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(socketDir)
		return nil, errors.New("tool host could not restrict its private channel")
	}
	h.listener = listener
	h.wg.Add(1)
	go h.accept()
	return h, nil
}

// environment names the channel for the bridge process. Only paths travel here.
func (h *toolHost) environment() map[string]string {
	return map[string]string{
		BridgeSocketEnv: h.socket,
		BridgeSecretEnv: filepath.Join(h.cfg.Dir, "t.secret"),
		BridgeLockEnv:   filepath.Join(h.cfg.Dir, "bridge.lock"),
	}
}

func (h *toolHost) close() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	lease := h.lease
	h.lease = nil
	h.mu.Unlock()
	close(h.done)
	_ = h.listener.Close()
	h.wg.Wait()
	if lease != nil {
		_ = lease.Close()
	}
	_ = os.RemoveAll(h.socketDir)
}

// setProbing refuses every call for the duration of a capability probe. The
// probe's provider rejects inference, so no model can ask for one; this makes
// that a property of the host rather than an assumption about the provider.
func (h *toolHost) setProbing(on bool) {
	h.mu.Lock()
	h.probing = on
	h.mu.Unlock()
}

// served reports that the tool channel has handed this session's tools to a
// harness. It says the surface exists and was reachable, nothing more.
func (h *toolHost) served() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listed
}

func (h *toolHost) channelClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

func (h *toolHost) accept() {
	defer h.wg.Done()
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			return
		}
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			defer conn.Close()
			h.serve(conn)
		}()
	}
}
