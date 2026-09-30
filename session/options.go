package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/jsonschema"
)

// normalize resolves a caller's options into the ones a session runs with,
// refusing any it cannot honour. Its stages run in order.
func normalize(o Options) (Options, error) {
	if err := supported(o); err != nil {
		return o, err
	}
	if err := normalizeBrowser(o); err != nil {
		return o, err
	}
	if err := normalizeBackground(o); err != nil {
		return o, err
	}
	if o.Provider.Engine.Transport() == harness.APITransport {
		return normalizeAPI(o)
	}
	if o.Loop != (Loop{}) {
		return o, refuse(o, "loop", RefusedOtherEnginePolicy, "Loop bounds the library's own agent loop, which only an OpenAI-compatible session runs; leave it unset")
	}
	o, err := normalizePaths(o)
	if err != nil {
		return o, err
	}
	if o, err = normalizeLimits(o); err != nil {
		return o, err
	}
	if o.Instructions.Mode != "" && o.Instructions.Mode != Replace && o.Instructions.Mode != Append {
		return o, refuse(o, "instructions", RefusedInstructionMode, "instruction mode must be replace or append")
	}
	if o.Instructions.Text != "" && o.Instructions.Mode == "" {
		return o, refuse(o, "instructions", RefusedInstructionModeMissing, "instructions require an explicit replace or append mode")
	}
	if err = validateEnv(o); err != nil {
		return o, err
	}
	o.Env = append([]string(nil), o.Env...)
	// Both of these judge the policy the caller set, so they go before the
	// policy defaults, after which neither could tell which values those were.
	if err = otherEnginePolicy(o); err != nil {
		return o, err
	}
	if o.Sandbox != nil {
		if o, err = normalizeSandbox(o); err != nil {
			return o, err
		}
	}
	if o, err = normalizePolicy(o); err != nil {
		return o, err
	}
	if o.Restriction != nil {
		if o, err = normalizeRestriction(o); err != nil {
			return o, err
		}
	}
	return normalizeSkills(o)
}

// supported refuses an engine the library offers no session for, and a
// provider that is not well formed.
func supported(o Options) error {
	engine := o.Provider.Engine
	if support := harness.Support(engine, harness.Session, harness.Available); !support.Usable() {
		return &UnsupportedError{Engine: engine, Operation: "engine", Code: RefusedEngine, Capability: support}
	}
	if code := o.Provider.Problem(); code != "" {
		return refuse(o, "provider", code, "the provider is not well formed")
	}
	return unsupportedMode(o)
}

// unsupportedMode refuses a restricted or sandboxed Grok session, with the
// capability's own reason. Codex and Claude refuse these on their own terms,
// including on a platform without containment.
func unsupportedMode(o Options) error {
	engine := o.Provider.Engine
	if engine != harness.Grok {
		return nil
	}
	if o.Restriction != nil {
		if c := harness.Support(engine, harness.Session, harness.RestrictTools); c.Availability == harness.Unsupported {
			return &UnsupportedError{Engine: engine, Operation: "restriction", Code: RefusedNotOffered, Capability: c}
		}
	}
	if o.Sandbox != nil {
		if c := harness.Support(engine, harness.Session, harness.Sandbox); c.Availability == harness.Unsupported {
			return &UnsupportedError{Engine: engine, Operation: "sandbox", Code: RefusedNotOffered, Capability: c}
		}
	}
	return nil
}

// otherEnginePolicy refuses a policy field only the other engine reads. Left
// in place it would be ignored, and a caller reading an empty ClaudeTools as
// "no tools" on Codex would be running with every native tool.
func otherEnginePolicy(o Options) error {
	p := o.Policy
	grok := p.GrokPermission != "" || p.GrokTelemetry != ""
	switch o.Provider.Engine {
	case harness.Codex:
		if p.ClaudePermission != "" || p.ClaudeTools != nil {
			return refuse(o, "policy", RefusedOtherEnginePolicy, "Codex does not read Policy.ClaudePermission or Policy.ClaudeTools; leave them unset")
		}
		if grok {
			return refuse(o, "policy", RefusedOtherEnginePolicy, "Codex does not read Policy.GrokPermission or Policy.GrokTelemetry; leave them unset")
		}
	case harness.Claude:
		if p.CodexSandbox != "" || p.CodexApproval != "" {
			return refuse(o, "policy", RefusedOtherEnginePolicy, "Claude does not read Policy.CodexSandbox or Policy.CodexApproval; leave them unset")
		}
		if grok {
			return refuse(o, "policy", RefusedOtherEnginePolicy, "Claude does not read Policy.GrokPermission or Policy.GrokTelemetry; leave them unset")
		}
	case harness.Grok:
		if p.CodexSandbox != "" || p.CodexApproval != "" || p.ClaudePermission != "" || p.ClaudeTools != nil {
			return refuse(o, "policy", RefusedOtherEnginePolicy, "Grok does not read the Codex or Claude policy fields; leave them unset")
		}
	}
	return nil
}

// normalizePaths resolves the binary, working directory and harness home,
// defaulting each from this process's environment.
func normalizePaths(o Options) (Options, error) {
	cli := &o.Provider.CLI
	if cli.Binary == "" {
		cli.Binary = string(o.Provider.Engine)
	}
	var err error
	if o.WorkDir == "" {
		if o.WorkDir, err = os.Getwd(); err != nil {
			return o, refuse(o, "work_dir", RefusedWorkDir, "working directory unavailable")
		}
	}
	o.WorkDir, err = filepath.Abs(o.WorkDir)
	if err != nil {
		return o, refuse(o, "work_dir", RefusedWorkDir, "invalid working directory")
	}
	if cli.Home == "" {
		key, suffix := homeVariable(o.Provider.Engine), ".codex"
		switch o.Provider.Engine {
		case harness.Claude:
			suffix = ".claude"
		case harness.Grok:
			suffix = ".grok"
		}
		cli.Home = os.Getenv(key)
		if cli.Home == "" {
			home, e := os.UserHomeDir()
			if e != nil {
				return o, refuse(o, "home", RefusedHome, "home directory unavailable")
			}
			cli.Home = filepath.Join(home, suffix)
		}
	}
	cli.Home, err = filepath.Abs(cli.Home)
	if err != nil {
		return o, refuse(o, "home", RefusedHome, "invalid harness home")
	}
	return o, nil
}

func normalizeLimits(o Options) (Options, error) {
	if o.EventBuffer <= 0 {
		o.EventBuffer = 256
	}
	if o.EventBuffer > 65536 {
		return o, refuse(o, "event_buffer", RefusedLimit, "event buffer exceeds limit")
	}
	if o.MaxTextBytes <= 0 {
		o.MaxTextBytes = 1 << 20
	}
	if o.MaxTextBytes > 16<<20 {
		return o, refuse(o, "max_text_bytes", RefusedLimit, "turn text limit exceeds limit")
	}
	return o, nil
}

// normalizePolicy applies the native policy defaults and refuses a value the
// engine does not recognise. Every Codex and Claude default is applied to
// either of those engines, because every one of them is part of a Ref's digest.
// Grok's references never existed without its own defaults, so it carries only
// those.
func normalizePolicy(o Options) (Options, error) {
	if o.Provider.Engine == harness.Grok {
		return normalizeGrokPolicy(o)
	}
	if o.Policy.CodexSandbox == "" {
		o.Policy.CodexSandbox = "read-only"
	}
	if o.Policy.CodexApproval == "" {
		o.Policy.CodexApproval = "never"
	}
	if o.Policy.ClaudePermission == "" {
		o.Policy.ClaudePermission = "dontAsk"
	}
	if o.Provider.Engine == harness.Codex {
		switch o.Policy.CodexSandbox {
		case "read-only", "workspace-write", "danger-full-access":
		default:
			return o, refuse(o, "policy", RefusedPolicy, "invalid Codex sandbox policy")
		}
		switch o.Policy.CodexApproval {
		case "never", "on-request", "untrusted":
		default:
			return o, refuse(o, "policy", RefusedPolicy, "invalid Codex approval policy")
		}
	} else {
		switch o.Policy.ClaudePermission {
		case "dontAsk", "default", "acceptEdits", "plan", "auto":
		default:
			return o, refuse(o, "policy", RefusedPolicy, "invalid Claude permission policy")
		}
	}
	// Freeze caller-owned slices before fingerprinting or launching.
	if o.Policy.ClaudeTools != nil {
		o.Policy.ClaudeTools = append([]string{}, o.Policy.ClaudeTools...)
	}
	return o, nil
}

func normalizeGrokPolicy(o Options) (Options, error) {
	switch o.Policy.GrokPermission {
	case "":
		return o, refuse(o, "policy", RefusedPolicy, "Grok's agent mode runs edits and commands without asking; set Policy.GrokPermission to GrokDenyWhenAsked or GrokAllowWhenAsked knowingly")
	case GrokDenyWhenAsked, GrokAllowWhenAsked:
	default:
		return o, refuse(o, "policy", RefusedPolicy, "invalid Grok permission policy")
	}
	switch o.Policy.GrokTelemetry {
	case "", GrokTelemetryReduced:
	default:
		return o, refuse(o, "policy", RefusedPolicy, "invalid Grok telemetry policy")
	}
	return o, nil
}

// reference digests the stable contract a resume has to match.
//
// An ordinary session hashes exactly the fields it always hashed, in exactly
// the same shape. References persisted before restricted sessions existed have
// to keep resuming, and adding fields "that are empty anyway" would still have
// changed every one of those digests — an empty field is still a field.
//
// A restricted session hashes the same things plus that it is restricted, its
// tool server's name and its runtime home. It deliberately excludes the tools
// themselves — their names, descriptions and schemas. The restriction is not
// something a reference vouches for: every launch, Start or Resume, re-proves
// it against the installed CLI before the caller's login is used, and Claude's
// startup frame is cross-checked against the tools configured for that launch.
// So a release that edits a tool's description or adds a tool resumes the
// stored conversation under the new surface, rather than orphaning it; a
// transcript that mentions an older tool is harmless.
//
// It also excludes the channel's endpoint. The listener path and the per-launch
// credential change every time the owning process restarts, so including them
// would invalidate every stored reference on each restart while proving
// nothing about what the session can do. The credential never reaches a
// reference in any form.
func reference(o Options, id string) Ref {
	if o.Provider.Engine.Transport() == harness.APITransport {
		return apiReference(o, id)
	}
	// Include nil versus empty tool lists: they have different permission meaning.
	legacy := struct {
		Binary, Model, Effort string
		Instructions          Instructions
		Policy                Policy
		ToolsSpecified        bool
	}{o.Provider.CLI.Binary, o.Model, o.Effort, o.Instructions, o.Policy, o.Policy.ClaudeTools != nil}
	payload, _ := json.Marshal(legacy)
	if o.Restriction != nil {
		// The runtime home is part of a restricted session's identity: the native
		// conversation lives in it, so resuming somewhere else is a different
		// session wearing the same name.
		payload, _ = json.Marshal(struct {
			Legacy      any
			Restricted  bool
			ToolServer  string
			RuntimeHome string
		}{legacy, true, o.Restriction.Tools.Server, o.RuntimeHome})
	}
	if o.Sandbox != nil {
		// A resume must not open a different sandbox than the one the session
		// was started under, so the sandbox is part of what a reference names.
		payload, _ = json.Marshal(struct {
			Legacy      any
			Sandboxed   bool
			Write       bool
			Read        []string `json:",omitempty"`
			Web         bool     `json:",omitempty"`
			ToolServer  string   `json:",omitempty"`
			RuntimeHome string
			Loopback    bool `json:",omitempty"`
		}{legacy, true, o.Sandbox.Write, o.Sandbox.Read, o.Sandbox.Web, sandboxToolServer(o.Sandbox), o.RuntimeHome, o.Sandbox.Loopback})
	}
	if o.Browser {
		// The browser widens what the agent can reach, so a resume must carry
		// it too. Wrapped, so a session without one keeps its digest.
		payload, _ = json.Marshal(struct {
			Base    json.RawMessage
			Browser bool
		}{payload, true})
	}
	if skills := skillsDigest(o); skills != nil {
		// A skill request changes what the agent can do, so a resume must
		// carry the same one. Wrapped rather than merged, so a session without
		// one keeps exactly the digest it always had.
		payload, _ = json.Marshal(struct {
			Base   json.RawMessage
			Skills any
		}{payload, skills})
	}
	hash := sha256.Sum256(payload)
	return Ref{Engine: o.Provider.Engine, ID: id, Home: o.Provider.CLI.Home, WorkDir: o.WorkDir, AccountIdentity: o.AccountIdentity, ConfigHash: hex.EncodeToString(hash[:])}
}

// hostedTools is the tool channel a session serves, restricted or sandboxed,
// or nil for a session that hosts no tools.
func hostedTools(o Options) *ToolHost {
	switch {
	case o.Restriction != nil:
		return &o.Restriction.Tools
	case o.Sandbox != nil:
		return o.Sandbox.Tools
	}
	return nil
}

func sandboxToolServer(s *Sandbox) string {
	if s.Tools == nil {
		return ""
	}
	return s.Tools.Server
}

// freezeTools takes a deep copy, schemas included. A caller keeps its own
// definitions and may reuse or edit them between launches; a session that held
// references into them could have its tool surface changed underneath it after
// the check that approved it, and two concurrent launches could edit each
// other's. The copy is also what reaches the harness, so it is the schema as
// jsonschema.Object prepares it. A schema that cannot be prepared is left as
// the caller's, for validation to refuse.
func freezeTools(tools []ToolDefinition) []ToolDefinition {
	out := make([]ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		if schema, err := jsonschema.Object(tool.Schema); err == nil {
			tool.Schema = schema
		}
		out = append(out, tool)
	}
	return out
}
func compatible(o Options, r Ref) bool {
	if o.Provider.Engine.Transport() == harness.APITransport && !validSessionID(r.ID) {
		// The identifier names a directory; only one this library made is used.
		return false
	}
	expected := reference(o, r.ID)
	return r.ID != "" && r == expected
}
