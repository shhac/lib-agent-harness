package session

// Sessions over an OpenAI-compatible endpoint. An endpoint is only a model, so
// the library is the agent: each turn calls the model through completion's
// transport, runs every proposed call through the caller's handler one at a
// time, answers the library's skill calls, and repeats until the model answers
// without calls. The Session, Turn, Event and Result shapes are the CLIs'.
//
// Only the caller's hosted tools and the library's skill tools exist. The
// library writes every request itself, so the restricted surface holds by
// construction rather than by probe, and no bridge is involved: the handler is
// called directly, under the same admission, serialization, closing-tool and
// settlement rules as a hosted channel.
//
// The conversation lives in RuntimeHome/sessions/<id>/transcript.jsonl, an
// append-only record synced after every write: the user's input, each model
// response, each tool call before it runs and its result after it returns,
// each response's usage, and each turn's start and end. The session holds an
// exclusive lock on its directory while it is open, and releases it only once
// every call it admitted has returned. Nothing credential-shaped is recorded:
// credentials are fetched per request by the caller's source and never kept.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
	"github.com/shhac/lib-agent-harness/internal/skills"
)

// Loop bounds the agent loop the library runs for an OpenAI-compatible
// session.
type Loop struct {
	// MaxSteps bounds the model requests one turn may make, so a model that
	// keeps calling tools cannot run unattended. Zero means 64; more than 1024
	// is refused. A turn that reaches it fails with TurnStepLimit.
	MaxSteps int
	// MaxRequestBytes bounds one request body, which carries the whole
	// history: Chat Completions resends it on every call. Zero means 8 MiB;
	// more than 64 MiB is refused. A turn whose history outgrows it fails with
	// cause context_limit.
	MaxRequestBytes int
	// RequestTimeout bounds one model request. Zero means five minutes.
	RequestTimeout time.Duration
}

const (
	defaultLoopSteps        = 64
	maxLoopSteps            = 1024
	defaultLoopRequestBytes = 8 << 20
	maxLoopRequestBytes     = 64 << 20
)

// TurnStepLimit is the TurnError code of a turn that reached Loop.MaxSteps.
const TurnStepLimit = "max_steps"

type modelCall func(context.Context, completion.Config, []completion.Message, []completion.Tool) (completion.Result, error)

// Recovery is what reopening a session found left over from the process that
// last had it open. It is reported, never acted on: no tool is ever re-run.
type Recovery struct {
	// TurnID names a turn that was still running when the session stopped
	// without closing, "" when there was none. Such a turn is recorded as
	// interrupted.
	TurnID string `json:"turn_id,omitempty"`
	// UnknownOutcomes are calls that had started without a recorded result.
	// Their effects may or may not have happened; each is answered in the
	// history as having an unknown outcome.
	UnknownOutcomes []RecoveredCall `json:"unknown_outcomes,omitempty"`
}

// RecoveredCall names one call by the model's identifier and the tool's name.
type RecoveredCall struct {
	ID   string `json:"id"`
	Tool string `json:"tool"`
}

// Recovered reports what opening this session found left over. It is the zero
// value for a new session, a clean resume, and every CLI session.
func (s *Session) Recovered() Recovery {
	if s.api == nil {
		return Recovery{}
	}
	return s.api.recovery
}

// State failure codes: an OpenAI-compatible session's own durable state.
const (
	// StateLocked: another session, in this process or another, has the
	// conversation open. It unwraps to ErrLeaseHeld.
	StateLocked = "session_locked"
	// StateUnusable: RuntimeHome or the session's directory cannot be used.
	StateUnusable = "state_unusable"
	// StateMissing: the reference's conversation is not in RuntimeHome. Open
	// starts a new one with FreshUnavailable.
	StateMissing = "conversation_missing"
	// StateCorrupt: the transcript cannot be read, or names another session.
	StateCorrupt = "transcript_unreadable"
	// StateUnwritable: a record could not be made durable. The session ends,
	// because its history could no longer be trusted to survive a restart.
	StateUnwritable = "transcript_unwritable"
)

// StateError reports that a session's durable state could not be used.
type StateError struct {
	Engine harness.Engine
	Code   string
}

func (e *StateError) Error() string {
	message := map[string]string{
		StateLocked:     "the conversation is open in another session",
		StateUnusable:   "the session's runtime home cannot be used",
		StateMissing:    "the conversation is not in the runtime home",
		StateCorrupt:    "the session's transcript cannot be read",
		StateUnwritable: "the session's transcript could not be written",
	}[e.Code]
	if message == "" {
		message = "the session's state cannot be used"
	}
	return string(e.Engine) + ": " + message
}

func (e *StateError) Unwrap() error {
	switch e.Code {
	case StateLocked:
		return ErrLeaseHeld
	case StateMissing:
		return errConversationGone
	}
	return nil
}

func (e *StateError) HarnessFacts() harness.Facts {
	return harness.Facts{Engine: e.Engine, Operation: harness.Session, Family: harness.FailurePreflight, Code: e.Code}
}

func stateError(code string) *StateError {
	return &StateError{Engine: harness.OpenAICompatible, Code: code}
}

// apiSession is the library's side of a session whose loop it runs.
type apiSession struct {
	store    *transcript
	complete modelCall
	tools    []completion.Tool
	system   string
	recovery Recovery

	mu        sync.Mutex
	records   []record
	responses int
	// cancelTurn stops the running turn's request and loop; loopDone closes
	// when that loop has returned.
	cancelTurn context.CancelFunc
	loopDone   chan struct{}
	stopping   bool
	// released closes once the transcript is closed and the lock given up.
	released chan struct{}
	// failed ends the session when a record cannot be made durable.
	failed func(error)
}

// normalizeAPI resolves an OpenAI-compatible session's options, refusing every
// field it cannot honour. Only what the loop reads is accepted.
func normalizeAPI(o Options) (Options, error) {
	switch {
	case o.Sandbox != nil:
		return o, &UnsupportedError{Engine: o.Provider.Engine, Operation: "sandbox", Code: RefusedNotOffered, Capability: harness.Support(o.Provider.Engine, harness.Session, harness.Sandbox)}
	case o.Model == "":
		return o, refuse(o, "model", RefusedModelRequired, "an API session requires an explicit model")
	case o.WorkDir != "":
		return o, refuse(o, "work_dir", RefusedConflict, "an API session has no workspace: the caller's tools own theirs, and SkillRun.WorkDir says where skill scripts run; leave WorkDir unset")
	case len(o.Env) > 0:
		return o, refuse(o, "env", RefusedConflict, "an API session starts no process; leave Env unset")
	case policySet(o.Policy):
		return o, refuse(o, "policy", RefusedOtherEnginePolicy, "an API session reads no native policy; leave Policy unset")
	}
	if code := o.Provider.API.EffortProblem(o.Effort); code != "" {
		return o, refuse(o, "effort", code, "the effort cannot be sent to this endpoint as configured")
	}
	var err error
	if o, err = normalizeLimits(o); err != nil {
		return o, err
	}
	if err = checkInstructions(o); err != nil {
		return o, err
	}
	if o, err = normalizeAPIRestriction(o); err != nil {
		return o, err
	}
	if o, err = normalizeRuntimeHome(o); err != nil {
		return o, err
	}
	if o, err = normalizeLoop(o); err != nil {
		return o, err
	}
	return normalizeAPISkills(o)
}

func policySet(p Policy) bool {
	return p.CodexSandbox != "" || p.CodexApproval != "" || p.ClaudePermission != "" || p.ClaudeTools != nil || p.GrokPermission != "" || p.GrokTelemetry != ""
}

func checkInstructions(o Options) error {
	if o.Instructions.Mode != "" && o.Instructions.Mode != Replace && o.Instructions.Mode != Append {
		return refuse(o, "instructions", RefusedInstructionMode, "instruction mode must be replace or append")
	}
	if o.Instructions.Text != "" && o.Instructions.Mode == "" {
		return refuse(o, "instructions", RefusedInstructionModeMissing, "instructions require an explicit replace or append mode")
	}
	return nil
}

func normalizeAPIRestriction(o Options) (Options, error) {
	if o.Restriction == nil {
		return o, refuse(o, "restriction", RefusedNotConfigured, "an API session requires Restriction with a ToolHost naming the caller's tools and handler")
	}
	frozen := *o.Restriction
	host := frozen.Tools
	switch {
	case frozen.Probe != 0:
		return o, refuse(o, "restriction", RefusedConflict, "an API session has no capability probe: its tool surface holds by construction; leave Restriction.Probe unset")
	case host.Dir != "" || host.Bridge.Path != "" || host.Bridge.Args != nil:
		return o, refuse(o, "tools", RefusedConflict, "the library calls an API session's handler directly; leave ToolHost.Dir and ToolHost.Bridge unset")
	}
	if err := host.validateTools(); err != nil {
		return o, toolHostRefusal(o, err)
	}
	frozen.Tools.Tools = freezeTools(host.Tools)
	o.Restriction = &frozen
	return o, nil
}

func normalizeRuntimeHome(o Options) (Options, error) {
	if o.RuntimeHome == "" {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "an API session keeps its conversation in a durable private runtime home; set Options.RuntimeHome")
	}
	home, err := filepath.Abs(o.RuntimeHome)
	if err != nil {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "invalid runtime home")
	}
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "the runtime home must be an existing directory")
	}
	if !privateStateDir(info) {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "the runtime home must be readable only by its owner")
	}
	o.RuntimeHome = home
	return o, nil
}

func normalizeLoop(o Options) (Options, error) {
	l := &o.Loop
	if l.MaxSteps == 0 {
		l.MaxSteps = defaultLoopSteps
	}
	if l.MaxRequestBytes == 0 {
		l.MaxRequestBytes = defaultLoopRequestBytes
	}
	if l.RequestTimeout == 0 {
		l.RequestTimeout = 5 * time.Minute
	}
	if l.MaxSteps < 1 || l.MaxSteps > maxLoopSteps || l.MaxRequestBytes < 1024 || l.MaxRequestBytes > maxLoopRequestBytes || l.RequestTimeout < 0 {
		return o, refuse(o, "loop", RefusedLimit, "a loop bound is out of range")
	}
	return o, nil
}

// normalizeAPISkills checks the skill request. Provided skills are composed by
// completion's own mechanism: an index in the system message and its skill
// tools in every request, answered by AnswerSkillCalls through the tool host.
func normalizeAPISkills(o Options) (Options, error) {
	set := o.Skills
	switch {
	case !set.Global.Known():
		return o, refuse(o, "skills", RefusedGlobalSkillsInvalid, "Skills.Global is not a known value")
	case !set.Delivery.Known():
		return o, refuse(o, "skills", RefusedSkillDeliveryInvalid, "Skills.Delivery is not a known value")
	case set.Global == harness.GlobalSkillsInclude:
		return o, &UnsupportedError{Engine: o.Provider.Engine, Operation: "skills", Code: RefusedGlobalSkills, Capability: harness.Support(o.Provider.Engine, harness.Session, harness.IncludeGlobalSkills)}
	}
	if len(set.Provided) == 0 {
		if o.SkillRun.set() {
			return o, refuse(o, "skills", RefusedConflict, "SkillRun applies only to provided skills that permit scripts")
		}
		return o, nil
	}
	loaded, err := skills.Load(set.Provided)
	if err != nil {
		return o, refuse(o, "skills", skills.Code(err), "a provided skill could not be loaded")
	}
	for _, tool := range o.Restriction.Tools.Tools {
		if skills.IsTool(tool.Name) {
			return o, refuse(o, "tools", RefusedSkillToolReserved, "load_skill and run_skill_script are reserved for the library's skill tools")
		}
	}
	_, scripted := skills.Names(loaded)
	switch {
	case o.SkillRun.set() && len(scripted) == 0:
		return o, refuse(o, "skills", RefusedConflict, "SkillRun applies only to provided skills that permit scripts")
	case len(scripted) > 0 && o.SkillRun.WorkDir == "":
		return o, refuse(o, "skills", skills.CodeWorkDirRequired, "a session offering skill scripts requires SkillRun.WorkDir")
	}
	o.Skills.Provided = slices.Clone(set.Provided)
	o.SkillRun.Env = slices.Clone(o.SkillRun.Env)
	o.skills = &skillPlan{delivery: requestSkills, loaded: loaded}
	return o, nil
}

// apiReference digests what a resume has to match: the engine, the endpoint
// (its base URL and dialect, never a credential), the model, effort,
// instructions, tool server name and skill request. Ref.Home is the runtime
// home, because the conversation lives there: resuming from another is a
// different session wearing the same name. Ref.WorkDir is empty; an API
// session has none. Like a restricted session's, the digest excludes the
// tools themselves, so a release that edits a tool resumes the stored
// conversation under the new surface.
func apiReference(o Options, id string) Ref {
	payload, _ := json.Marshal(struct {
		Engine        harness.Engine
		BaseURL       string
		Dialect       harness.Dialect
		Model, Effort string
		Instructions  Instructions
		ToolServer    string
	}{o.Provider.Engine, o.Provider.API.BaseURL, o.Provider.API.Dialect, o.Model, o.Effort, o.Instructions, o.Restriction.Tools.Server})
	if skills := skillsDigest(o); skills != nil {
		payload, _ = json.Marshal(struct {
			Base   json.RawMessage
			Skills any
		}{payload, skills})
	}
	hash := sha256.Sum256(payload)
	return Ref{Engine: o.Provider.Engine, ID: id, Home: o.RuntimeHome, AccountIdentity: o.AccountIdentity, ConfigHash: hex.EncodeToString(hash[:])}
}

// openAPI starts or resumes a session whose loop the library runs. Its
// options are already normalized and, for a resume, checked against r.
func openAPI(ctx context.Context, o Options, r *Ref) (*Session, error) {
	var (
		ref     Ref
		store   *transcript
		records []record
		err     error
	)
	if r == nil {
		ref = apiReference(o, newID())
		store, records, err = createTranscript(o.RuntimeHome, ref)
	} else {
		ref = *r
		store, records, err = openTranscript(o.RuntimeHome, ref)
	}
	if err != nil {
		return nil, err
	}
	s := &Session{options: o, ref: ref, caps: CapabilitiesFor(o.Provider.Engine), lifetime: ctx, done: make(chan struct{}), opGate: make(chan struct{}, 1), removeSkillFiles: func() {}}
	complete := o.complete
	if complete == nil {
		complete = completion.Complete
	}
	a := &apiSession{store: store, complete: complete, tools: apiTools(o), system: o.Instructions.Text, records: records, responses: countResponses(records), released: make(chan struct{}), failed: s.fail}
	additions, recovery := recoverTranscript(records)
	for _, addition := range additions {
		if err = a.append(addition); err != nil {
			store.close()
			return nil, err
		}
	}
	a.recovery = recovery
	s.api = a
	// No onRefusal: the loop owns every call it hands the host, so it reports
	// each refusal on the turn itself, in order.
	host := newDirectToolHost(apiToolHost(o), apiSkillDefinitions(o)...)
	host.activeTurn = s.activeTurnID
	s.tools = host
	if !conversationBegun(a.records) {
		s.markContext(ContextStarted)
	}
	go func() {
		select {
		case <-ctx.Done():
			s.fail(ctx.Err())
		case <-s.done:
		}
	}()
	return s, nil
}

func (s *Session) activeTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return ""
	}
	return s.active.ID()
}

// apiToolHost is the caller's host with the library's skill calls answered in
// front of the caller's handler.
func apiToolHost(o Options) ToolHost {
	host := o.Restriction.Tools
	if o.skills != nil {
		run := o.SkillRun
		opts := completion.SkillRunOptions{WorkDir: run.WorkDir, Env: run.Env, Timeout: run.Timeout}
		if run.Exec != nil {
			opts.Exec = func(ctx context.Context, c completion.SkillCommand) (completion.SkillOutput, error) {
				out, err := run.Exec(ctx, SkillCommand(c))
				return completion.SkillOutput(out), err
			}
		}
		host.Handler = apiSkillHandler{set: o.Skills, opts: opts, next: host.Handler}
	}
	return host
}

// apiSkillDefinitions are the skill tools the host admits beside the
// caller's. completion adds them to each request itself.
func apiSkillDefinitions(o Options) []ToolDefinition {
	if o.skills == nil {
		return nil
	}
	return skillToolDefinitions(o.skills.loaded)
}

// apiSkillHandler answers the library's skill calls with completion's own
// AnswerSkillCalls, and passes every other call to the caller's handler.
type apiSkillHandler struct {
	set  harness.Skills
	opts completion.SkillRunOptions
	next ToolHandler
}

func (h apiSkillHandler) CallTool(ctx context.Context, call ToolCall) (ToolResult, error) {
	if !skills.IsTool(call.Name) {
		return h.next.CallTool(ctx, call)
	}
	proposal := completion.ToolCall{ID: "skill", Type: "function"}
	proposal.Function.Name, proposal.Function.Arguments = call.Name, string(call.Arguments)
	answers := completion.AnswerSkillCalls(ctx, []completion.ToolCall{proposal}, h.set, h.opts)
	if len(answers) != 1 {
		return ToolResult{Content: call.Name + " error: unanswered", IsError: true}, nil
	}
	content := answers[0].Content
	return ToolResult{Content: content, IsError: strings.HasPrefix(content, call.Name+" error: ")}, nil
}

// apiTools are the caller's tools as function definitions. The skill tools
// are added by completion itself.
func apiTools(o Options) []completion.Tool {
	tools := make([]completion.Tool, 0, len(o.Restriction.Tools.Tools))
	for _, t := range o.Restriction.Tools.Tools {
		tools = append(tools, completion.Tool{Type: "function", Function: completion.Function{Name: t.Name, Description: t.Description, Parameters: t.Schema}})
	}
	return tools
}

// apiConfig is one request's completion configuration.
func (s *Session) apiConfig() completion.Config {
	o := s.options
	return completion.Config{Provider: o.Provider, Model: o.Model, Effort: o.Effort, MaxContextBytes: o.Loop.MaxRequestBytes, Timeout: o.Loop.RequestTimeout, Skills: o.Skills}
}

// append makes a record durable, then adds it to the history. A record that
// cannot be made durable ends the session.
func (a *apiSession) append(r record) error {
	r.At = time.Now().UTC()
	if err := a.store.append(r); err != nil {
		if a.failed != nil {
			a.failed(err)
		}
		return err
	}
	a.mu.Lock()
	a.records = append(a.records, r)
	a.mu.Unlock()
	return nil
}

func (a *apiSession) messages() []completion.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return conversation(a.records, a.system)
}

func (a *apiSession) nextResponse() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.responses++
	return a.responses
}

// begin registers a turn's loop, refusing once the session is stopping.
func (a *apiSession) begin(cancel context.CancelFunc) (chan struct{}, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		return nil, ErrClosed
	}
	done := make(chan struct{})
	a.cancelTurn, a.loopDone = cancel, done
	return done, nil
}

func (a *apiSession) interrupt() {
	a.mu.Lock()
	cancel := a.cancelTurn
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// shutdown stops the running turn and, once its loop has returned and every
// call the host admitted has settled, closes the transcript and gives up the
// lock. A handler still running keeps the conversation locked, so no other
// session can resume it and answer that call as unknown while it may still be
// taking effect; its result is recorded when it returns.
func (a *apiSession) shutdown(host *toolHost) {
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		return
	}
	a.stopping = true
	cancel, loop := a.cancelTurn, a.loopDone
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	go func() {
		if loop != nil {
			<-loop
		}
		if host != nil {
			host.mu.Lock()
			settled := host.settled
			host.mu.Unlock()
			<-settled
		}
		a.store.close()
		close(a.released)
	}()
}
