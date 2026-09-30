// Package native drives the native Codex, Claude, and Grok command-line harnesses.
// It owns protocol details, not application tool authorization or retry policy.
package native

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/restrict"
	"github.com/shhac/lib-agent-harness/process"
)

// Config selects an installed CLI and its explicit native execution policy.
// Native tools remain available according to the engine's options. For a model
// that only proposes application actions, use package completion instead.
//
// Every field is consumed or refused before launch: options for an engine
// other than the Provider's are refused, never ignored. An empty Provider
// CLI Home inherits the CLI's own authentication and home selection.
type Config struct {
	// RunCommand replaces only process execution, for instrumentation or synthetic tests.
	// Nil runs the Provider's CLI. It must stream output and honor context cancellation.
	RunCommand func(context.Context, []string, string, io.Writer, io.Writer) error
	// Provider's CLI half locates the harness; an API provider is refused.
	Provider harness.Provider
	Model    string
	Effort   string
	Codex    CodexOptions
	Claude   ClaudeOptions
	Grok     GrokOptions
	// Skills are made available for this run (see skills.go): natively by a
	// private plugin for Claude, and as an index in the appended instructions
	// for Codex and Grok, or for every engine with SkillDeliveryComposed.
	// Excluding installed skills is refused.
	Skills harness.Skills
	// Browser turns on the browser integration the harness itself ships, for
	// this run only: Claude Code's --chrome, or Codex's browser_use and
	// browser_use_external features with an already configured native bridge
	// in the selected CLI home and the ChatGPT Chrome extension. The browser
	// uses that profile's logins outside the shell sandbox. Off unless set;
	// an engine without one is refused (see harness.Support).
	Browser bool
	// Background runs the harness, and everything it starts, at background
	// priority, so agent work yields to the machine's interactive use (see
	// harness.Support for where it is offered).
	Background bool
	// Args is a trusted escape hatch for flags the library does not model. It
	// must not come from untrusted model output, and a flag the library manages
	// is refused rather than allowed to override the typed options.
	Args []string
	// Env, when non-nil, is the complete process environment. Nil inherits it.
	Env []string
}

// Request is one initial or resumed turn. Instructions are supplied by the
// caller. Schema is inline JSON for every engine: for Codex the library writes
// it, and reads the report back, in a private directory outside the workspace.
type Request struct {
	Prompt             string
	WorkDir            string
	Schema             string
	ResumeSession      string
	AppendInstructions string
}

// Event is normalized observable harness activity. Payloads can contain private
// model/tool data; applications must apply their own visibility/redaction rules.
// Callbacks run synchronously and must return promptly; no events are dropped.
type Event struct {
	Kind      string
	SessionID string
	ItemID    string
	Text      string
	ToolName  string
	Input     json.RawMessage
	Output    string
	Failed    bool
	Usage     harness.Usage
	Cost      harness.Cost
}

// Result is the latest response plus session-wide accumulated metadata.
// Codex usage is already cumulative; Claude and Grok usage is summed per
// invocation. Cost is the CLI's API-rate valuation, not necessarily money
// charged. Known flags describe completeness: after an unaccounted invocation,
// the figures retain recorded partial usage/cost but must not be treated as
// totals.
type Result struct {
	SessionID string
	// Report is valid JSON: the structured output object, or a JSON string for plain text.
	Report   json.RawMessage
	Usage    harness.Usage
	Cost     harness.Cost
	RawUsage string
	// Failure is the harness's own account of a failed turn. It is provider
	// text, so it never appears in an error; show or record it deliberately.
	Failure string
}

// StreamOptions controls transcript rendering and event delivery.
type StreamOptions struct {
	OnEvent func(Event)
	Clock   func() time.Time
	// Structured expects a report constrained by a Schema rather than a text
	// result. It must match whether the Requests run on the stream set Schema.
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
	// failureCause says why a failed turn failed, where the harness said;
	// empty when it did not.
	failureCause() (harness.Cause, *time.Time)
}

// Stream converts JSON-line CLI output to a common transcript and events.
// Reuse the same stream for sequential resumes of ONE session. It is not safe
// for concurrent writes or snapshots. Close flushes a turn, not the session.
type Stream struct {
	engine     harness.Engine
	structured bool
	t          transcoder
}

func NewStream(engine harness.Engine, transcript io.Writer, options StreamOptions) (*Stream, error) {
	if transcript == nil {
		transcript = io.Discard
	}
	s := &Stream{engine: engine, structured: options.Structured}
	switch engine {
	case harness.Codex:
		t := newCodexTranscoder(transcript)
		t.onEvent = options.OnEvent
		t.structured = options.Structured
		if options.Clock != nil {
			t.now = options.Clock
		}
		s.t = t
	case harness.Claude:
		t := newStreamTranscoder(transcript)
		t.onEvent = options.OnEvent
		t.structured = options.Structured
		if options.Clock != nil {
			t.now = options.Clock
		}
		s.t = t
	case harness.Grok:
		t := newGrokTranscoder(transcript)
		t.onEvent = options.OnEvent
		t.structured = options.Structured
		if options.Clock != nil {
			t.now = options.Clock
		}
		s.t = t
	default:
		return nil, capabilityError(engine, CodeUnsupportedEngine)
	}
	return s, nil
}

func (s *Stream) Write(p []byte) (int, error) { return s.t.Write(p) }
func (s *Stream) Close()                      { s.t.Close() }

// UserPrompt begins a new invocation, clearing the previous report and failure.
func (s *Stream) UserPrompt(prompt string) { s.t.beginTurn(prompt) }
func (s *Stream) Snapshot() Result         { return s.t.snapshot() }

// turnFailed is the failed turn's error, with the cause the harness gave.
func (s *Stream) turnFailed() error {
	failure := turnError(s.engine, CodeTurnFailed)
	failure.Cause, failure.ResetsAt = s.t.failureCause()
	return failure
}

// Report returns the latest invocation's report, or a RunError saying why there
// is none; the provider's own account of a failure is Snapshot().Failure.
func (s *Stream) Report() (json.RawMessage, error) {
	r := s.Snapshot()
	if len(r.Report) > 0 {
		return r.Report, nil
	}
	if r.Failure != "" {
		return nil, s.turnFailed()
	}
	return nil, turnError(s.engine, CodeNoResponse)
}

// buildArgs builds a native CLI invocation. Codex and Claude positionals sit
// behind -- so variadic caller-supplied options cannot consume a prompt or
// session ID; Grok binds every value with --flag=value, since its parser
// rejects a separate value that begins with "-". report is Codex's private
// schema and output files, when a Schema was requested.
func buildArgs(c Config, r Request, report *codexReport) []string {
	switch c.Provider.Engine {
	case harness.Codex:
		return codexArgs(c, r, report)
	case harness.Claude:
		return claudeArgs(c, r)
	default:
		return grokArgs(c, r)
	}
}

func codexArgs(c Config, r Request, report *codexReport) []string {
	args := []string{"exec"}
	if r.ResumeSession != "" {
		args = append(args, "resume")
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	args = append(args, "--json")
	if r.ResumeSession == "" {
		if c.Codex.Sandbox != "" {
			args = append(args, "--sandbox", c.Codex.Sandbox)
		}
		if r.WorkDir != "" {
			args = append(args, "--cd", r.WorkDir)
		}
		args = append(args, "--skip-git-repo-check")
	} else {
		args = append(args, "--skip-git-repo-check")
		if c.Codex.Sandbox != "" {
			args = append(args, "-c", codexOverride("sandbox_mode", c.Codex.Sandbox))
		}
	}
	if r.AppendInstructions != "" {
		args = append(args, "-c", codexOverride("developer_instructions", r.AppendInstructions))
	}
	if report != nil {
		args = append(args, "--output-schema", report.schema, "--output-last-message", report.output)
	}
	args = append(args, c.Args...)
	if c.Browser {
		args = append(args, nativecli.CodexBrowserArgs()...)
	}
	if c.Effort != "" {
		args = append(args, "-c", codexOverride("model_reasoning_effort", c.Effort))
	}
	args = append(args, "--")
	if r.ResumeSession != "" {
		args = append(args, r.ResumeSession)
	}
	return append(args, r.Prompt)
}

func claudeArgs(c Config, r Request) []string {
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
	if c.Claude.PermissionMode != "" {
		args = append(args, "--permission-mode", c.Claude.PermissionMode)
	}
	if c.Browser {
		args = append(args, "--chrome")
	}
	if len(c.Claude.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(c.Claude.AllowedTools, ","))
	}
	if c.Claude.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(c.Claude.MaxBudgetUSD, 'f', -1, 64))
	}
	args = append(args, c.Args...)
	return append(args, "--", r.Prompt)
}

// codexOverride encodes a `-c` override as the TOML Codex parses. Invalid UTF-8
// becomes U+FFFD byte by byte, which is what the JSON encoding used here before
// did, so no value Args accepted before is refused now or means something else.
func codexOverride(key, value string) string {
	encoded, _ := restrict.TOMLString(string([]rune(value)))
	return key + "=" + encoded
}

// execute starts exactly one CLI invocation. There is no automatic retry.
func execute(ctx context.Context, c Config, args []string, workDir string, stdout, stderr io.Writer) error {
	bin := c.Provider.CLI.Binary
	if bin == "" {
		bin = string(c.Provider.Engine)
	}
	cmd, p, err := process.Command(ctx, bin, args...)
	if err != nil {
		return err
	}
	defer p.Close()
	if c.Background {
		p.Background()
	}
	cmd.Dir = workDir
	var outputMu sync.Mutex
	cmd.Stdout = lockedWriter{mu: &outputMu, out: stdout}
	cmd.Stderr = lockedWriter{mu: &outputMu, out: stderr}
	cmd.Env = nativeEnvironment(c)
	cmd.WaitDelay = 10 * time.Second
	return p.Run()
}

// Run drives one turn and returns normalized output. Supply an existing Stream
// to accumulate a resumed session; nil creates a one-turn stream without a log.
//
// Every error is a *RunError, except that a cancelled or expired context
// returns the context's error. The Result is returned with any error, since a
// turn that failed still spent tokens and may still have reported.
func Run(ctx context.Context, c Config, r Request, stream *Stream) (Result, error) {
	engine := c.Provider.Engine
	if err := validate(c, r); err != nil {
		return Result{}, err
	}
	if harness.LoginStoreLocked(engine) {
		return Result{}, &RunError{Engine: engine, Family: harness.FailurePreflight, Code: harness.CodeKeychainUnavailable}
	}
	structured := r.Schema != ""
	if stream != nil && (stream.engine != engine || stream.structured != structured) {
		return Result{}, capabilityError(engine, CodeStreamMismatch)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	delivery, loaded, planErr := planSkills(c)
	if planErr != nil {
		return Result{}, planErr
	}
	c, r, removeSkills, err := applySkills(c, r, delivery, loaded)
	if err != nil {
		return Result{}, err
	}
	defer removeSkills()
	if stream == nil {
		stream, _ = NewStream(engine, nil, StreamOptions{Structured: structured})
	}
	var report *codexReport
	if engine == harness.Codex && structured {
		var err error
		if report, err = newCodexReport(r.WorkDir, r.Schema); err != nil {
			return Result{}, &RunError{Engine: engine, Family: harness.FailurePreflight, Code: CodeReportDirUnavailable, cause: err}
		}
		defer report.remove()
	}
	args := buildArgs(c, r, report)

	stream.UserPrompt(r.Prompt)
	var runErr error
	if c.RunCommand != nil {
		runErr = c.RunCommand(ctx, args, r.WorkDir, stream, stream)
	} else {
		runErr = execute(ctx, c, args, r.WorkDir, stream, stream)
	}
	stream.Close()
	reportErr := error(nil)
	if report != nil {
		reportErr = stream.readCodexReport(report.output)
	}
	result := stream.Snapshot()
	if runErr != nil && ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, runOutcome(engine, stream, result, runErr, reportErr)
}

// runOutcome names the first thing that went wrong: the process, then the
// turn, then the report.
func runOutcome(engine harness.Engine, stream *Stream, result Result, runErr, reportErr error) error {
	switch {
	case runErr != nil:
		return processError(engine, runErr)
	case result.Failure != "":
		return stream.turnFailed()
	case !stream.t.reachedTerminal():
		return &RunError{Engine: engine, Family: harness.FailureProcess, Code: CodeNoTerminalResult}
	case reportErr != nil:
		return &RunError{Engine: engine, Family: harness.FailureTurn, Code: CodeReportUnavailable, cause: reportErr}
	case len(bytes.TrimSpace(result.Report)) == 0:
		return turnError(engine, CodeNoResponse)
	case !json.Valid(result.Report):
		return turnError(engine, CodeMalformedReport)
	}
	return nil
}

// readCodexReport replaces the stream's report with the one Codex wrote. A
// missing file leaves no report, never the streamed message in its place.
func (s *Stream) readCodexReport(path string) error {
	t, ok := s.t.(*codexTranscoder)
	if !ok {
		return nil
	}
	data, err := os.ReadFile(path)
	t.report = data
	return err
}

func overrideEnv(env []string, key, value string) []string {
	return append(withoutEnv(env, key), key+"="+value)
}

// nativeEnvironment applies only the selected engine's documented home and
// policy variables. Nil continues to mean that the process inherits its exact
// environment when no override is requested.
func nativeEnvironment(c Config) []string {
	env := c.Env
	engine := c.Provider.Engine
	if home := c.Provider.CLI.Home; home != "" {
		if env == nil {
			env = os.Environ()
		}
		key := "CODEX_HOME"
		switch engine {
		case harness.Claude:
			key = "CLAUDE_CONFIG_DIR"
		case harness.Grok:
			key = "GROK_HOME"
		}
		if engine == harness.Claude && isDefaultClaudeHome(home, env) {
			env = withoutEnv(env, key)
		} else {
			env = overrideEnv(env, key, home)
		}
	}
	if engine != harness.Grok || c.Grok.Telemetry != GrokTelemetryReduced {
		return env
	}
	return withGrokReducedTelemetry(env)
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

// Version reads the installed CLI version without model inference. Only the
// Provider, Env and RunCommand are read; engine options are not checked.
func Version(ctx context.Context, c Config) (string, error) {
	engine := c.Provider.Engine
	if !harness.Support(engine, harness.Run, harness.Available).Usable() {
		return "", capabilityError(engine, CodeUnsupportedEngine)
	}
	if code := c.Provider.Problem(); code != "" {
		return "", capabilityError(engine, code)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var out bytes.Buffer
	args := []string{"--version"}
	var err error
	if c.RunCommand != nil {
		err = c.RunCommand(ctx, args, "", &out, io.Discard)
	} else {
		err = execute(ctx, c, args, "", &out, io.Discard)
	}
	switch {
	case err != nil && ctx.Err() != nil:
		err = ctx.Err()
	case err != nil:
		err = processError(engine, err)
	}
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
