// Package session controls persistent agent sessions: native CLI sessions,
// whose native tools are available according to the explicitly configured
// provider policy, and sessions over an OpenAI-compatible endpoint, whose agent
// loop the library runs with only the caller's hosted tools. This is not the
// tools-disabled completion API.
package session

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// MaxFrameBytes bounds one native JSON-line frame, including discarded tool
// payloads. Oversized frames stop the session with ErrOutputLimit.
const MaxFrameBytes = 16 << 20

// Capabilities is what one session has established about its installed
// harness. It starts from harness.Support's static claim for the engine and
// records what the running harness has since shown.
type Capabilities struct {
	Start, Resume, Interrupt, Steer, ReplaceInstructions, AppendInstructions harness.Capability
	// Telemetry capabilities become Native only after a successful response or
	// event. Older CLI versions may reject optional inspection methods.
	Account, Quota, Context, Compact harness.Capability
	// RestrictTools reports whether the installed harness was observed running
	// with exactly the configured tool surface. Unknown means it was not checked
	// on this path, never that it was checked and found acceptable.
	RestrictTools harness.Capability
}

// CapabilitiesFor describes availability before contacting an installed CLI,
// as harness.Support states it for a session. Strategy alone is not evidence
// that a particular installed version supports it. Account and Quota are the
// account operation's Login and Quota features, which a session reads through
// its own harness process.
func CapabilitiesFor(e harness.Engine) Capabilities {
	session := func(f harness.Feature) harness.Capability { return harness.Support(e, harness.Session, f) }
	account := func(f harness.Feature) harness.Capability {
		if start := session(harness.Available); !start.Usable() {
			return start
		}
		return harness.Support(e, harness.Account, f)
	}
	return Capabilities{
		Start:               session(harness.Available),
		Resume:              session(harness.Resume),
		Interrupt:           session(harness.Interrupt),
		Steer:               session(harness.Steer),
		ReplaceInstructions: session(harness.ReplaceInstructions),
		AppendInstructions:  session(harness.AppendInstructions),
		Account:             account(harness.Login),
		Quota:               account(harness.Quota),
		Context:             session(harness.ContextWindow),
		Compact:             session(harness.Compact),
		RestrictTools:       session(harness.RestrictTools),
	}
}

type InstructionMode string

const (
	Replace InstructionMode = "replace"
	Append  InstructionMode = "append"
)

// Instructions apply at session creation and must remain identical on resume.
// Replace replaces the native base prompt; Append uses provider-supported
// additional instructions. Neither suppresses project files or managed policy.
type Instructions struct {
	Mode InstructionMode `json:"mode,omitempty"`
	Text string          `json:"text,omitempty"`
}

// Policy uses explicit native provider settings. Empty values resolve to
// read-only/never for Codex, dontAsk for Claude and deny for Grok. Setting a
// field only another engine reads is refused rather than ignored. Unhandled
// requests from the CLI are denied. These are execution settings, not a
// tools-disabled guarantee.
type Policy struct {
	CodexSandbox     string `json:"codex_sandbox,omitempty"`
	CodexApproval    string `json:"codex_approval,omitempty"`
	ClaudePermission string `json:"claude_permission,omitempty"`
	// ClaudeTools nil preserves native tools; a non-nil empty slice disables
	// native tools. This does not disable installed hooks or MCP configuration.
	ClaudeTools []string `json:"claude_tools,omitempty"`
	// GrokPermission answers the permission requests Grok sends, and a Grok
	// session requires it to be set: GrokDenyWhenAsked rejects each request,
	// GrokAllowWhenAsked approves each one once, and neither grants an
	// "always" option. It governs only the requests Grok actually makes: Grok
	// 1.0.41's agent mode asks only where a permission rule or its configured
	// mode says to, and otherwise runs edits and shell commands without
	// asking. Neither value makes a read-only session, which is why there is
	// no default: a Grok session is more permissive than Codex's read-only or
	// Claude's dontAsk defaults, and the caller must choose it knowingly.
	GrokPermission string `json:"grok_permission,omitempty"`
	// GrokTelemetry "" preserves the installed CLI's behaviour;
	// GrokTelemetryReduced turns off Grok's documented client telemetry,
	// trace upload, feedback, auto-update and memory controls, and its imports
	// of other harnesses' skills, rules, agents, MCP servers, hooks and
	// sessions, for this session's process. It is not a no-egress guarantee.
	GrokTelemetry string `json:"grok_telemetry,omitempty"`
}

// Grok permission answers and telemetry policy.
const (
	GrokDenyWhenAsked    = "deny-when-asked"
	GrokAllowWhenAsked   = "allow-when-asked"
	GrokTelemetryReduced = "reduced"
)

type Options struct {
	// Provider selects the engine and locates its CLI. Only a CLI provider for
	// an engine harness.Support offers sessions is accepted. An empty
	// Provider.CLI.Binary runs the engine's name on PATH; an empty
	// Provider.CLI.Home resolves CODEX_HOME, CLAUDE_CONFIG_DIR or GROK_HOME,
	// then ~/.codex, ~/.claude or ~/.grok. Both resolved values are part of a
	// Ref.
	Provider               harness.Provider
	WorkDir, Model, Effort string
	// AccountIdentity is a caller-owned, non-secret label. Credentials are never
	// read or copied. Change this label when deliberately changing an account.
	AccountIdentity string
	Instructions    Instructions
	Policy          Policy
	// RuntimeHome is the durable private home a restricted session runs in. The
	// library owns its configuration and shares only the login from
	// Provider.CLI.Home, so a worker gets the operator's account without the
	// rest of their setup. It must be separate from that home, must persist for
	// the assignment's life — the native conversation lives in it — and belongs
	// in private application state.
	RuntimeHome string
	// Restriction opts this session into the restricted worker contract: the
	// harness's own tools are removed and replaced by the caller's, verified
	// against the installed CLI before a credentialed process starts. Leaving it
	// nil keeps the ordinary native contract exactly as it was.
	Restriction *Restriction
	// Sandbox opts an ordinary native session into the installed CLI's OS
	// sandbox: no network, and project writes only inside WorkDir when
	// Sandbox.Write is set. It is proved before a credentialed launch. A session is either
	// restricted or sandboxed, never both. Nil keeps the ordinary contract.
	Sandbox *Sandbox
	// Skills are made available to the session, and installed skills kept or
	// excluded, as skills.go describes for each engine and mode: natively
	// where the harness loads them itself, and otherwise composed by the
	// library. A request the mode cannot honour is refused. A skill request
	// is part of a Ref; a session without one keeps its existing digest.
	Skills harness.Skills
	// SkillRun says how a restricted session's hosted run_skill_script runs a
	// permitting skill's scripts. It is required when such a skill is
	// provided to a restricted session, and refused anywhere else. It is not
	// part of a Ref.
	SkillRun SkillRunOptions
	// skills is the planned delivery, set by normalize.
	skills *skillPlan
	// Env adds KEY=VALUE entries to the session's environment, after the
	// harness has removed provider credentials and overrides. It is for
	// ordinary settings such as a build cache inside the workspace; keys the
	// harness manages or strips are refused. It is not part of a Ref.
	Env []string
	// Context, when set, is asked for the caller's current context at the
	// start of a turn whose conversation is new or was compacted since the last
	// turn. Its text is delivered ahead of the turn's input, once per pending
	// reason. The harness never replays anything itself.
	//
	// The turn's text becomes, exactly:
	//
	//	<caller-context reason="started">
	//	…the returned text…
	//	</caller-context>
	//
	//	…the turn's input…
	//
	// with reason "started" or "compacted". Empty text sends the input
	// unchanged. The returned text is inserted verbatim. An error fails
	// StartTurn and keeps the reason pending; the reason is cleared only once a
	// turn carrying it has been accepted by the harness. A compaction reported
	// while a turn runs is delivered with the next one. A restricted session
	// keeps the pending reason in its tool directory, so it survives a restart
	// and is read back on Resume; an ordinary session keeps it in memory.
	Context func(ctx context.Context, reason ContextReason) (string, error)
	// OnDiagnostic receives bounded, sanitized detail about a failure, once, for
	// the caller's own private records. It is deliberately not part of any error
	// value: like provider text, captured harness output does not belong in a
	// message that might reach a log or a user interface by default.
	OnDiagnostic func(Diagnostic)
	// QuietAfter is how long without an event makes a live session Quiet rather
	// than Running. Quiet is unknown, never stuck. Defaults to two minutes.
	QuietAfter time.Duration
	// EventBuffer defaults to 256; MaxTextBytes defaults to 1 MiB per turn.
	EventBuffer, MaxTextBytes int
	// Loop bounds the agent loop the library runs for an OpenAI-compatible
	// session (see api.go). Only such a session reads it; setting it for a
	// CLI engine is refused. It is not part of a Ref.
	Loop Loop
	// complete replaces completion.Complete for synthetic tests.
	complete modelCall
}

// ContextReason says why a session asks its caller for current context.
type ContextReason string

const (
	ContextStarted   ContextReason = "started"   // the conversation is new
	ContextCompacted ContextReason = "compacted" // its history was compacted since the last turn
)

// Diagnostic carries what a failure looked like locally. Detail is a bounded,
// control-stripped tail of the harness's own standard error with credential-
// shaped runs removed; it is evidence for an operator, not a classification.
type Diagnostic struct {
	Engine harness.Engine `json:"engine"`
	Stage  string         `json:"stage"`
	Code   string         `json:"code"`
	Detail string         `json:"detail,omitempty"`
	At     time.Time      `json:"at"`
}

// Ref is safe to persist as private application state. It contains local paths
// and an opaque configuration digest, never instructions or credentials. It
// binds the selected home/account label, not the identity of a refreshed login.
type Ref struct {
	Engine          harness.Engine `json:"engine"`
	ID              string         `json:"id"`
	Home            string         `json:"home"`
	WorkDir         string         `json:"work_dir"`
	AccountIdentity string         `json:"account_identity,omitempty"`
	ConfigHash      string         `json:"config_hash"`
}
type Input struct{ Text string }
type SteerOptions struct{ RequireNative bool }
type SteerResult struct {
	Strategy harness.Availability
	Turn     *Turn
}

// Event is observable session activity. Text, Input and Output are model and
// tool data that can be private: the library passes them through unredacted
// (except its own tool-channel credential) and applications apply their own
// visibility rules before showing or storing them.
type Event struct {
	Kind   string `json:"kind"` // text_delta, text, tool_started, tool_completed, status, usage, context, quota, credits, account
	TurnID string `json:"turn_id"`
	ItemID string `json:"item_id,omitempty"`
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
	// Status on tool_completed is "completed", "failed", or the engine's own
	// terminal word (Codex's "declined", an API session's "refused").
	Status string `json:"status,omitempty"`
	// Input is a tool's arguments as the harness reported them, always valid
	// JSON: set on tool_started, and on tool_completed where the completion
	// restates the call (Codex items, Grok updates carrying rawInput).
	Input json.RawMessage `json:"input,omitempty"`
	// Output is a tool's result text on tool_completed: its aggregated output,
	// result content or error message.
	Output string `json:"output,omitempty"`
	// ExitCode is a command's exit status, where the harness reports one.
	ExitCode *int `json:"exit_code,omitempty"`
	// Input and Output are each bounded to MaxToolPayloadBytes. A truncated
	// Output is its first bytes, cut on a character boundary; a truncated Input
	// becomes a JSON string holding the first bytes of the original JSON text,
	// so it stays valid JSON but no longer parses as the arguments object.
	InputTruncated  bool                     `json:"input_truncated,omitempty"`
	OutputTruncated bool                     `json:"output_truncated,omitempty"`
	Usage           *Usage                   `json:"usage,omitempty"`
	Context         *ContextSnapshot         `json:"context,omitempty"`
	Quota           *harness.QuotaSnapshot   `json:"quota,omitempty"`
	Credits         *harness.CreditSnapshot  `json:"credits,omitempty"`
	Account         *harness.AccountSnapshot `json:"account,omitempty"`
}

// Usage is what a provider reported, in the shared harness shape: Input counts
// every prompt token, cached or not, and the cache figures are parts of it.
//
// Final separates a turn's own accounting from an observation of one model
// response inside it. A turn makes many requests, so a caller that wants to
// react while work is still running has to read the non-final ones — and a
// caller that wants the turn's accounting must not sum them.
type Usage struct {
	harness.Usage
	Final bool `json:"final"`
}

// add accumulates one response's figures. Anything that would not stay
// representable makes the accumulation unknown rather than wrapping into a
// number that looks measured. The cache split stays known only while every
// response reported it.
func (u Usage) add(next Usage) Usage {
	if !next.Known {
		return u
	}
	columns := [][2]int64{{u.Input, next.Input}, {u.Output, next.Output}, {u.CacheRead, next.CacheRead}, {u.CacheWrite, next.CacheWrite}, {u.Reasoning, next.Reasoning}}
	for _, c := range columns {
		if c[1] < 0 || c[0] > maxInt64-c[1] {
			return Usage{}
		}
	}
	return Usage{Usage: harness.Usage{
		Known:      true,
		Input:      u.Input + next.Input,
		Output:     u.Output + next.Output,
		CacheRead:  u.CacheRead + next.CacheRead,
		CacheWrite: u.CacheWrite + next.CacheWrite,
		Reasoning:  u.Reasoning + next.Reasoning,
		CacheKnown: (!u.Known || u.CacheKnown) && next.CacheKnown,
	}}
}

const maxInt64 = int64(^uint64(0) >> 1)

type Result struct {
	// NativeError preserves a native failure indication, including Claude's
	// error_during_execution result after a requested interruption. Correlation
	// proves the turn ended, not that interruption was the sole failure cause.
	NativeError          bool
	TurnID, Text, Status string
	Usage                Usage
	// Observed accumulates each model response's reported usage during this
	// turn. It survives a failed or interrupted turn, where the provider's own
	// terminal accounting is often absent or zero despite work having happened.
	// It is evidence of what was seen, not a measurement of the turn: a caller
	// with a budget should still treat an unknown Usage as an unknown call.
	Observed Usage
	Context  ContextSnapshot
}
type Turn struct {
	mu                 sync.Mutex
	id                 string
	events             chan Event
	done               chan struct{}
	result             Result
	err                error
	finished           bool
	textItem           string
	lastCodexTotal     codexUsage
	interruptRequested bool
	starting           bool
	compacting         bool
	awaitingCompactID  bool
	pending            []map[string]json.RawMessage
	pendingBytes       int
	// grokResponse counts Grok's completed model responses in this turn; it
	// names the response the next streamed text belongs to.
	grokResponse int
	// closeTools shuts the caller's tool channel when this turn ends.
	closeTools func()
}

func (t *Turn) ID() string           { t.mu.Lock(); defer t.mu.Unlock(); return t.id }
func (t *Turn) Events() <-chan Event { return t.events }

// ended reports that this turn's stream is closed, so anything emitted to it now
// would be dropped rather than observed.
func (t *Turn) ended() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.finished }

// Wait does not drain Events; callers should consume the bounded event stream
// concurrently. Context cancellation here only stops waiting.
func (t *Turn) Wait(ctx context.Context) (Result, error) {
	select {
	case <-t.done:
		t.mu.Lock()
		defer t.mu.Unlock()
		result := t.result
		result.Context = cloneContext(result.Context)
		return result, t.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}
func (t *Turn) finish(status string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	t.finished = true
	t.result.Status = status
	t.result.TurnID = t.id
	t.err = err
	// Tool admission closes with the turn, not when the caller gets around to
	// noticing it has ended. Both installed harnesses have been observed sending
	// a tool call after their terminal result, and there is always a gap between
	// a turn ending and a caller reacting to it — so a caller that closed the
	// channel on the terminal event would still be racing. Anything already
	// running is left alone: it is cancellation that stops work, and a turn
	// ending is not a reason to abandon a write half-done.
	if t.closeTools != nil {
		t.closeTools()
	}
	close(t.events)
	close(t.done)
}
func (t *Turn) emit(e Event) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return nil
	}
	e.TurnID = t.id
	select {
	case t.events <- e:
		return nil
	default:
		return ErrBackpressure
	}
}
