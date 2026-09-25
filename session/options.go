package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// normalize resolves a caller's options into the ones a session runs with,
// refusing any it cannot honour. Its stages run in order.
func normalize(o Options) (Options, error) {
	if o.Engine != Codex && o.Engine != Claude {
		return o, &UnsupportedError{"engine", Capability{Unsupported, "unrecognized harness"}}
	}
	o, err := normalizePaths(o)
	if err != nil {
		return o, err
	}
	if o, err = normalizeLimits(o); err != nil {
		return o, err
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
	// The sandbox goes before the policy defaults: it refuses a policy the
	// caller set, and once defaults are applied it could not tell which that was.
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
	return o, nil
}

// normalizePaths resolves the binary, working directory and harness home,
// defaulting each from this process's environment.
func normalizePaths(o Options) (Options, error) {
	if o.Binary == "" {
		o.Binary = string(o.Engine)
	}
	var err error
	if o.WorkDir == "" {
		if o.WorkDir, err = os.Getwd(); err != nil {
			return o, errors.New("working directory unavailable")
		}
	}
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
	return o, nil
}

func normalizeLimits(o Options) (Options, error) {
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
	return o, nil
}

// normalizePolicy applies the native policy defaults and refuses a value the
// engine does not recognise.
func normalizePolicy(o Options) (Options, error) {
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
			RuntimeHome string
		}{legacy, true, o.Sandbox.Write, o.Sandbox.Read, o.Sandbox.Web, o.RuntimeHome})
	}
	hash := sha256.Sum256(payload)
	return Ref{Engine: o.Engine, ID: id, Home: o.Home, WorkDir: o.WorkDir, AccountIdentity: o.AccountIdentity, ConfigHash: hex.EncodeToString(hash[:])}
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
