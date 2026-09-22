// Package native drives the native Codex and Claude command-line harnesses.
// It owns protocol details, not application tool authorization or retry policy.
package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

// Config selects an installed CLI and its explicit native execution policy.
// Native tools remain available according to Sandbox / PermissionMode. For a
// model that only proposes application actions, use package completion instead.
// An empty Home inherits the CLI's own authentication and home selection.
// Args is a trusted escape hatch; it can override policy and must not come from
// untrusted model output.
type Config struct {
	// RunCommand replaces only process execution, for instrumentation or synthetic tests.
	// Nil uses Execute. It must stream output and honor context cancellation.
	RunCommand     func(context.Context, []string, string, io.Writer, io.Writer) error
	Engine         string
	Binary         string
	Home           string
	Model          string
	Effort         string
	Sandbox        string
	PermissionMode string
	AllowedTools   []string
	MaxBudgetUSD   float64
	Args           []string
	// Env, when non-nil, is the complete process environment. Nil inherits it.
	Env []string
}

// Request is one initial or resumed turn. Instructions are supplied by the
// caller. Schema is inline JSON for Claude; Codex consumes SchemaPath. OutputPath
// is the Codex final-response file. The caller owns these files and their lifetime.
type Request struct {
	Prompt             string
	WorkDir            string
	Schema             string
	SchemaPath         string
	OutputPath         string
	ResumeSession      string
	AppendInstructions string
}

// Event is normalized observable harness activity. Payloads can contain private
// model/tool data; applications must apply their own visibility/redaction rules.
// Callbacks run synchronously and must return promptly; no events are dropped.
type Event struct {
	UsageKnown bool
	CostKnown  bool
	Kind       string
	SessionID  string
	ItemID     string
	Text       string
	ToolName   string
	Input      json.RawMessage
	Output     string
	Failed     bool
	Usage      TokenUsage
	CostUSD    float64
}

// Result is the latest response plus session-wide accumulated metadata.
// Codex usage is already cumulative; Claude usage is summed per invocation.
// CostUSD is the CLI's API-rate valuation, not necessarily money charged.
// Known flags describe completeness: after an unaccounted invocation, numeric
// fields retain recorded partial usage/cost but must not be treated as totals.
type Result struct {
	SessionID string
	// Report is valid JSON: the structured output object, or a JSON string for plain text.
	Report     json.RawMessage
	Usage      TokenUsage
	CostUSD    float64
	UsageKnown bool
	CostKnown  bool
	RawUsage   string
	Failure    string
}

// StreamOptions controls transcript rendering and event delivery.
type StreamOptions struct {
	OnEvent func(Event)
	Clock   func() time.Time
	// Structured expects Claude's structured_output rather than its text result.
	Structured bool
}

// transcoder is one engine's half of Stream: reading that CLI's stream, the
// per-turn reset its usage accounting requires, and assembling its Result.
// Those three rules differ per engine and are stated beside the engine
// knowledge that justifies them, in codexstream.go and claudestream.go.
// Stream owns dispatch and nothing else.
type transcoder interface {
	io.Writer
	Close()
	beginTurn(prompt string)
	snapshot() Result
	// reachedTerminal reports whether the turn ended with a terminal result.
	// Named apart from the transcoders' own completed field, which it reads.
	reachedTerminal() bool
}

// Stream converts JSON-line CLI output to a common transcript and events.
// Reuse the same stream for sequential resumes of ONE session. It is not safe
// for concurrent writes or snapshots. Close flushes a turn, not the session.
type Stream struct {
	engine string
	t      transcoder
}

func NewStream(engine string, transcript io.Writer, options StreamOptions) (*Stream, error) {
	if transcript == nil {
		transcript = io.Discard
	}
	s := &Stream{engine: engine}
	switch engine {
	case "codex":
		t := newCodexTranscoder(transcript)
		t.onEvent = options.OnEvent
		t.structured = options.Structured
		if options.Clock != nil {
			t.now = options.Clock
		}
		s.t = t
	case "claude":
		t := newStreamTranscoder(transcript)
		t.onEvent = options.OnEvent
		t.structured = options.Structured
		if options.Clock != nil {
			t.now = options.Clock
		}
		s.t = t
	default:
		return nil, fmt.Errorf("unsupported native harness %q", engine)
	}
	return s, nil
}

func (s *Stream) Write(p []byte) (int, error) { return s.t.Write(p) }
func (s *Stream) Close()                      { s.t.Close() }

// UserPrompt begins a new invocation, clearing the previous report and failure.
func (s *Stream) UserPrompt(prompt string) { s.t.beginTurn(prompt) }
func (s *Stream) Snapshot() Result         { return s.t.snapshot() }
func (s *Stream) Report() (json.RawMessage, error) {
	r := s.Snapshot()
	if len(r.Report) > 0 {
		return r.Report, nil
	}
	if r.Failure != "" {
		return nil, fmt.Errorf("no report: %s", r.Failure)
	}
	return nil, fmt.Errorf("no structured output in the result event")
}

// Args builds a native CLI invocation. Every positional sits behind -- so
// variadic caller-supplied options cannot consume a prompt or session ID.
func Args(c Config, r Request) ([]string, error) {
	switch c.Engine {
	case "codex":
		args := []string{"exec"}
		if r.ResumeSession != "" {
			args = append(args, "resume")
		}
		if c.Model != "" {
			args = append(args, "--model", c.Model)
		}
		args = append(args, "--json")
		if r.ResumeSession == "" {
			if c.Sandbox != "" {
				args = append(args, "--sandbox", c.Sandbox)
			}
			if r.WorkDir != "" {
				args = append(args, "--cd", r.WorkDir)
			}
			args = append(args, "--skip-git-repo-check")
		} else {
			args = append(args, "--skip-git-repo-check")
			if c.Sandbox != "" {
				v, _ := json.Marshal(c.Sandbox)
				args = append(args, "-c", "sandbox_mode="+string(v))
			}
		}
		if r.AppendInstructions != "" {
			v, _ := json.Marshal(r.AppendInstructions)
			args = append(args, "-c", "developer_instructions="+string(v))
		}
		if r.SchemaPath != "" {
			args = append(args, "--output-schema", r.SchemaPath)
		}
		if r.OutputPath != "" {
			args = append(args, "--output-last-message", r.OutputPath)
		}
		args = append(args, c.Args...)
		if c.Effort != "" {
			v, _ := json.Marshal(c.Effort)
			args = append(args, "-c", "model_reasoning_effort="+string(v))
		}
		args = append(args, "--")
		if r.ResumeSession != "" {
			args = append(args, r.ResumeSession)
		}
		return append(args, r.Prompt), nil
	case "claude":
		args := []string{"-p"}
		if r.ResumeSession != "" {
			args = append(args, "--resume", r.ResumeSession)
		}
		args = append(args, "--output-format", "stream-json", "--verbose")
		if r.Schema != "" {
			args = append(args, "--json-schema", jsonCompact(r.Schema))
		}
		if r.AppendInstructions != "" {
			args = append(args, "--append-system-prompt", r.AppendInstructions)
		}
		if c.Model != "" {
			args = append(args, "--model", c.Model)
		}
		if c.Effort != "" {
			args = append(args, "--effort", c.Effort)
		}
		if c.PermissionMode != "" {
			args = append(args, "--permission-mode", c.PermissionMode)
		}
		if len(c.AllowedTools) > 0 {
			args = append(args, "--allowedTools", strings.Join(c.AllowedTools, ","))
		}
		if c.MaxBudgetUSD > 0 {
			args = append(args, "--max-budget-usd", strconv.FormatFloat(c.MaxBudgetUSD, 'f', -1, 64))
		}
		args = append(args, c.Args...)
		return append(args, "--", r.Prompt), nil
	default:
		return nil, fmt.Errorf("unsupported native harness %q", c.Engine)
	}
}

// Execute starts exactly one CLI invocation. There is no automatic retry.
func Execute(ctx context.Context, c Config, args []string, workDir string, stdout, stderr io.Writer) error {
	bin := c.Binary
	if bin == "" {
		bin = c.Engine
	}
	cmd, p, err := process.Command(ctx, bin, args...)
	if err != nil {
		return err
	}
	defer p.Close()
	cmd.Dir = workDir
	var outputMu sync.Mutex
	cmd.Stdout = lockedWriter{mu: &outputMu, out: stdout}
	cmd.Stderr = lockedWriter{mu: &outputMu, out: stderr}
	cmd.Env = c.Env
	if c.Home != "" {
		env := cmd.Env
		if env == nil {
			env = os.Environ()
		}
		key := "CODEX_HOME"
		if c.Engine == "claude" {
			key = "CLAUDE_CONFIG_DIR"
		}
		if c.Engine == "claude" && isDefaultClaudeHome(c.Home, env) {
			cmd.Env = withoutEnv(env, key)
		} else {
			cmd.Env = overrideEnv(env, key, c.Home)
		}
	}
	cmd.WaitDelay = 10 * time.Second
	return p.Run()
}

// Run drives one turn and returns normalized output. Supply an existing Stream
// to accumulate a resumed session; nil creates a one-turn stream without a log.
func Run(ctx context.Context, c Config, r Request, stream *Stream) (Result, error) {
	if stream != nil && stream.engine != c.Engine {
		return Result{}, fmt.Errorf("stream engine %q does not match invocation %q", stream.engine, c.Engine)
	}
	args, err := Args(c, r)
	if err != nil {
		return Result{}, err
	}
	if stream == nil {
		stream, err = NewStream(c.Engine, nil, StreamOptions{Structured: r.Schema != "" || r.SchemaPath != ""})
		if err != nil {
			return Result{}, err
		}
	}
	if c.Engine == "codex" && r.OutputPath != "" {
		if err := os.Remove(r.OutputPath); err != nil && !os.IsNotExist(err) {
			return Result{}, err
		}
	}
	stream.UserPrompt(r.Prompt)
	if c.RunCommand != nil {
		err = c.RunCommand(ctx, args, r.WorkDir, stream, stream)
	} else {
		err = Execute(ctx, c, args, r.WorkDir, stream, stream)
	}
	stream.Close()
	result := stream.Snapshot()
	if c.Engine == "codex" && r.OutputPath != "" {
		result.Report = nil
		data, readErr := os.ReadFile(r.OutputPath)
		if readErr == nil {
			if r.SchemaPath == "" {
				result.Report, _ = json.Marshal(string(data))
			} else {
				result.Report = data
			}
		} else if err == nil {
			err = readErr
		}
	}

	if err == nil && result.Failure != "" {
		err = fmt.Errorf("%s: %s", c.Engine, result.Failure)
	}
	if err == nil && !stream.t.reachedTerminal() {
		err = fmt.Errorf("%s ended without a terminal result", c.Engine)
	}
	if err == nil && len(bytes.TrimSpace(result.Report)) == 0 {
		err = fmt.Errorf("%s ended without a response", c.Engine)
	}
	if err == nil && !json.Valid(result.Report) {
		err = fmt.Errorf("%s returned malformed structured output", c.Engine)
	}
	return result, err
}

func overrideEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, v := range env {
		if !strings.HasPrefix(v, key+"=") {
			out = append(out, v)
		}
	}
	return append(out, key+"="+value)
}
func jsonCompact(s string) string {
	var b bytes.Buffer
	if json.Compact(&b, []byte(s)) != nil {
		return s
	}
	return b.String()
}
func joinRawUsage(payloads []json.RawMessage) string {
	if len(payloads) == 0 {
		return ""
	}
	out, err := json.Marshal(payloads)
	if err != nil {
		return ""
	}
	return string(out)
}

// Serialize stdout/stderr sinks even when they ultimately share a transcript.
type lockedWriter struct {
	mu  *sync.Mutex
	out io.Writer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out == nil {
		return len(p), nil
	}
	return w.out.Write(p)
}

// Version reads the installed CLI version without model inference.
func Version(ctx context.Context, c Config) (string, error) {
	var out bytes.Buffer
	err := Execute(ctx, c, []string{"--version"}, "", &out, io.Discard)
	return strings.TrimSpace(out.String()), err
}

func withoutEnv(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			out = append(out, entry)
		}
	}
	return out
}
func isDefaultClaudeHome(home string, env []string) bool {
	userHome := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, "HOME=") {
			userHome = strings.TrimPrefix(entry, "HOME=")
		}
	}
	if userHome == "" {
		userHome, _ = os.UserHomeDir()
	}
	return userHome != "" && filepath.Clean(home) == filepath.Join(userHome, ".claude")
}
