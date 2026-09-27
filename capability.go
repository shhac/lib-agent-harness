package harness

import "runtime"

// Availability distinguishes how, and whether, an engine offers something.
type Availability string

const (
	// Native: the harness's own protocol provides it.
	Native Availability = "native"
	// Composed: the library builds it from other native operations.
	Composed Availability = "composed"
	// Unsupported: refused before any work starts.
	Unsupported Availability = "unsupported"
	// Unknown: not established until checked against the installed harness.
	Unknown Availability = "unknown"
)

// Capability is an availability with a library-authored reason.
type Capability struct {
	Availability Availability `json:"availability"`
	Reason       string       `json:"reason,omitempty"`
}

// Usable reports whether a caller may attempt it: native or composed, or
// unknown until the installed harness answers.
func (c Capability) Usable() bool { return c.Availability != Unsupported && c.Availability != "" }

// Operation is a kind of invocation.
type Operation string

const (
	// Complete: the model returns text and proposes the caller's tools; native
	// tools are proven absent (package completion).
	Complete Operation = "complete"
	// Run: one native agent invocation (package native).
	Run Operation = "run"
	// Session: a persistent native agent with turns (package session).
	Session Operation = "session"
	// Models: listing the models and efforts an engine offers.
	Models Operation = "models"
	// Account: login, plan, quota windows and credits.
	Account Operation = "account"
)

// Operations lists every operation, in a stable order for display.
func Operations() []Operation { return []Operation{Complete, Run, Session, Models, Account} }

// Feature is something an operation may offer.
type Feature string

const (
	// Available is the operation itself.
	Available Feature = "available"

	Effort           Feature = "effort"
	Tools            Feature = "tools"             // caller-defined tools: proposed (Complete) or hosted (Session)
	StructuredOutput Feature = "structured_output" // a report constrained by the caller's JSON schema
	// ProgressMessages: before its final report the agent may send messages
	// the schema does not constrain. Codex applies the schema to every message.
	ProgressMessages    Feature = "progress_messages"
	Resume              Feature = "resume"
	Interrupt           Feature = "interrupt"
	Steer               Feature = "steer"
	Compact             Feature = "compact"
	AppendInstructions  Feature = "append_instructions"
	ReplaceInstructions Feature = "replace_instructions"
	RestrictTools       Feature = "restrict_tools" // only the caller's tools, proven before launch
	Sandbox             Feature = "sandbox"        // native tools inside a proven OS sandbox
	CostReport          Feature = "cost"           // the harness values its token spend
	CacheSplit          Feature = "cache_split"    // cached input is reported apart from fresh
	ContextWindow       Feature = "context_window" // the serving model's window is stated
	// MaxOutputTokens: a caller-set cap on the tokens one response may produce.
	MaxOutputTokens Feature = "max_output_tokens"
	Login           Feature = "login"
	Quota           Feature = "quota"   // subscription usage windows
	Credits         Feature = "credits" // prepaid or overage balance
	// ProvidedSkills: caller-provided skills (Skills.Provided) made available
	// for one invocation. Native where the harness loads them itself, composed
	// where the library does. (The names Skills and GlobalSkills are the
	// request types in skills.go.)
	ProvidedSkills Feature = "skills"
	// IncludeGlobalSkills: including the harness's own installed skills
	// (Skills.Global Include).
	IncludeGlobalSkills Feature = "global_skills"
)

type supportKey struct {
	engine    Engine
	operation Operation
	feature   Feature
}

// Support is the library's static claim about an engine. Unknown entries are
// promoted only by evidence from the installed harness; nothing promotes past
// this table. A caller asks it instead of comparing engine names.
func Support(e Engine, op Operation, f Feature) Capability {
	if e.Transport() == "" {
		return Capability{Unsupported, "unrecognized engine"}
	}
	if f != Available {
		if operation := Support(e, op, Available); !operation.Usable() {
			return operation
		}
	}
	if c, ok := supportTable[supportKey{e, op, f}]; ok {
		return platform(e, op, f, c)
	}
	return Capability{Unsupported, "not offered by this engine"}
}

// platform applies what the operating system rules out: restricted and
// sandboxed hosting are unavailable on Windows.
func platform(e Engine, op Operation, f Feature, c Capability) Capability {
	if runtime.GOOS == "windows" && op == Session && (f == RestrictTools || f == Sandbox || f == Tools) {
		return Capability{Unsupported, "restricted and sandboxed hosting are unavailable on Windows"}
	}
	if runtime.GOOS == "windows" && e == Grok && op == Complete {
		return Capability{Unsupported, "Grok's restricted runtime home is unavailable on Windows"}
	}
	return c
}

var (
	native      = Capability{Availability: Native}
	unverified  = Capability{Unknown, "not verified against the installed harness"}
	cacheVaries = Capability{Unknown, "reported only by endpoints that split cached input"}
	// Constrained completion has no native tools, so no engine loads skills
	// there; the library composes them for every engine instead.
	composedSkills = Capability{Composed, "the library indexes provided skills and answers a read-only skill tool; a permitted skill's scripts run only when the caller answers those calls"}
	noGlobalSkills = Capability{Unsupported, "constrained completion never loads installed skills; only Default or Exclude is accepted"}
)

var supportTable = map[supportKey]Capability{
	{Codex, Complete, Available}:             {Native, "native tools are proven absent before each request"},
	{Codex, Complete, Effort}:                {Native, "checked against the installed CLI's model catalog"},
	{Codex, Complete, Tools}:                 native,
	{Codex, Complete, CacheSplit}:            native,
	{Claude, Complete, Available}:            {Native, "native tools are proven absent before each request"},
	{Claude, Complete, Effort}:               native,
	{Claude, Complete, Tools}:                native,
	{Claude, Complete, CacheSplit}:           native,
	{Claude, Complete, CostReport}:           native,
	{Claude, Complete, ContextWindow}:        native,
	{Grok, Complete, Available}:              {Native, "native tools and instructions are proven against a local refusing provider before the first launch of each binary and flag set, and checked again in each run's stream and transcript; unavailable on Windows"},
	{Grok, Complete, Effort}:                 native,
	{Grok, Complete, Tools}:                  native,
	{Grok, Complete, CacheSplit}:             native,
	{Grok, Complete, CostReport}:             native,
	{OpenAICompatible, Complete, Available}:  native,
	{OpenAICompatible, Complete, Effort}:     {Native, "requires API.EffortParameter"},
	{OpenAICompatible, Complete, Tools}:      native,
	{OpenAICompatible, Complete, CacheSplit}: cacheVaries,

	{Claude, Complete, MaxOutputTokens}:           {Native, "CLAUDE_CODE_MAX_OUTPUT_TOKENS, proven by the capability probe"},
	{OpenAICompatible, Complete, MaxOutputTokens}: {Native, "sent as max_completion_tokens"},
	{Codex, Complete, MaxOutputTokens}:            {Unsupported, "codex exec sends no output limit"},
	{Grok, Complete, MaxOutputTokens}:             {Unsupported, "the catalog model's own limit can override it, and no probe can prove it"},

	{Codex, Complete, ProvidedSkills}:                 composedSkills,
	{Claude, Complete, ProvidedSkills}:                composedSkills,
	{Grok, Complete, ProvidedSkills}:                  composedSkills,
	{OpenAICompatible, Complete, ProvidedSkills}:      composedSkills,
	{Codex, Complete, IncludeGlobalSkills}:            noGlobalSkills,
	{Claude, Complete, IncludeGlobalSkills}:           noGlobalSkills,
	{Grok, Complete, IncludeGlobalSkills}:             noGlobalSkills,
	{OpenAICompatible, Complete, IncludeGlobalSkills}: noGlobalSkills,

	{Codex, Run, Available}:         native,
	{Codex, Run, Effort}:            native,
	{Codex, Run, StructuredOutput}:  native,
	{Codex, Run, Resume}:            native,
	{Codex, Run, CacheSplit}:        native,
	{Claude, Run, Available}:        native,
	{Claude, Run, Effort}:           native,
	{Claude, Run, StructuredOutput}: native,
	{Claude, Run, ProgressMessages}: native,
	{Claude, Run, Resume}:           native,
	{Claude, Run, CostReport}:       native,
	{Claude, Run, CacheSplit}:       native,
	{Grok, Run, Available}:          native,
	{Grok, Run, Effort}:             native,
	{Grok, Run, StructuredOutput}:   native,
	{Grok, Run, ProgressMessages}:   native,
	{Grok, Run, Resume}:             native,
	{Grok, Run, CostReport}:         native,
	{Grok, Run, CacheSplit}:         native,

	{Codex, Session, Available}:            unverified,
	{Codex, Session, Resume}:               unverified,
	{Codex, Session, Interrupt}:            unverified,
	{Codex, Session, Steer}:                unverified,
	{Codex, Session, Compact}:              unverified,
	{Codex, Session, AppendInstructions}:   unverified,
	{Codex, Session, ReplaceInstructions}:  unverified,
	{Codex, Session, RestrictTools}:        unverified,
	{Codex, Session, Sandbox}:              unverified,
	{Codex, Session, Tools}:                unverified,
	{Codex, Session, CacheSplit}:           native,
	{Codex, Session, ContextWindow}:        unverified,
	{Claude, Session, Available}:           unverified,
	{Claude, Session, Resume}:              unverified,
	{Claude, Session, Interrupt}:           unverified,
	{Claude, Session, Steer}:               unverified,
	{Claude, Session, Compact}:             {Unsupported, "Claude exposes no verified manual compaction control protocol"},
	{Claude, Session, AppendInstructions}:  unverified,
	{Claude, Session, ReplaceInstructions}: unverified,
	{Claude, Session, RestrictTools}:       unverified,
	{Claude, Session, Sandbox}:             unverified,
	{Claude, Session, Tools}:               unverified,
	{Claude, Session, CacheSplit}:          native,
	{Claude, Session, ContextWindow}:       unverified,
	{Grok, Session, Available}:             unverified,
	{Grok, Session, Resume}:                unverified,
	{Grok, Session, Interrupt}:             unverified,
	{Grok, Session, Steer}:                 {Composed, "Grok steering cancels the running prompt and sends another"},
	{Grok, Session, Compact}:               {Unsupported, "Grok exposes no verified manual compaction control; its /compact command and compaction extension are not used"},
	{Grok, Session, AppendInstructions}:    unverified,
	{Grok, Session, ReplaceInstructions}:   unverified,
	{Grok, Session, RestrictTools}:         {Unsupported, "Grok sessions have no proven removal of native tools; use Complete for tool-free Grok"},
	{Grok, Session, Sandbox}:               {Unsupported, "Grok sessions have no proven OS sandbox"},
	{Grok, Session, Tools}:                 {Unsupported, "Grok sessions do not host caller tools"},
	{Grok, Session, CacheSplit}:            native,
	{Grok, Session, ContextWindow}:         {Unknown, "estimated from each response's input against the window Grok's model state states"},

	{Codex, Models, Available}:                native,
	{Codex, Models, Effort}:                   native,
	{Claude, Models, Available}:               native,
	{Claude, Models, Effort}:                  native,
	{Grok, Models, Available}:                 native,
	{Grok, Models, Effort}:                    native,
	{Grok, Models, ContextWindow}:             native,
	{OpenAICompatible, Models, Available}:     native,
	{OpenAICompatible, Models, Effort}:        {Unsupported, "endpoints do not list efforts"},
	{OpenAICompatible, Models, ContextWindow}: {Unknown, "reported only by some gateways"},

	{OpenAICompatible, Run, Available}:     {Unsupported, "an API endpoint has no native agent; use Complete"},
	{OpenAICompatible, Session, Available}: {Unsupported, "remote sessions are not implemented"},
	{OpenAICompatible, Account, Available}: {Unsupported, "API endpoints expose no account inspection"},

	{Codex, Account, Available}:  unverified,
	{Codex, Account, Login}:      unverified,
	{Codex, Account, Quota}:      unverified,
	{Claude, Account, Available}: unverified,
	{Claude, Account, Login}:     unverified,
	{Claude, Account, Quota}:     unverified,
	{Codex, Account, Credits}:    unverified,
	{Claude, Account, Credits}:   unverified,
	{Grok, Account, Available}:   {Native, "read over Grok's agent protocol without a session"},
	{Grok, Account, Login}:       native,
	{Grok, Account, Quota}:       {Unsupported, "Grok exposes no quota windows"},
	{Grok, Account, Credits}:     {Unsupported, "Grok exposes no credit balance"},
}
