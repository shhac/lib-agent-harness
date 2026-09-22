package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Tool hosting lets a caller give a native session a tool surface it implements
// itself. The library serves the protocol and owns the channel's lifetime; it
// never executes a tool's effect, and it never reads a file, opens a socket or
// starts a container on a tool's behalf.

// Environment variables naming the private tool channel for a bridge process.
// They carry paths, never secret bytes: the credential lives in an owner-only
// file so it cannot be read out of a process listing.
const (
	BridgeSocketEnv = "AGENT_HARNESS_TOOL_SOCKET"
	BridgeSecretEnv = "AGENT_HARNESS_TOOL_SECRET_FILE"
	BridgeLockEnv   = "AGENT_HARNESS_TOOL_LOCK_FILE"
)

// MaxToolRequestBytes bounds one protocol frame arriving from the harness.
const MaxToolRequestBytes = 1 << 20

var (
	// ErrToolsClosed reports a tool call made after the channel was closed by a
	// closing tool. Nothing was executed.
	ErrToolsClosed = errors.New("harness tool channel is closed")
	// ErrToolUnknown reports a call naming a tool this session does not host.
	ErrToolUnknown = errors.New("harness tool is not hosted by this session")
)

// ToolDefinition describes one caller-implemented tool. Schema must be a JSON
// Schema object describing the tool's arguments.
//
// Closing marks a tool that ends the session's work — reporting completion,
// asking for a decision, handing off to another party. Because calls execute
// one at a time, a closing tool runs with nothing else in flight, and the
// channel latches shut only if the call actually succeeded: a rejected finish
// has not finished anything.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
	Closing     bool           `json:"closing,omitempty"`
}

// ToolCall is one harness request for a caller-implemented tool. Arguments is
// the raw JSON object the model supplied and is untrusted input.
type ToolCall struct {
	TurnID    string
	Name      string
	Arguments json.RawMessage
}

// ToolResult is what the model sees. Content is truncated to the host's limit
// with an explicit marker. Closes latches the channel shut for a tool whose
// closing behaviour depends on its arguments rather than its name. Neither
// Closes nor a Closing definition latches when IsError is set: a refusal is not
// a completion.
type ToolResult struct {
	Content string
	IsError bool
	Closes  bool
}

type ToolHandler interface {
	CallTool(context.Context, ToolCall) (ToolResult, error)
}
type ToolHandlerFunc func(context.Context, ToolCall) (ToolResult, error)

func (f ToolHandlerFunc) CallTool(ctx context.Context, c ToolCall) (ToolResult, error) {
	return f(ctx, c)
}

// Bridge is the caller's own command, re-executed by the harness as its tool
// server. Its only job is to relay the harness's protocol stream to this
// library's listener; session.RunBridge implements exactly that. The library
// never searches PATH for one and never substitutes a shell.
type Bridge struct {
	Path string
	Args []string
}

// ToolHost configures the tool channel. Dir must be an existing owner-only
// directory in private application state: it holds the listener, the channel
// credential and the bridge lock, and it must not be inside anything a tool can
// reach.
type ToolHost struct {
	Server         string
	Tools          []ToolDefinition
	Handler        ToolHandler
	Dir            string
	Bridge         Bridge
	MaxResultBytes int
}

// Qualified returns the identifiers a harness exposes these tools under. They
// are what a restricted session's expected tool surface is compared against.
func (h ToolHost) Qualified() []string {
	names := make([]string, 0, len(h.Tools))
	for _, t := range h.Tools {
		names = append(names, "mcp__"+h.Server+"__"+t.Name)
	}
	return names
}

func validToolName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func (h ToolHost) validate() error {
	if !validToolName(h.Server) {
		return errors.New("tool host server name must be a short alphanumeric identifier")
	}
	if h.Handler == nil {
		return errors.New("tool host requires a handler")
	}
	if len(h.Tools) == 0 || len(h.Tools) > 64 {
		return errors.New("tool host requires between 1 and 64 tools")
	}
	seen := map[string]bool{}
	for _, t := range h.Tools {
		if !validToolName(t.Name) || seen[t.Name] {
			return errors.New("tool host names must be unique short alphanumeric identifiers")
		}
		if t.Schema == nil {
			return errors.New("tool host requires an argument schema for every tool")
		}
		seen[t.Name] = true
	}
	if h.Bridge.Path == "" || !filepath.IsAbs(h.Bridge.Path) {
		return errors.New("tool host requires an absolute bridge command path")
	}
	if h.Dir == "" || !filepath.IsAbs(h.Dir) {
		return errors.New("tool host requires an absolute private directory")
	}
	info, err := os.Stat(h.Dir)
	if err != nil || !info.IsDir() {
		return errors.New("tool host directory must exist")
	}
	if err = ownerOnly(info); err != nil {
		return err
	}
	return nil
}

// CancelTools pauses the tool channel and stops everything it has admitted —
// running and queued alike — so the caller can describe the workspace.
//
// Interrupting a native turn does not reach a caller's tools: measured on both
// installed harnesses, a terminal interrupted result arrived while a hosted
// call was still running. Cancelling only what happened to be executing left
// the queue behind it to run afterwards, which is a write arriving after the
// work was reported as stopped. So this closes admission too, and the channel
// stays closed until the caller opens it for new work.
//
// This is not the same as the channel being finished. A closing tool ends the
// work; a pause suspends it, and the assignment can continue afterwards.
func (s *Session) CancelTools() {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	if host == nil {
		return
	}
	host.pause()
}

// ResumeTools reopens a paused tool channel for new work. The generation moves,
// so anything still queued from before the pause is refused rather than running
// against a turn that never asked for it.
//
// It does not reopen a channel a closing tool finished: that work is over, and
// reopening it would be a different decision than resuming a pause.
func (s *Session) ResumeTools() {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	host.reopen()
}

// ToolsSettled reports that this session is not on the hook for any tool call —
// none executing and none queued. After an interrupt a caller waits for this
// before checkpointing: a turn can end while a tool is still writing, and
// evidence collected in between describes a workspace that was still moving.
func (s *Session) ToolsSettled() bool {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	if host == nil {
		return true
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	return host.settledLocked()
}

// AwaitToolsSettled blocks until nothing is outstanding, or the context ends.
//
// Cancelling a tool asks its handler to stop; it does not establish that the
// handler has stopped, and a handler that writes files may be mid-write when its
// context is cancelled. This is the difference between the two, and it is what a
// caller waits on after CancelTools before describing a workspace or starting
// anything new. Call it with the channel paused: settlement observed while
// admission is open says only that nothing was outstanding at that instant.
func (s *Session) AwaitToolsSettled(ctx context.Context) error {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	if host == nil {
		return nil
	}
	host.mu.Lock()
	settled := host.settled
	host.mu.Unlock()
	select {
	case <-settled:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseTools shuts the private tool channel. It is safe before a session has
// one, and idempotent afterwards.
func (s *Session) releaseTools() {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	if host != nil {
		host.close()
	}
}

// ToolsClosed reports that a closing tool has ended this session's tool
// channel. It is the only signal that means the work asked to stop; a status,
// a quiet period or a finished turn is not one.
func (s *Session) ToolsClosed() bool {
	s.mu.Lock()
	host := s.tools
	s.mu.Unlock()
	return host != nil && host.channelClosed()
}

func (s *Session) toolRefused(tool, reason string) {
	s.mu.Lock()
	t, report := s.active, s.options.OnDiagnostic
	s.mu.Unlock()
	if t != nil && !t.ended() {
		s.emit(t, Event{Kind: "tool_refused", Tool: tool, Status: reason})
		return
	}
	// No live turn to report it on. This is the refusal that matters most — a
	// harness asking for a tool after its turn ended — so it goes to the caller's
	// diagnostic record instead of nowhere. Both fields are fixed vocabulary: the
	// caller's own tool name and this library's own reason.
	if report != nil {
		report(Diagnostic{Engine: string(s.options.Engine), Stage: "tool_refused", Code: reason, Detail: "tool: " + tool, At: time.Now().UTC()})
	}
}
