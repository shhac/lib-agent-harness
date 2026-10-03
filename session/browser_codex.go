package session

// The sandboxed browser is the owner's bridge, with an explicit, minimal
// configuration. Never copy its arbitrary environment, arguments or settings.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/restrict"
)

type codexBrowserBridge struct {
	Command string
	Env     map[string]string
	home    string // Source of the declaration; excluded from its JSON identity.
}

func browserCapability(code string) error {
	return &CapabilityError{Engine: harness.Codex, Code: code, Phase: BeforeLaunch}
}

func bridgeUnavailable(reason string) error {
	return &CapabilityError{Engine: harness.Codex, Code: CapabilityBrowserBridgeUnavailable, Phase: BeforeLaunch, Reason: reason}
}

func browserBridgeHome(o Options) string {
	if o.BrowserBridgeHome != "" {
		return o.BrowserBridgeHome
	}
	return o.Provider.CLI.Home
}

// containmentPath resolves existing ancestors even when the final directory
// has not been created. Otherwise /var/work/home and /private/var/work can
// appear unrelated on macOS solely because home does not exist yet.
func containmentPath(path string) string {
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(containmentPath(parent), filepath.Base(path))
}

func pathContains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func pathsOverlap(a, b string) bool {
	a, b = containmentPath(a), containmentPath(b)
	return pathContains(a, b) || pathContains(b, a)
}

func browserWorkspace(work string) (string, error) {
	resolved, err := filepath.EvalSymlinks(work)
	if err == nil {
		var info os.FileInfo
		info, err = os.Stat(resolved)
		if err == nil && info.IsDir() {
			return resolved, nil
		}
	}
	return "", refuse(Options{Provider: harness.Provider{Engine: harness.Codex}}, "work_dir", RefusedWorkDir, "browser bridge checks require an available working directory")
}

// CheckBrowserBridge checks a sandboxed Codex browser declaration and installation
// without launching a session, server, bridge, Node or proof. It writes no runtime
// home, checks no login, and never consults the proof cache. A nil result proves
// neither confinement (use VerifySandbox) nor Chrome connectivity.
// Missing Browser is RefusedNotConfigured; unsupported checks use operation
// browser_bridge, while invalid explicit home options use browser_bridge_home.
// An unavailable workspace is a work_dir / RefusedWorkDir preflight refusal.
// Cancellation during mcp get returns ctx.Err().
// The CLI may perform incidental writes during mcp get.
func CheckBrowserBridge(ctx context.Context, o Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !o.Browser {
		return refuse(o, "browser", RefusedNotConfigured, "Browser must be set to check its bridge")
	}
	var err error
	o, err = normalize(o)
	if err != nil {
		return err
	}
	if o.Provider.Engine != harness.Codex || o.Sandbox == nil {
		return refuse(o, "browser_bridge", RefusedConflict, "bridge checks require a sandboxed Codex browser session")
	}
	_, err = readCodexBrowserBridge(ctx, o)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func readCodexBrowserBridge(ctx context.Context, o Options) (*codexBrowserBridge, error) {
	home := browserBridgeHome(o)
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() {
		return nil, bridgeUnavailable(BridgeHomeUnavailable)
	}
	// Stat alone can succeed for a directory the CLI cannot read. Opening it
	// checks access without enumerating or reading any of its other entries.
	dir, err := os.Open(home)
	if err != nil {
		return nil, bridgeUnavailable(BridgeHomeUnavailable)
	}
	if err := dir.Close(); err != nil {
		return nil, bridgeUnavailable(BridgeHomeUnavailable)
	}
	if _, err := browserWorkspace(o.WorkDir); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// mcp get retrieves configuration, starting neither a server nor a turn.
	// The library adds no writes; the CLI may perform incidental writes.
	env := disposableEnvironment(o, home)
	raw, err := runOnce(ctx, o.Provider.CLI.Binary, []string{"mcp", "get", "node_repl", "--json"}, o.WorkDir, env)
	if err != nil {
		return nil, bridgeUnavailable(BridgeNotDeclared)
	}
	bridge, err := parseCodexBrowserBridge(raw)
	if err != nil {
		return nil, err
	}
	if err = bridge.outsideWorkspace(o.WorkDir); err != nil {
		return nil, err
	}
	if _, err = bridge.identity(); err != nil {
		return nil, err
	}
	bridge.home = home
	return bridge, nil
}

func parseCodexBrowserBridge(raw []byte) (*codexBrowserBridge, error) {
	var cfg struct {
		Name      string `json:"name"`
		Enabled   bool   `json:"enabled"`
		Transport struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"transport"`
	}
	fail := func() (*codexBrowserBridge, error) { return nil, bridgeUnavailable(BridgeDeclarationUnsupported) }
	if json.Unmarshal(raw, &cfg) != nil || cfg.Name != "node_repl" || !cfg.Enabled || cfg.Transport.Type != "stdio" || !filepath.IsAbs(cfg.Transport.Command) || len(cfg.Transport.Args) != 0 {
		return fail()
	}
	source := cfg.Transport.Env
	var services map[string]string
	if json.Unmarshal([]byte(source["NODE_REPL_TRUSTED_SERVICES"]), &services) != nil || !filepath.IsAbs(services["browser"]) {
		return fail()
	}
	// Keep installation identity and module resolution, not caller-controlled
	// process flags, credentials, service registrations or owner-home settings.
	env := map[string]string{}
	for _, key := range []string{"NODE_REPL_NODE_PATH", "NODE_REPL_NODE_MODULE_DIRS", "CODEX_CLI_PATH", "BROWSER_USE_CODEX_APP_VERSION", "BROWSER_USE_CODEX_APP_BUILD_FLAVOR"} {
		if value := source[key]; value != "" {
			env[key] = value
		}
	}
	if !filepath.IsAbs(env["NODE_REPL_NODE_PATH"]) || (env["CODEX_CLI_PATH"] != "" && !filepath.IsAbs(env["CODEX_CLI_PATH"])) {
		return fail()
	}
	modules := filepath.SplitList(env["NODE_REPL_NODE_MODULE_DIRS"])
	if len(modules) == 0 {
		return fail()
	}
	for _, dir := range modules {
		if !filepath.IsAbs(dir) {
			return fail()
		}
	}
	trusted := append([]string{filepath.Dir(services["browser"])}, modules...)
	serviceJSON, _ := json.Marshal(map[string]string{"browser": services["browser"]})
	env["NODE_REPL_TRUSTED_SERVICES"] = string(serviceJSON)
	env["NODE_REPL_TRUSTED_CODE_PATHS"] = strings.Join(trusted, string(os.PathListSeparator))
	env["BROWSER_USE_AVAILABLE_BACKENDS"] = "chrome"
	env["NODE_REPL_INSTRUCTIONS_USE_CASE_CHROME"] = ""
	env["NODE_REPL_INSTRUCTIONS_USE_CASE_BROWSER"] = ""
	env["NODE_REPL_INSTRUCTIONS_USE_CASE_COMPUTER_USE"] = ""
	env["NODE_REPL_NATIVE_PIPE_CONNECT_TIMEOUT_MS"] = "1000"
	return &codexBrowserBridge{Command: cfg.Transport.Command, Env: env}, nil
}

// config is TOML, including escaped string keys/values. CODEX_HOME is the
// session's private home even in the service process; the owner's settings
// cannot be loaded by a trusted browser service either.
func (b *codexBrowserBridge) config(home string) ([]byte, error) {
	command, err := restrict.TOMLString(b.Command)
	if err != nil {
		return nil, browserCapability(CapabilityBrowserBridgeUnavailable)
	}
	selected, err := restrict.TOMLString(home)
	if err != nil {
		return nil, browserCapability(CapabilityBrowserBridgeUnavailable)
	}
	var out strings.Builder
	out.WriteString(runtimeConfig)
	out.WriteString("\n[mcp_servers.node_repl]\ncommand = " + command + "\nargs = []\nenabled_tools = [\"js\", \"js_reset\"]\nstartup_timeout_sec = 120\n[mcp_servers.node_repl.env]\n")
	keys := make([]string, 0, len(b.Env))
	for key := range b.Env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		encodedKey, _ := restrict.TOMLString(key)
		value, err := restrict.TOMLString(b.Env[key])
		if err != nil {
			return nil, browserCapability(CapabilityBrowserBridgeUnavailable)
		}
		out.WriteString(encodedKey + " = " + value + "\n")
	}
	out.WriteString("CODEX_HOME = " + selected + "\n")
	return []byte(out.String()), nil
}

// identity binds evidence to the actual bridge, Node and trusted browser code,
// as well as its configuration. An app/plugin upgrade requires a fresh proof.
func (b *codexBrowserBridge) identity() (string, error) {
	var services map[string]string
	_ = json.Unmarshal([]byte(b.Env["NODE_REPL_TRUSTED_SERVICES"]), &services)
	paths := []string{b.Command, b.Env["NODE_REPL_NODE_PATH"], services["browser"]}
	if cli := b.Env["CODEX_CLI_PATH"]; cli != "" {
		paths = append(paths, cli)
	}
	paths = append(paths, filepath.SplitList(b.Env["NODE_REPL_NODE_MODULE_DIRS"])...)
	var identities []any
	for _, path := range paths {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", bridgeUnavailable(BridgeInstallationMissing)
		}
		info, err := os.Stat(real)
		if err != nil {
			return "", bridgeUnavailable(BridgeInstallationMissing)
		}
		identities = append(identities, []any{real, info.Size(), info.ModTime()})
	}
	raw, _ := json.Marshal([]any{b, identities})
	return string(raw), nil
}

// Trusted code and executable installation paths cannot overlap a workspace
// the model may write. Otherwise it could replace code that runs unsandboxed.
func (b *codexBrowserBridge) outsideWorkspace(work string) error {
	var services map[string]string
	_ = json.Unmarshal([]byte(b.Env["NODE_REPL_TRUSTED_SERVICES"]), &services)
	paths := []string{b.Command, b.Env["NODE_REPL_NODE_PATH"], filepath.Dir(services["browser"])}
	paths = append(paths, filepath.SplitList(b.Env["NODE_REPL_NODE_MODULE_DIRS"])...)
	if cli := b.Env["CODEX_CLI_PATH"]; cli != "" {
		paths = append(paths, cli)
	}
	resolvedWork, err := browserWorkspace(work)
	if err != nil {
		return err
	}
	work = resolvedWork
	for _, path := range paths {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return bridgeUnavailable(BridgeInstallationMissing)
		}
		if pathContains(work, resolved) || pathContains(resolved, work) {
			return bridgeUnavailable(BridgeInWorkspace)
		}
	}
	return nil
}
