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
	Available      Feature = "available"
	WorkspaceRead  Feature = "workspace_read"  // confined read-only workspace tools
	WorkspaceWrite Feature = "workspace_write" // workspace mutation

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
	RestrictTools       Feature = "restrict_tools" // only configured tools, proven before launch
	Sandbox             Feature = "sandbox"        // native tools or workbench commands inside a proven OS sandbox
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
	// ToolActivity: session tool events carry the tool's arguments and its
	// result text, bounded, as the harness reported them.
	ToolActivity Feature = "tool_activity"
	// Loopback: a sandboxed session may bind and connect to this machine's own
	// addresses only, proven before launch, while every other host stays
	// closed.
	Loopback Feature = "loopback"
	// Browser: the browser integration the harness itself ships, switched on
	// for this invocation. Off unless asked for.
	Browser Feature = "browser"
	// SandboxedBrowser: Browser is admitted in a session that also sets
	// Sandbox. A caller that runs every session sandboxed asks this before
	// offering the browser at all.
	SandboxedBrowser Feature = "sandboxed_browser"
	// Background: the harness and everything it starts run at background
	// priority, so agent work yields to the machine's interactive use.
	Background Feature = "background"
	// ToolImages: tool_completed events carry the images a tool returned,
	// such as a browser screenshot, decoded and bounded.
	ToolImages Feature = "tool_images"
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
// sandboxed hosting of a CLI harness are unavailable on Windows. An API
// session has no process to contain, so it is unaffected.
func platform(e Engine, op Operation, f Feature, c Capability) Capability {
	if e == OpenAICompatible && op == Session && runtime.GOOS == "linux" {
		switch f {
		case Sandbox:
			return Capability{Unknown, "Workbench.Commands proves bubblewrap 0.8.0+ without capabilities or nested user namespaces before launch or refuses the session"}
		case Loopback:
			return Capability{Unsupported, "each command runs in its own bubblewrap network namespace, so a server one command starts is gone before the next; a single command may still start and request its own server"}
		case Background:
			return Capability{Composed, "Workbench.Commands runs each contained process group at nice 10"}
		}
	}
	if e == OpenAICompatible && op == Session && (f == Sandbox || f == Loopback || f == Background) && runtime.GOOS != "darwin" {
		return Capability{Unsupported, "API commands require a proved macOS Seatbelt sandbox; this platform has no command sandbox"}
	}
	if e == OpenAICompatible && op == Session && (f == WorkspaceRead || f == WorkspaceWrite) && (runtime.GOOS == "linux" || runtime.GOOS == "darwin") {
		return Capability{Unsupported, "workbench file tools are off until their workspace check is verified"}
	}
	if e == OpenAICompatible && op == Session && f == WorkspaceWrite && runtime.GOOS == "windows" {
		c.Reason += "; not directory-synced; new files inherit the directory ACL"
	}
	if (f == WorkspaceRead || f == WorkspaceWrite) && runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		return Capability{Unsupported, "workspace mount checks are unavailable on this platform"}
	}
	if runtime.GOOS == "windows" && op == Session && e.Transport() == CLITransport && (f == RestrictTools || f == Sandbox || f == Tools || f == Loopback) {
		if e == Codex && f == Loopback {
			return Capability{Unsupported, "restricted and sandboxed hosting are unavailable on Windows; " + c.Reason}
		}
		return Capability{Unsupported, "restricted and sandboxed hosting are unavailable on Windows"}
	}
	if runtime.GOOS == "windows" && f == Background {
		return Capability{Unsupported, "background priority is not implemented on Windows"}
	}
	if runtime.GOOS == "windows" && e == CommandCode && op == Session {
		return Capability{Unsupported, "Command Code sessions are not verified on Windows, where cmd names the system command interpreter"}
	}
	if runtime.GOOS == "windows" && e == Grok && op == Complete {
		return Capability{Unsupported, "Grok's restricted runtime home is unavailable on Windows"}
	}
	return c
}

var (
	native     = Capability{Availability: Native}
	unverified = Capability{Unknown, "not verified against the installed harness"}
	// Claude in Chrome is not a sandboxed browser: it is the operator's own.
	claudeBrowser = Capability{Native, "Claude in Chrome (--chrome) drives the operator's real Chrome through its extension, outside any sandbox, reaching whatever the extension's site permissions allow with that profile's logins"}
	codexBrowser  = Capability{Native, "codex-cli browser_use and browser_use_external features select the configured native browser bridge; requires that bridge in the selected CLI home and the ChatGPT Chrome extension; uses the browser profile's logins and site permissions outside the shell sandbox; sandboxed sessions require an isolated bridge and a pre-launch JavaScript confinement proof; not available with Restriction"}
	cacheVaries   = Capability{Unknown, "reported only by endpoints that split cached input"}
	background    = Capability{Native, "the process group is niced to 10, which descendants inherit"}
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

	{OpenAICompatible, Session, MaxOutputTokens}:  {Native, "Loop.MaxOutputTokens, sent as max_completion_tokens"},
	{Claude, Complete, MaxOutputTokens}:           {Native, "CLAUDE_CODE_MAX_OUTPUT_TOKENS, proven by the capability probe"},
	{OpenAICompatible, Complete, MaxOutputTokens}: {Native, "sent as max_completion_tokens"},
	{Codex, Complete, MaxOutputTokens}:            {Unsupported, "codex exec sends no output limit"},
	{Grok, Complete, MaxOutputTokens}:             {Unsupported, "the catalog model's own limit can override it, and no probe can prove it"},

	{Claude, Run, ProvidedSkills}:          {Native, "loaded through a private --plugin-dir"},
	{Codex, Run, ProvidedSkills}:           {Composed, "an index with each SKILL.md's path is added to the developer instructions"},
	{Grok, Run, ProvidedSkills}:            {Composed, "an index with each SKILL.md's path is added through --rules"},
	{Claude, Run, IncludeGlobalSkills}:     native,
	{Codex, Run, IncludeGlobalSkills}:      native,
	{Grok, Run, IncludeGlobalSkills}:       native,
	{Claude, Session, ProvidedSkills}:      {Native, "a private --plugin-dir; composed when sandboxed or restricted"},
	{Codex, Session, ProvidedSkills}:       {Composed, "instructions or hosted tools; native in sandboxed sessions through the runtime home"},
	{Grok, Session, ProvidedSkills}:        {Native, "grok agent --plugin-dir"},
	{Claude, Session, IncludeGlobalSkills}: {Native, "ordinary sessions only"},
	{Codex, Session, IncludeGlobalSkills}:  {Native, "ordinary sessions only"},
	{Grok, Session, IncludeGlobalSkills}:   {Native, "ordinary sessions only"},

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
	{Codex, Session, Effort}:               {Native, "sent with each turn; a restricted session checks it against the installed CLI's model catalog"},
	{Codex, Session, CacheSplit}:           native,
	{Codex, Session, ContextWindow}:        unverified,
	{Codex, Session, ToolActivity}:         {Native, "item arguments, aggregated output, exit code and MCP results from the app-server's own items"},
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
	{Claude, Session, Effort}:              native,
	{Claude, Session, CacheSplit}:          native,
	{Claude, Session, ContextWindow}:       unverified,
	{Claude, Session, ToolActivity}:        {Native, "tool_use input and tool_result content from the stream"},
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
	{Grok, Session, Effort}:                {Native, "checked against the session's model state before the first turn"},
	{Grok, Session, CacheSplit}:            native,
	{Grok, Session, ContextWindow}:         {Unknown, "estimated from each response's input against the window Grok's model state states"},
	{Grok, Session, ToolActivity}:          {Native, "the agent protocol's rawInput, and rawOutput or content on a tool call's final update"},

	{Codex, Models, Available}:                native,
	{Codex, Models, Effort}:                   native,
	{Codex, Models, ContextWindow}:            {Unknown, "the window the installed CLI's bundled model catalog states; the account's service may differ"},
	{Claude, Models, Available}:               native,
	{Claude, Models, Effort}:                  native,
	{Grok, Models, Available}:                 native,
	{Grok, Models, Effort}:                    native,
	{Grok, Models, ContextWindow}:             native,
	{OpenAICompatible, Models, Available}:     native,
	{OpenAICompatible, Models, Effort}:        {Unsupported, "endpoints do not list efforts"},
	{OpenAICompatible, Models, ContextWindow}: {Unknown, "reported only by some gateways"},

	{OpenAICompatible, Run, Available}:               {Unsupported, "an API endpoint has no native agent; use Complete"},
	{OpenAICompatible, Session, WorkspaceRead}:       {Composed, "the library's read_file, list_files and search_files: regular, singly linked files reached inside WorkDir on WorkDir's own mount"},
	{OpenAICompatible, Session, WorkspaceWrite}:      {Composed, "the library's write_file and edit_file, inside WorkDir on WorkDir's own mount; .git is never written; symlinks are refused, not written through; each write is an atomic, synced replacement"},
	{OpenAICompatible, Session, Available}:           {Composed, "the library runs the agent loop over the endpoint, keeping the conversation in RuntimeHome"},
	{OpenAICompatible, Session, Resume}:              {Composed, "the library reloads its own transcript; a call interrupted mid-run is answered as an unknown outcome, never re-run"},
	{OpenAICompatible, Session, Interrupt}:           {Composed, "the library cancels the in-flight request and the running handler"},
	{OpenAICompatible, Session, Steer}:               {Composed, "the library interrupts the turn and starts another"},
	{OpenAICompatible, Session, RestrictTools}:       {Composed, "only the caller's hosted tools and the library's workbench tools exist: the library writes every request itself"},
	{OpenAICompatible, Session, Tools}:               {Composed, "the library calls the caller's handler directly, one call at a time"},
	{OpenAICompatible, Session, AppendInstructions}:  {Composed, "sent as the leading system message"},
	{OpenAICompatible, Session, ReplaceInstructions}: {Composed, "sent as the leading system message; an endpoint has no base prompt to replace"},
	{OpenAICompatible, Session, Effort}:              {Native, "requires API.EffortParameter"},
	{OpenAICompatible, Session, ProvidedSkills}:      {Composed, "the library indexes provided skills and answers its read-only skill tool; a permitted skill's scripts run as SkillRun says"},
	{OpenAICompatible, Session, IncludeGlobalSkills}: {Unsupported, "an API endpoint has no installed skills; only Default or Exclude is accepted"},
	{OpenAICompatible, Session, Sandbox}:             {Unknown, "Workbench.Commands proves its pinned Seatbelt profile before launch or refuses the session"},
	{OpenAICompatible, Session, Loopback}:            {Unknown, "Workbench.Commands.Loopback requires a positive localhost and negative off-machine canary before launch"},
	{OpenAICompatible, Session, Background}:          {Composed, "Workbench.Commands runs each contained process group at nice 10"},
	{OpenAICompatible, Session, Compact}:             {Unsupported, "the library does not compact a composed session's history"},
	{OpenAICompatible, Session, CacheSplit}:          cacheVaries,
	{OpenAICompatible, Session, ContextWindow}:       {Unknown, "estimated from each response's input; Chat Completions states no window"},
	{OpenAICompatible, Session, ToolActivity}:        {Composed, "the library reports the model's call arguments and the handler's result as it ran them"},
	{OpenAICompatible, Account, Available}:           {Unsupported, "API endpoints expose no account inspection"},

	{Claude, Session, Loopback}:         {Unknown, "Claude Code's sandbox allowLocalBinding, which admits this machine's own addresses; proved before each launch by a canary that must reach and bind loopback and be refused an off-machine address"},
	{Codex, Session, Loopback}:          {Unsupported, "codex-cli 0.160.0 native loopback is unproved: owner-observed macOS tests found closed network refused loopback, while enabled network with localhost domain rules, including a proxy variant and a native session, allowed off-machine TCP 443 and TCP/UDP port 53; other platforms have no native enforcement proof; use a closed-network sandbox or a separately proved command sandbox"},
	{Grok, Session, Loopback}:           {Unsupported, "Grok sessions have no proven OS sandbox"},
	{Codex, Run, Loopback}:              {Unsupported, "a native run has no library-proven sandbox to scope networking in"},
	{Claude, Run, Loopback}:             {Unsupported, "a native run has no library-proven sandbox to scope networking in"},
	{Grok, Run, Loopback}:               {Unsupported, "a native run has no library-proven sandbox to scope networking in"},
	{Claude, Session, Background}:       background,
	{Codex, Session, Background}:        background,
	{Grok, Session, Background}:         background,
	{Claude, Run, Background}:           background,
	{Codex, Run, Background}:            background,
	{Grok, Run, Background}:             background,
	{Claude, Session, ToolImages}:       {Native, "image blocks in Claude Code's tool results, checked with a claude-in-chrome screenshot"},
	{Codex, Session, ToolImages}:        {Native, "generated images in codex-cli 0.159.2's imageGeneration results, checked through a native session; MCP image content as the app-server protocol declares it"},
	{Grok, Session, ToolImages}:         {Unknown, "image content blocks in an ACP tool call update, as the protocol declares them; not seen from a real tool"},
	{Claude, Session, Browser}:          claudeBrowser,
	{Claude, Run, Browser}:              claudeBrowser,
	{Codex, Session, Browser}:           codexBrowser,
	{Codex, Run, Browser}:               codexBrowser,
	{Grok, Session, Browser}:            {Unsupported, "Grok 1.0.41 ships no browser integration"},
	{Claude, Session, SandboxedBrowser}: {Native, "a sandboxed Claude session admits the browser's tools beside its own, except those that would read local files or start another agent; the browser itself runs outside the sandbox"},
	{Codex, Session, SandboxedBrowser}:  {Native, "the ChatGPT app node_repl bridge runs JavaScript confined by the session sandbox, proven before launch; drives the owner's real Chrome with its logins; Chrome backend only, no computer use"},
	{Grok, Run, Browser}:                {Unsupported, "Grok 1.0.41 ships no browser integration"},

	// Command Code is offered only as a session over its agent protocol,
	// `cmd acp`, checked against Command Code 1.74.1.
	{CommandCode, Complete, Available}:          {Unsupported, "no tool-free Command Code mode has been proven; use Session"},
	{CommandCode, Run, Available}:               {Unsupported, "Command Code is offered only as a session"},
	{CommandCode, Models, Available}:            {Unsupported, "Command Code lists its models only inside a session's configuration"},
	{CommandCode, Account, Available}:           {Unsupported, "Command Code's agent protocol exposes no account inspection"},
	{CommandCode, Session, Available}:           unverified,
	{CommandCode, Session, Resume}:              {Unknown, "session/resume, once session/list shows Command Code holds the conversation for WorkDir: it resumes an unknown id as an empty conversation"},
	{CommandCode, Session, Interrupt}:           unverified,
	{CommandCode, Session, Steer}:               {Composed, "Command Code steering cancels the running prompt and sends another"},
	{CommandCode, Session, Effort}:              {Native, "set through session/set_config_option and checked against the configuration the session reports before the first turn"},
	{CommandCode, Session, CacheSplit}:          native,
	{CommandCode, Session, ContextWindow}:       {Unknown, "Command Code's own used and size figures from the usage_update each prompt ends with"},
	{CommandCode, Session, ToolActivity}:        {Native, "the agent protocol's rawInput, and rawOutput or content on a tool call's final update"},
	{CommandCode, Session, ToolImages}:          {Unknown, "image content blocks in an ACP tool call update, as the protocol declares them; not seen from a real tool"},
	{CommandCode, Session, Background}:          background,
	{CommandCode, Session, IncludeGlobalSkills}: {Native, "Command Code loads its installed skills itself"},
	{CommandCode, Session, ProvidedSkills}:      {Unsupported, "cmd acp takes no skill paths and Command Code sessions have no instructions to carry a skill index"},
	{CommandCode, Session, AppendInstructions}:  {Unsupported, "cmd acp's session/new takes no instructions"},
	{CommandCode, Session, ReplaceInstructions}: {Unsupported, "cmd acp's session/new takes no instructions"},
	{CommandCode, Session, Compact}:             {Unsupported, "Command Code exposes no verified manual compaction control; its /compact command is not used"},
	{CommandCode, Session, RestrictTools}:       {Unsupported, "Command Code sessions have no proven removal of native tools"},
	{CommandCode, Session, Sandbox}:             {Unsupported, "Command Code sessions have no proven OS sandbox"},
	{CommandCode, Session, Loopback}:            {Unsupported, "Command Code sessions have no proven OS sandbox"},
	{CommandCode, Session, Tools}:               {Unsupported, "Command Code sessions do not host caller tools"},
	{CommandCode, Session, Browser}:             {Unsupported, "the library switches on no Command Code browser integration"},

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
