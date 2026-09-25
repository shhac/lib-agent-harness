package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Sandbox opts an ordinary native session — its own tools intact — into the
// installed CLI's OS sandbox. Nothing the session executes reaches the network,
// and it writes inside WorkDir only when Write is set. Claude Code's shell may
// also write its own private session temporary directory; Codex's may not write
// /tmp or $TMPDIR at all.
//
// Reads are not fully contained. A Codex session can read what the operating
// account can read. A Claude session's shell cannot read the home directory
// outside WorkDir and Read, but can read the rest of the system. Either way the
// model provider connection remains an outward channel for anything read.
// Claude Code's file tools are confined by permission rules rather than by the
// OS sandbox, which covers only its shell.
//
// The sandbox is proved before any credentialed launch and, for Codex, read
// back from the running harness before its first prompt. There is no mode that
// starts a sandboxed session whose sandbox could not be established.
type Sandbox struct {
	Write bool
	// Read names directories outside the workspace that the session's shell
	// may also read, such as a shared build-module cache. Claude Code blocks its
	// shell from reading the home directory otherwise; Codex already reads
	// everything the account can. Each must be an absolute directory that holds
	// no credentials. None may contain the home directory, which would reopen
	// all of it.
	Read []string
}

// sandboxProfile is the Codex permission profile a sandboxed session runs
// under. The installed app-server applies a profile only when thread/start
// carries no legacy sandbox mode; passing one silently replaces the profile
// with a policy that also writes temporary directories.
const sandboxProfile = "harness_sandbox"

const sandboxProbeTimeout = 60 * time.Second

func sandboxClaudeTools(write bool) []string {
	if write {
		return []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep"}
	}
	return []string{"Bash", "Read", "Glob", "Grep"}
}

// normalizeSandbox rejects every setting a sandbox would otherwise have to
// silently override. o.Policy is the caller's, before defaults are applied.
// sandboxReadDirs resolves each extra readable directory to the path the
// sandbox will match, and refuses any that would reopen the home directory.
func sandboxReadDirs(dirs []string) ([]string, error) {
	home, _ := os.UserHomeDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return nil, fmt.Errorf("sandbox read path %q must be a clean absolute path", dir)
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		if rel, err := filepath.Rel(dir, home); home != "" && err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("sandbox read path %q would reopen the home directory", dir)
		}
		out = append(out, dir)
	}
	return out, nil
}

func normalizeSandbox(o Options) (Options, error) {
	if o.Restriction != nil {
		return o, errors.New("a session is either restricted or sandboxed; set only one of Restriction and Sandbox")
	}
	// Sandbox rules name the working directory by path. A path reached through
	// a symlink would not match the one the sandbox resolves, so name the real
	// directory.
	if resolved, err := filepath.EvalSymlinks(o.WorkDir); err == nil {
		o.WorkDir = resolved
	}
	if !restrictedPlatform() {
		return o, &CapabilityError{Engine: string(o.Engine), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	read, err := sandboxReadDirs(o.Sandbox.Read)
	if err != nil {
		return o, err
	}
	o.Sandbox.Read = read
	if o.Engine == Codex {
		if o.Policy.CodexSandbox != "" {
			return o, errors.New("a sandboxed Codex session owns its sandbox; leave Policy.CodexSandbox unset")
		}
		if o.Policy.CodexApproval != "" && o.Policy.CodexApproval != "never" {
			return o, errors.New("a sandboxed Codex session never asks for approval; leave Policy.CodexApproval unset or never")
		}
		o.Policy.CodexApproval = "never"
		if o.RuntimeHome == "" {
			return o, errors.New("a sandboxed Codex session requires a durable private runtime home; set Options.RuntimeHome")
		}
		abs, err := filepath.Abs(o.RuntimeHome)
		if err != nil {
			return o, errors.New("invalid sandboxed session runtime home")
		}
		o.RuntimeHome = abs
		return o, nil
	}
	if o.Policy.ClaudePermission != "" && o.Policy.ClaudePermission != "dontAsk" {
		return o, errors.New("a sandboxed Claude session runs in dontAsk permission mode; leave Policy.ClaudePermission unset or dontAsk")
	}
	o.Policy.ClaudePermission = "dontAsk"
	if o.Policy.ClaudeTools == nil {
		o.Policy.ClaudeTools = sandboxClaudeTools(o.Sandbox.Write)
		return o, nil
	}
	allowed := sandboxClaudeTools(true)
	for _, tool := range o.Policy.ClaudeTools {
		if !slices.Contains(allowed, tool) || (!o.Sandbox.Write && (tool == "Edit" || tool == "Write")) {
			return o, &UnsupportedError{"tools", Capability{Unsupported, "a sandboxed session cannot enable " + tool}}
		}
	}
	return o, nil
}

var sandboxDisabledFeatures = []string{"apps", "plugins", "remote_plugin", "hooks", "browser_use", "browser_use_external", "computer_use", "multi_agent", "multi_agent_v2", "skill_mcp_dependency_install", "workspace_dependencies", "tool_suggest", "memories"}

// codexSandboxArgs are the overrides a sandboxed Codex harness and its canary
// both run with. They are the only place the profile is described.
func codexSandboxArgs(write bool) []string {
	workspace := "read"
	if write {
		workspace = "write"
	}
	settings := []string{
		`default_permissions="` + sandboxProfile + `"`,
		// Configuration a later launch would read is never writable: .git for
		// hooks, .codex and .agents for servers and skills that run outside it.
		`permissions.` + sandboxProfile + `.filesystem={":root"="read",":workspace_roots"={"."="` + workspace + `",".git"="read",".codex"="read",".agents"="read"}}`,
		`permissions.` + sandboxProfile + `.network.enabled=false`,
		`approval_policy="never"`,
		`web_search="disabled"`,
		`agents.enabled=false`,
		`check_for_update_on_startup=false`,
		`analytics.enabled=false`,
	}
	// Surfaces that reach past the sandbox — connectors, plugins, hooks, a
	// browser or the desktop — are switched off. The shell and file tools stay,
	// and so does the code-mode host: on codex 0.154.0 every native tool call
	// runs through it, and disabling it leaves a session unable to do anything.
	for _, feature := range sandboxDisabledFeatures {
		settings = append(settings, "features."+feature+"=false")
	}
	args := make([]string, 0, len(settings)*2)
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	return args
}

// claudeSandboxSettings is the settings document a sandboxed Claude session
// and its status check both load, with every other settings source dropped.
func claudeSandboxSettings(o Options) string {
	permissions := map[string]any{
		"defaultMode":                         "dontAsk",
		"deny":                                []string{"WebFetch", "WebSearch"},
		"blockReadsOutsideWorkingDirectories": true,
	}
	filesystem := map[string]any{}
	if o.Sandbox.Write {
		// Permission rules take "//" for an absolute path. Repository metadata
		// stays read-only, as in the Codex profile: hooks or config written
		// there would run wherever git next runs, outside this sandbox.
		gitDir := "/" + filepath.Join(o.WorkDir, ".git") + "/**"
		permissions["allow"] = []string{"Edit(/" + o.WorkDir + "/**)"}
		permissions["deny"] = []string{"WebFetch", "WebSearch", "Edit(" + gitDir + ")", "Write(" + gitDir + ")"}
		filesystem["denyWrite"] = []string{filepath.Join(o.WorkDir, ".git")}
	} else {
		permissions["deny"] = []string{"WebFetch", "WebSearch", "Edit", "Write"}
		// Sandbox paths are plain absolute paths. The shell may otherwise write
		// to its working directory by default.
		filesystem["denyWrite"] = []string{o.WorkDir}
	}
	if len(o.Sandbox.Read) > 0 {
		filesystem["allowRead"] = o.Sandbox.Read
		allow, _ := permissions["allow"].([]string)
		for _, dir := range o.Sandbox.Read {
			allow = append(allow, "Read(/"+dir+"/**)")
		}
		permissions["allow"] = allow
	}
	sandbox := map[string]any{
		"enabled":                  true,
		"failIfUnavailable":        true,
		"allowUnsandboxedCommands": false,
		"autoAllowBashIfSandboxed": true,
		"network":                  map[string]any{"allowedDomains": []string{}, "strictAllowlist": true},
	}
	if len(filesystem) > 0 {
		sandbox["filesystem"] = filesystem
	}
	raw, _ := json.Marshal(map[string]any{"sandbox": sandbox, "disableAllHooks": true, "permissions": permissions, "claudeMdExcludes": claudeInstructionExcludes(o)})
	return string(raw)
}

// claudeInstructionExcludes names the operator's own instruction files: their
// user-level CLAUDE.md and rules, and any in a directory above the workspace.
// With every settings source dropped, Claude Code 2.1.280 loads no instruction
// files at all, including the workspace's own, so this is a second guard for
// builds where that changes. A caller that wants a repository's instructions
// followed has to point the session at them.
func claudeInstructionExcludes(o Options) []string {
	homes := []string{o.Home}
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, filepath.Join(home, ".claude"))
	}
	var out []string
	for _, home := range homes {
		if home != "" {
			out = append(out, filepath.Join(home, "CLAUDE.md"), filepath.Join(home, "rules")+"/**")
		}
	}
	for dir := filepath.Dir(o.WorkDir); ; dir = filepath.Dir(dir) {
		out = append(out, filepath.Join(dir, "CLAUDE.md"), filepath.Join(dir, "CLAUDE.local.md"), filepath.Join(dir, ".claude", "CLAUDE.md"), filepath.Join(dir, ".claude", "rules")+"/**")
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return out
}

func claudeSandboxArgs(o Options) []string {
	return []string{"--setting-sources=", "--strict-mcp-config", "--disable-slash-commands", "--disallowedTools", "WebFetch,WebSearch,mcp__*", "--settings", claudeSandboxSettings(o)}
}

// prepareSandbox assembles a sandboxed launch and proves its sandbox against
// the installed harness before a credentialed process exists.
func prepareSandbox(ctx context.Context, o Options) (*launch, error) {
	l := &launch{}
	if o.Engine == Codex {
		if _, err := prepareRuntimeHome(o.Home, o.RuntimeHome); err != nil {
			return nil, err
		}
		l.extra = codexSandboxArgs(o.Sandbox.Write)
	} else {
		l.extra = claudeSandboxArgs(o)
	}
	if err := verifySandbox(ctx, o, l); err != nil {
		return nil, err
	}
	return l, nil
}

// VerifySandbox proves, without inference, that the installed harness can run
// a session with this sandbox. Start and Resume run the same check; this lets
// a caller report readiness before any work is asked for.
func VerifySandbox(ctx context.Context, o Options) error {
	if o.Sandbox == nil {
		return errors.New("options carry no sandbox to verify")
	}
	o, err := normalize(o)
	if err != nil {
		return err
	}
	l := &launch{extra: codexSandboxArgs(o.Sandbox.Write)}
	if o.Engine == Claude {
		l.extra = claudeSandboxArgs(o)
	} else if login, err := credentialDigest(filepath.Join(o.Home, codexCredentialFile)); err != nil || login == nil {
		// Start would refuse to share a missing login; say so here too rather
		// than report a session ready that cannot open.
		return &CapabilityError{Engine: string(Codex), Code: CapabilityLoginUnavailable, Phase: BeforeLaunch}
	}
	return verifySandbox(ctx, o, l)
}

func verifySandbox(ctx context.Context, o Options, l *launch) error {
	key, err := sandboxKey(o, l)
	if err != nil {
		return err
	}
	if verified.holds(key) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	if o.Engine == Codex {
		err = probeCodexSandbox(ctx, o)
	} else {
		err = probeClaudeSandbox(ctx, o)
	}
	if err != nil {
		return err
	}
	verified.record(key)
	return nil
}

// sandboxKey identifies exactly what a sandbox check established: this binary
// as it is on disk now, with these sandbox arguments.
func sandboxKey(o Options, l *launch) (string, error) {
	binary, info, err := binaryIdentity(o)
	if err != nil {
		return "", &CapabilityError{Engine: string(o.Engine), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	payload, _ := json.Marshal(struct {
		Kind     string
		Engine   Engine
		Binary   string
		Size     int64
		Modified time.Time
		Write    bool
		Read     []string
		Args     []string
		TempDir  string
	}{"sandbox", o.Engine, binary, info.Size(), info.ModTime(), o.Sandbox.Write, o.Sandbox.Read, l.extra, sessionTempDir()})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// probeClaudeSandbox asks the installed CLI for the sandbox status it would
// apply with exactly this session's settings and no other settings source.
//
// "available" and "installed" are not required: on macOS they describe an
// optional Windows installer and read false while the Seatbelt sandbox is in
// force. What must hold is that the sandbox is supported, enabled and strict,
// with no reason it is unavailable.
func probeClaudeSandbox(ctx context.Context, o Options) error {
	// A throwaway home: with every settings source but this one dropped, the
	// operator's own configuration cannot change the answer, and the login is
	// never needed to ask the question.
	home, err := os.MkdirTemp("", "agent-harness-sandbox-status-")
	if err != nil {
		return &CapabilityError{Engine: string(Claude), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	defer os.RemoveAll(home)
	args := []string{"--setting-sources=", "--settings", claudeSandboxSettings(o), "sandbox", "status"}
	out, err := runOnce(ctx, o.Binary, args, o.WorkDir, disposableEnvironment(o, home))
	if err != nil {
		return &CapabilityError{Engine: string(Claude), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	var status struct {
		Supported         bool    `json:"supported"`
		Enabled           bool    `json:"enabled"`
		StrictMode        bool    `json:"strictMode"`
		UnavailableReason *string `json:"unavailableReason"`
	}
	if json.Unmarshal(out, &status) != nil {
		return &CapabilityError{Engine: string(Claude), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	if !status.Supported || status.UnavailableReason != nil {
		return &CapabilityError{Engine: string(Claude), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	if !status.Enabled || !status.StrictMode {
		return &CapabilityError{Engine: string(Claude), Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch}
	}
	return nil
}

// Canary outcomes, one per line, written by the probe script.
const (
	canaryInside   = "inside"
	canarySibling  = "sibling"
	canaryTmp      = "tmp"
	canaryTmpdir   = "tmpdir"
	canaryNetwork  = "network"
	canaryNoClient = "no-network-client"
	canaryGitDir   = "gitdir"
	// canaryRan is the script's last line. Without it an empty result could
	// mean "everything refused" or "nothing ran", and only one is evidence.
	canaryRan = "canary-ran"
)

// canaryScript attempts, from inside the sandbox, everything the session must
// be refused, and reports each attempt that succeeded. It never reaches past
// the machine: the network attempt targets a listener this probe owns.
const canaryScript = `
try() { name=$1; shift; if ( "$@" ) >/dev/null 2>&1; then echo "$name"; fi; }
try inside sh -c 'echo x > ./canary'
try sibling sh -c 'echo x > "$1"' _ "$CANARY_SIBLING"
try tmp sh -c 'echo x > "/tmp/$CANARY_NAME" && rm -f "/tmp/$CANARY_NAME"'
try tmpdir sh -c 'echo x > "$TMPDIR/$CANARY_NAME" && rm -f "$TMPDIR/$CANARY_NAME"'
try gitdir sh -c 'echo x >> .git/config'
if command -v curl >/dev/null 2>&1; then curl -s -m 3 --noproxy '*' -o /dev/null "http://127.0.0.1:$CANARY_PORT/"
elif command -v nc >/dev/null 2>&1; then nc -z -w 3 127.0.0.1 "$CANARY_PORT"
else echo no-network-client; fi
echo canary-ran
exit 0
`

// probeCodexSandbox runs the canary under the same permission profile the
// session uses, in a disposable home, and checks every refusal. The temporary
// directory is the one a real session inherits, not the probe's own.
func probeCodexSandbox(ctx context.Context, o Options) error {
	unavailable := &CapabilityError{Engine: string(Codex), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	notEnforced := &CapabilityError{Engine: string(Codex), Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch}
	root, err := os.MkdirTemp("", "agent-harness-sandbox-")
	if err != nil {
		return unavailable
	}
	defer os.RemoveAll(root)
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	sibling := filepath.Join(root, "outside")
	for _, dir := range []string{home, filepath.Join(workspace, ".git"), sibling} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return unavailable
		}
	}
	// Repository metadata must stay read-only even for a writing session.
	if err = os.WriteFile(filepath.Join(workspace, ".git", "config"), []byte("[core]\n"), 0600); err != nil {
		return unavailable
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return unavailable
	}
	defer listener.Close()
	reached := make(chan struct{}, 1)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			conn.Close()
			select {
			case reached <- struct{}{}:
			default:
			}
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	env := disposableEnvironment(o, home)
	env = slices.DeleteFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "TMPDIR=") })
	env = append(env, "TMPDIR="+sessionTempDir(), "CANARY_SIBLING="+filepath.Join(sibling, "canary"), "CANARY_NAME="+filepath.Base(root)+".canary", "CANARY_PORT="+strconv.Itoa(port))
	args := append([]string{"sandbox", "-P", sandboxProfile, "-C", workspace}, codexSandboxArgs(o.Sandbox.Write)...)
	args = append(args, "--", "/bin/sh", "-c", canaryScript)
	out, err := runOnce(ctx, o.Binary, args, workspace, env)
	if err != nil {
		if ctx.Err() != nil {
			return &CapabilityError{Engine: string(Codex), Code: CapabilityProbeTimeout, Phase: BeforeLaunch}
		}
		return unavailable
	}
	escaped := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		escaped[strings.TrimSpace(line)] = true
	}
	if !escaped[canaryRan] || escaped[canaryNoClient] {
		return unavailable
	}
	// The listener may accept a moment after the canary's client has exited.
	select {
	case <-reached:
		escaped[canaryNetwork] = true
	case <-time.After(500 * time.Millisecond):
	}
	if escaped[canaryInside] != o.Sandbox.Write {
		if o.Sandbox.Write {
			return unavailable // the workspace it must write to is not writable
		}
		return notEnforced
	}
	if escaped[canarySibling] || escaped[canaryTmp] || escaped[canaryTmpdir] || escaped[canaryNetwork] || escaped[canaryGitDir] {
		return notEnforced
	}
	if _, err = os.Stat(filepath.Join(sibling, "canary")); err == nil {
		return notEnforced
	}
	return nil
}

// sessionTempDir is the temporary directory a harness launched from this
// process would inherit.
func sessionTempDir() string {
	if dir := os.Getenv("TMPDIR"); dir != "" {
		return dir
	}
	return os.TempDir()
}

// checkCodexSandbox reads back the policy the running harness applied to its
// thread. A thread that is not under this session's profile, with the network
// closed and temporary directories excluded, is closed before any prompt.
func checkCodexSandbox(o Options, body []byte) error {
	var response struct {
		Sandbox struct {
			Type                string   `json:"type"`
			NetworkAccess       bool     `json:"networkAccess"`
			WritableRoots       []string `json:"writableRoots"`
			ExcludeSlashTmp     bool     `json:"excludeSlashTmp"`
			ExcludeTmpdirEnvVar bool     `json:"excludeTmpdirEnvVar"`
		} `json:"sandbox"`
		Profile *struct {
			ID string `json:"id"`
		} `json:"activePermissionProfile"`
	}
	notEnforced := &CapabilityError{Engine: string(Codex), Code: CapabilitySandboxNotEnforced, Phase: BeforeFirstPrompt}
	if json.Unmarshal(body, &response) != nil {
		return notEnforced
	}
	sandbox := response.Sandbox
	if response.Profile == nil || response.Profile.ID != sandboxProfile || sandbox.NetworkAccess || len(sandbox.WritableRoots) > 0 {
		return notEnforced
	}
	if !o.Sandbox.Write {
		if sandbox.Type != "readOnly" {
			return notEnforced
		}
		return nil
	}
	if sandbox.Type != "workspaceWrite" || !sandbox.ExcludeSlashTmp || !sandbox.ExcludeTmpdirEnvVar {
		return notEnforced
	}
	return nil
}
