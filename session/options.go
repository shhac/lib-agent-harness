package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
		if o.Restriction.Probe <= 0 {
			o.Restriction.Probe = 60 * time.Second
		}
		o.Restriction.Tools.Tools = append([]ToolDefinition(nil), o.Restriction.Tools.Tools...)
	}
	return o, nil
}

// reference digests the stable contract a resume has to match. The tool channel
// contributes its surface — the server name and every tool's name and schema —
// and deliberately not its endpoint: the listener path and the per-launch
// channel credential change every time the owning process restarts, and
// including them would invalidate every stored reference on each restart while
// proving nothing about what the session can do. The credential never reaches a
// reference in any form.
func reference(o Options, id string) Ref {
	// Include nil versus empty tool lists: they have different permission meaning.
	payload, _ := json.Marshal(struct {
		Binary, Model, Effort string
		Instructions          Instructions
		Policy                Policy
		ToolsSpecified        bool
		Restricted            bool
		ToolServer            string
		HostedTools           []ToolDefinition
	}{o.Binary, o.Model, o.Effort, o.Instructions, o.Policy, o.Policy.ClaudeTools != nil, o.Restriction != nil, toolServerName(o), hostedTools(o)})
	hash := sha256.Sum256(payload)
	return Ref{Engine: o.Engine, ID: id, Home: o.Home, WorkDir: o.WorkDir, AccountIdentity: o.AccountIdentity, ConfigHash: hex.EncodeToString(hash[:])}
}

func toolServerName(o Options) string {
	if o.Restriction == nil {
		return ""
	}
	return o.Restriction.Tools.Server
}

func hostedTools(o Options) []ToolDefinition {
	if o.Restriction == nil {
		return nil
	}
	tools := append([]ToolDefinition(nil), o.Restriction.Tools.Tools...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
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
func environment(o Options) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		// Retain USER and other OS identity variables: native keychain lookup uses
		// them. Strip provider credentials/overrides to preserve subscription login.
		if key == "CODEX_HOME" || key == "CLAUDE_CONFIG_DIR" || key == "CLAUDECODE" || key == "OPENAI_API_KEY" || key == "OPENAI_BASE_URL" || key == "ANTHROPIC_API_KEY" || key == "ANTHROPIC_AUTH_TOKEN" || key == "ANTHROPIC_BASE_URL" || key == "CLAUDE_CODE_OAUTH_TOKEN" || strings.HasPrefix(key, "CLAUDE_CODE_USE_") || strings.HasPrefix(key, "ANTHROPIC_DEFAULT_") || key == "ANTHROPIC_MODEL" {
			continue
		}
		env = append(env, entry)
	}
	key := "CODEX_HOME"
	if o.Engine == Claude {
		key = "CLAUDE_CONFIG_DIR"
		home, err := os.UserHomeDir()
		if err == nil && filepath.Clean(o.Home) == filepath.Join(home, ".claude") {
			return env
		}
	}
	return append(env, key+"="+o.Home)
}
