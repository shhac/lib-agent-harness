package session

// Sessions over an OpenAI-compatible endpoint. An endpoint is only a model, so
// the library is the agent: each turn calls the model through completion's
// transport, runs every proposed call through the caller's handler one at a
// time, answers the library's own skill and workbench calls, and repeats until
// the model answers without calls. The Session, Turn, Event and Result shapes
// are the CLIs'.
//
// Only the caller's hosted tools, the library's skill tools and, with a
// Workbench, the library's workbench tools exist. The
// library writes every request itself, so the restricted surface holds by
// construction rather than by probe, and no bridge is involved: the handlers
// are called directly, under the same admission, serialization, closing-tool
// and settlement rules as a hosted channel.
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
	"os"
	"slices"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
	"github.com/shhac/lib-agent-harness/internal/skills"
	"github.com/shhac/lib-agent-harness/sandbox"
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
	// RequestTimeout bounds one model request. Zero means five minutes, or
	// thirty for an endpoint with API.Streaming, whose IdleTimeout catches a
	// stall.
	RequestTimeout time.Duration
	// MaxOutputTokens caps the tokens each model response may produce, sent as
	// max_completion_tokens. Zero means no cap. A response the cap cuts short
	// fails its turn with cause output_truncated rather than passing as done.
	MaxOutputTokens int
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

// openAPI starts or resumes a session whose loop the library runs. Its
// options are already normalized and, for a resume, checked against r.
func openAPI(ctx context.Context, o Options, r *Ref) (*Session, error) {
	if err := proveWorkbench(ctx, o); err != nil {
		return nil, err
	}
	var (
		ref     Ref
		store   *transcript
		records []record
		err     error
	)
	// The workspace opens first, so a workspace that cannot be opened leaves
	// no transcript behind.
	if r == nil {
		ref = apiReference(o, newID())
	} else {
		ref = *r
	}
	ws, err := openWorkspace(o, ref.ID)
	if err != nil {
		return nil, err
	}
	if r == nil {
		store, records, err = createTranscript(o.RuntimeHome, ref)
	} else {
		store, records, err = openTranscript(o.RuntimeHome, ref)
	}
	if err != nil {
		ws.close()
		return nil, err
	}
	if ws != nil {
		if err = setupWorkbenchCommands(ws, o, ref.ID); err != nil {
			store.close()
			ws.close()
			if r == nil {
				_ = os.RemoveAll(sessionDir(o.RuntimeHome, ref.ID))
			}
			return nil, err
		}
		if err = ws.cleanupWrites(records); err != nil {
			if ws.commands != nil {
				ws.fromSandbox(ws.commands.Close())
			}
			store.close()
			ws.close()
			if r == nil {
				_ = os.RemoveAll(sessionDir(o.RuntimeHome, ref.ID))
			}
			return nil, err
		}
	}
	s := &Session{options: o, ref: ref, caps: CapabilitiesFor(o.Provider.Engine), lifetime: ctx, done: make(chan struct{}), opGate: make(chan struct{}, 1), removeSkillFiles: func() {}}
	complete := o.complete
	if complete == nil {
		complete = completion.Complete
	}
	s.caps.WorkbenchTools = append(s.caps.WorkbenchTools, workbenchCapabilities(o)...)
	a := &apiSession{store: store, workspace: ws, complete: complete, tools: apiTools(o), system: o.Instructions.Text, resultLimit: o.Restriction.Tools.resultLimit(), records: records, responses: countResponses(records), released: make(chan struct{}), failed: s.fail}
	if err = restoreAPIRecovery(a, ws, o, ref, r == nil); err != nil {
		return nil, err
	}
	s.api = a
	if ws != nil {
		ws.setFailed(s.failWorkspace)
	}
	// No onRefusal: the loop owns every call it hands the host, so it reports
	// each refusal on the turn itself, in order.
	if o.Workbench != nil && sandbox.ToolAvailability("read_file") != "" {
		a.system += "\nWorkbench tools read_file, search_files and edit_file are unavailable: " + sandbox.FileToolsDisabledReason + "."
	}
	host := newDirectToolHost(apiToolHost(o, ws), append(apiSkillDefinitions(o), workbenchDefinitions(o)...)...)
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

// apiToolHost is the caller's host with the library's workbench and skill
// calls answered, in that order, in front of the caller's handler.
func apiToolHost(o Options, ws *workbenchHost) ToolHost {
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
	if ws != nil {
		host.Handler = workbenchHandler{ws: ws, next: host.Handler}
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

// apiTools are the caller's tools, then the workbench's, as function
// definitions. The skill tools are added by completion itself.
func apiTools(o Options) []completion.Tool {
	return completionTools(append(slices.Clone(o.Restriction.Tools.Tools), workbenchDefinitions(o)...))
}

// apiConfig is one request's completion configuration.
func (s *Session) apiConfig() completion.Config {
	o := s.options
	return completion.Config{Provider: o.Provider, Model: o.Model, Effort: o.Effort, MaxContextBytes: o.Loop.MaxRequestBytes, Timeout: o.Loop.RequestTimeout, MaxOutputTokens: o.Loop.MaxOutputTokens, Skills: o.Skills}
}

// restoreAPIRecovery writes unknown outcomes before admitting any tool. A
// failure gives up prepared resources; a fresh failed session leaves no state.
func restoreAPIRecovery(a *apiSession, ws *workbenchHost, o Options, ref Ref, fresh bool) error {
	additions, recovery := recoverTranscript(a.records)
	for _, addition := range additions {
		addition.Text = bound(addition.Text, a.resultLimit)
		if err := a.append(addition); err != nil {
			if ws != nil && ws.commands != nil {
				_ = ws.fromSandbox(ws.commands.Close())
			}
			a.store.close()
			ws.close()
			if fresh {
				_ = os.RemoveAll(sessionDir(o.RuntimeHome, ref.ID))
			}
			return err
		}
	}
	a.recovery = recovery
	return nil
}
