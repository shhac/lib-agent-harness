package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func normalize(o Options) (Options, error) {
	if o.Engine != Codex && o.Engine != Claude {
		return o, &UnsupportedError{"engine", Capability{Unsupported, "unrecognized harness"}}
	}
	if o.Binary == "" {
		o.Binary = string(o.Engine)
	}
	if o.WorkDir == "" {
		var err error
		o.WorkDir, err = os.Getwd()
		if err != nil {
			return o, errors.New("working directory unavailable")
		}
	}
	var err error
	o.WorkDir, err = filepath.Abs(o.WorkDir)
	if err != nil {
		return o, errors.New("invalid working directory")
	}
	if o.Home == "" {
		key := "CODEX_HOME"
		suffix := ".codex"
		if o.Engine == Claude {
			key = "CLAUDE_CONFIG_DIR"
			suffix = ".claude"
		}
		o.Home = os.Getenv(key)
		if o.Home == "" {
			home, e := os.UserHomeDir()
			if e != nil {
				return o, errors.New("home directory unavailable")
			}
			o.Home = filepath.Join(home, suffix)
		}
	}
	o.Home, err = filepath.Abs(o.Home)
	if err != nil {
		return o, errors.New("invalid harness home")
	}
	if o.EventBuffer <= 0 {
		o.EventBuffer = 256
	}
	if o.EventBuffer > 65536 {
		return o, errors.New("event buffer exceeds limit")
	}
	if o.MaxTextBytes <= 0 {
		o.MaxTextBytes = 1 << 20
	}
	if o.MaxTextBytes > 16<<20 {
		return o, errors.New("turn text limit exceeds limit")
	}
	if o.Instructions.Mode != "" && o.Instructions.Mode != Replace && o.Instructions.Mode != Append {
		return o, &UnsupportedError{"instructions", Capability{Unsupported, "instruction mode must be replace or append"}}
	}
	if o.Instructions.Text != "" && o.Instructions.Mode == "" {
		return o, errors.New("instructions require an explicit replace or append mode")
	}
	if err = validateEnv(o.Env); err != nil {
		return o, err
	}
	o.Env = append([]string(nil), o.Env...)
	if o.Sandbox != nil {
		frozen := *o.Sandbox
		frozen.Read = append([]string(nil), o.Sandbox.Read...)
		o.Sandbox = &frozen
		if o, err = normalizeSandbox(o); err != nil {
			return o, err
		}
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
	if o.Engine == Codex {
		switch o.Policy.CodexSandbox {
		case "read-only", "workspace-write", "danger-full-access":
		default:
			return o, errors.New("invalid Codex sandbox policy")
		}
		switch o.Policy.CodexApproval {
		case "never", "on-request", "untrusted":
		default:
			return o, errors.New("invalid Codex approval policy")
		}
	} else {
		switch o.Policy.ClaudePermission {
		case "dontAsk", "default", "acceptEdits", "plan", "auto":
		default:
			return o, errors.New("invalid Claude permission policy")
		}
	}
	// Freeze caller-owned slices before fingerprinting or launching.
	if o.Policy.ClaudeTools != nil {
		o.Policy.ClaudeTools = append([]string{}, o.Policy.ClaudeTools...)
	}
	if o.Restriction != nil {
		if !restrictedPlatform() {
			return o, &CapabilityError{Engine: string(o.Engine), Code: CapabilityUnsupportedPlatform, Phase: BeforeLaunch}
		}
		// Two tool policies would silently disagree about what this session may
		// do. The restriction owns the surface, so the other one has to be absent.
		if o.Policy.ClaudeTools != nil {
			return o, errors.New("a restricted session owns its tool surface; leave Policy.ClaudeTools unset")
		}
		if o.Instructions.Mode == Replace {
			return o, errors.New("a restricted session keeps the harness's coding instructions; append scoped instructions instead of replacing them")
		}
		if o.Engine == Codex && o.Model == "" {
			return o, errors.New("a restricted Codex session requires an explicit model to restrict in the installed catalog")
		}
		if err = o.Restriction.Tools.validate(); err != nil {
			return o, err
		}
		if o.Engine == Claude && reservedClaudeServer(o.Restriction.Tools.Server) {
			// Checked against the installed CLI: this name is accepted and then
			// silently not loaded, leaving a session with no tools at all. Refusing
			// it here says so, rather than letting the launch check discover a
			// missing surface and report it as a build problem.
			return o, &CapabilityError{Engine: string(o.Engine), Code: CapabilityServerNameReserved, Phase: BeforeLaunch, Tools: []string{o.Restriction.Tools.Server}}
		}
		if o.RuntimeHome == "" {
			return o, errors.New("a restricted session requires a durable private runtime home; set Options.RuntimeHome")
		}
		if o.RuntimeHome, err = filepath.Abs(o.RuntimeHome); err != nil {
			return o, errors.New("invalid restricted session runtime home")
		}
		// Copy the whole restriction rather than writing through the caller's
		// pointer: normalizing must not edit the value a caller still holds, and
		// two launches sharing one Restriction must not see each other's defaults.
		frozen := *o.Restriction
		if frozen.Probe <= 0 {
			frozen.Probe = 60 * time.Second
		}
		frozen.Tools.Tools = freezeTools(frozen.Tools.Tools)
		o.Restriction = &frozen
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
// A restricted session hashes the same things plus its tool surface: the server
// name and every tool's name and schema. It deliberately excludes the channel's
// endpoint. The listener path and the per-launch credential change every time
// the owning process restarts, so including them would invalidate every stored
// reference on each restart while proving nothing about what the session can
// do. The credential never reaches a reference in any form.
func reference(o Options, id string) Ref {
	// Include nil versus empty tool lists: they have different permission meaning.
	legacy := struct {
		Binary, Model, Effort string
		Instructions          Instructions
		Policy                Policy
		ToolsSpecified        bool
	}{o.Binary, o.Model, o.Effort, o.Instructions, o.Policy, o.Policy.ClaudeTools != nil}
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
			HostedTools []ToolDefinition
		}{legacy, true, o.Restriction.Tools.Server, o.RuntimeHome, hostedTools(o)})
	}
	if o.Sandbox != nil {
		// A resume must not open a different sandbox than the one the session
		// was started under, so the sandbox is part of what a reference names.
		payload, _ = json.Marshal(struct {
			Legacy      any
			Sandboxed   bool
			Write       bool
			Read        []string `json:",omitempty"`
			RuntimeHome string
		}{legacy, true, o.Sandbox.Write, o.Sandbox.Read, o.RuntimeHome})
	}
	hash := sha256.Sum256(payload)
	return Ref{Engine: o.Engine, ID: id, Home: o.Home, WorkDir: o.WorkDir, AccountIdentity: o.AccountIdentity, ConfigHash: hex.EncodeToString(hash[:])}
}

func hostedTools(o Options) []ToolDefinition {
	if o.Restriction == nil {
		return nil
	}
	tools := freezeTools(o.Restriction.Tools.Tools)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

// freezeTools takes a deep copy, schemas included. A caller keeps its own
// definitions and may reuse or edit them between launches; a session that held
// references into them could have its tool surface changed underneath it after
// the check that approved it, and two concurrent launches could edit each
// other's.
func freezeTools(tools []ToolDefinition) []ToolDefinition {
	out := make([]ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		tool.Schema = freezeValue(tool.Schema).(map[string]any)
		out = append(out, tool)
	}
	return out
}

func freezeValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, nested := range typed {
			out[key] = freezeValue(nested)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, nested := range typed {
			out = append(out, freezeValue(nested))
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}
func compatible(o Options, r Ref) bool {
	expected := reference(o, r.ID)
	return r.ID != "" && r == expected
}

// launch describes one restricted session's prepared runtime: the arguments it
// adds and the private files it depends on.
type launch struct {
	host    *toolHost
	extra   []string
	catalog string
}

func commandArgs(o Options, nativeID string, resuming bool, l *launch) []string {
	if o.Engine == Codex {
		args := []string{"app-server", "--listen", "stdio://"}
		if l != nil {
			args = append(args, l.extra...)
		}
		return args
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-mode", o.Policy.ClaudePermission}
	if l != nil {
		args = append(args, l.extra...)
	}
	if resuming {
		args = append(args, "--resume", nativeID)
	} else {
		args = append(args, "--session-id", nativeID)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.Policy.ClaudeTools != nil {
		args = append(args, "--tools="+strings.Join(o.Policy.ClaudeTools, ","))
	}
	if o.Instructions.Mode != "" {
		flag := "--system-prompt"
		if o.Instructions.Mode == Append {
			flag = "--append-system-prompt"
		}
		args = append(args, flag, o.Instructions.Text)
	}
	return args
}

// environment is the session's own environment plus the caller's additions,
// which come last so they take effect.
func environment(o Options) []string {
	env := baseEnvironment(o)
	if o.Sandbox != nil && o.Engine == Claude {
		// Auto-memory would read and write the operator's own memory folders.
		env = append(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1")
	}
	return append(env, o.Env...)
}

// sandboxInherited is all a sandboxed session inherits from this process:
// enough to find the CLI, its login and a shell, and nothing the process
// happened to have exported.
var sandboxInherited = map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TERM": true, "LANG": true, "TZ": true, "TMPDIR": true, "__CF_USER_TEXT_ENCODING": true}

func sandboxInherits(entry string) bool {
	key, _, _ := strings.Cut(entry, "=")
	return sandboxInherited[key] || strings.HasPrefix(key, "LC_")
}

func baseEnvironment(o Options) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if o.Sandbox != nil && !sandboxInherits(entry) {
			continue
		}
		key, _, _ := strings.Cut(entry, "=")
		// Retain USER and other OS identity variables: native keychain lookup uses
		// them. Strip provider credentials/overrides to preserve subscription login.
		if key == "CODEX_HOME" || key == "CLAUDE_CONFIG_DIR" || key == "CLAUDECODE" || key == "OPENAI_API_KEY" || key == "OPENAI_BASE_URL" || key == "ANTHROPIC_API_KEY" || key == "ANTHROPIC_AUTH_TOKEN" || key == "ANTHROPIC_BASE_URL" || key == "CLAUDE_CODE_OAUTH_TOKEN" || strings.HasPrefix(key, "CLAUDE_CODE_USE_") || strings.HasPrefix(key, "ANTHROPIC_DEFAULT_") || key == "ANTHROPIC_MODEL" {
			continue
		}
		env = append(env, entry)
	}
	// A restricted session runs in its own home: the library wrote that home's
	// configuration and shared the login into it, so nothing the operator keeps
	// beside their credential comes along.
	selected := o.Home
	if (o.Restriction != nil || o.Sandbox != nil) && o.RuntimeHome != "" && o.Engine == Codex {
		selected = o.RuntimeHome
	}
	key := "CODEX_HOME"
	if o.Engine == Claude {
		key = "CLAUDE_CONFIG_DIR"
		home, err := os.UserHomeDir()
		if err == nil && filepath.Clean(selected) == filepath.Join(home, ".claude") {
			return env
		}
	}
	return append(env, key+"="+selected)
}

// reservedClaudeServer names tool-server names the installed harness keeps for
// itself. A reserved name is not rejected at launch; it is accepted and then
// not loaded, which is why it is worth naming here.
func reservedClaudeServer(name string) bool {
	switch name {
	case "workspace", "claude", "anthropic":
		return true
	}
	return false
}

// validateEnv refuses additions the harness manages itself: credentials and
// provider overrides it strips, the homes it selects, and the process basics
// an addition could use to change which binaries or login are used.
func validateEnv(env []string) error {
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !envKey.MatchString(key) {
			return errors.New("environment additions must be KEY=VALUE")
		}
		upper := strings.ToUpper(key)
		switch {
		case key == "HOME", key == "PATH", key == "USER", key == "LOGNAME", key == "SHELL", key == "CLAUDECODE",
			strings.HasPrefix(upper, "CODEX_"), strings.HasPrefix(upper, "CLAUDE_"), strings.HasPrefix(upper, "ANTHROPIC_"), strings.HasPrefix(upper, "OPENAI_"),
			// These change what the CLI itself loads or where it connects, and
			// it runs outside the sandbox.
			strings.HasPrefix(upper, "DYLD_"), strings.HasPrefix(upper, "LD_"), upper == "NODE_OPTIONS", upper == "NODE_PATH", strings.HasPrefix(upper, "BUN_"),
			strings.HasSuffix(upper, "_PROXY"), strings.HasPrefix(upper, "SSL_CERT_"), upper == "NODE_EXTRA_CA_CERTS", strings.HasPrefix(upper, "GIT_"):
			return errors.New("environment addition " + key + " is managed by the harness or would change the CLI outside its sandbox")
		}
	}
	return nil
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
