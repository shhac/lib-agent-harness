package session

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/internal/restrict"
)

// A restricted session removes the harness's own tools and replaces them with
// the caller's, then proves that removal against the installed CLI before the
// credentialed process starts.
//
// The reason the removal has to be real, rather than a permission mode or a
// sandbox setting, is disclosure. A harness running on the operator's machine
// with a built-in file or shell tool can read credentials, keychain-backed
// material and private data and send them upstream as ordinary model input.
// Forbidding writes does not close that path, and neither does choosing a
// working directory: a working directory scopes relative paths, not what a tool
// may open. So the tools go, and the library checks that they went.
//
// This is opt-in. A session opened without Restriction keeps the ordinary
// native contract, and nothing about it changes.
type Restriction struct {
	// Tools is the session's entire tool surface.
	Tools ToolHost
	// Probe bounds the pre-launch capability check. Default 60s. The check ends
	// as soon as the harness has sent a request and a short settling window has
	// passed, so this bound is a ceiling for a harness that never sends one.
	Probe time.Duration
}

// There is deliberately no option to skip verification. A caller's assertion
// that a build is safe is not evidence, and an exported switch for it would
// become the thing every awkward deployment reaches for. Repeat launches avoid
// the cost through a process-local cache keyed by the exact binary and the
// exact arguments, which is evidence about the same thing the probe proved.

// VerifyRestriction runs the capability check for a restricted configuration
// without opening a session. It is what an application should call when setting
// a worker up, so an operator learns that an installed CLI cannot be restricted
// at the point they are configuring it rather than when work is commissioned.
//
// It starts no credentialed process: the check uses a disposable home, a dummy
// credential and a provider that refuses every request. A nil return means this
// exact binary and configuration were proved, and a later Start skips repeating
// the same check in this process.
func VerifyRestriction(ctx context.Context, o Options) error {
	if o.Restriction == nil {
		return errors.New("verification applies to a restricted session; set Options.Restriction")
	}
	normalized, err := normalize(o)
	if err != nil {
		return err
	}
	l, err := prepareLaunch(ctx, normalized, nil)
	if err != nil {
		return err
	}
	l.host.close()
	return nil
}

// normalizeRestriction refuses a restricted configuration that could not be
// honoured as asked, and freezes the restriction the session will run with.
func normalizeRestriction(o Options) (Options, error) {
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
	if err := o.Restriction.Tools.validate(); err != nil {
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
	runtimeHome, err := filepath.Abs(o.RuntimeHome)
	if err != nil {
		return o, errors.New("invalid restricted session runtime home")
	}
	o.RuntimeHome = runtimeHome
	// Copy the whole restriction rather than writing through the caller's
	// pointer: normalizing must not edit the value a caller still holds, and
	// two launches sharing one Restriction must not see each other's defaults.
	frozen := *o.Restriction
	if frozen.Probe <= 0 {
		frozen.Probe = 60 * time.Second
	}
	frozen.Tools.Tools = freezeTools(frozen.Tools.Tools)
	o.Restriction = &frozen
	return o, nil
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

// claudeRestrictedArgs disables every inherited customization surface and
// leaves the caller's tools as the only ones available.
//
// Two flags, because they answer two different questions. `--tools=` with an
// empty list removes the built-in tool surface, which is the part that would
// otherwise read the host. `--allowedTools` grants permission to exactly the
// hosted identifiers, which is how an MCP tool becomes usable without a prompt.
// Checked against the installed CLI: with both, its initialization reports
// `tools: ["mcp__<server>__<tool>"]` and no built-ins.
func claudeRestrictedArgs(h *toolHost) []string {
	return []string{
		// --restricted independently removes the built-in tools that run commands
		// or code, ignores user, project and local settings files, confines file
		// tools to the working directory and refuses bypassPermissions. Checked
		// against the installed CLI: it leaves an explicit MCP server loaded, so
		// it costs nothing here. --safe-mode would go further and was measured to
		// disable the explicit MCP configuration too, and --bare would drop the
		// subscription login for an API key; neither is usable for a worker.
		"--restricted",
		"--setting-sources=", `--settings={"disableAllHooks":true}`,
		"--strict-mcp-config", claudeMCPConfig(h),
		"--disable-slash-commands", "--no-chrome",
		"--tools=",
		claudeHostedAllowed(h),
	}
}

// claudeMCPConfig registers the tool channel's bridge as the session's one
// MCP server. Restricted and sandboxed sessions both load it this way, beside
// --strict-mcp-config.
func claudeMCPConfig(h *toolHost) string {
	config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		h.cfg.Server: map[string]any{
			"type": "stdio", "command": h.cfg.Bridge.Path,
			"args": bridgeArgs(h), "env": h.environment(),
		},
	}})
	return "--mcp-config=" + string(config)
}

// claudeHostedAllowed grants permission to exactly the hosted identifiers,
// which is how an MCP tool becomes usable without a prompt.
func claudeHostedAllowed(h *toolHost) string {
	return "--allowedTools=" + strings.Join(h.cfg.Qualified(), ",")
}

func bridgeArgs(h *toolHost) []string {
	if h.cfg.Bridge.Args == nil {
		return []string{}
	}
	return h.cfg.Bridge.Args
}

// codexRestrictedArgs pairs the shared catalog restriction with the settings
// that suppress project documents and the surfaces a worker must not have.
//
// There are deliberately no `--ignore-user-config` or `--ignore-rules` flags
// here. The installed CLI does not accept them on `app-server` at any argument
// position — they belong to `exec` — so passing them is not a stricter launch,
// it is a launch that fails to start. Inherited configuration is handled where
// it can actually be handled: the session runs in a private RuntimeHome whose
// configuration this library writes (see runtime.go).
//
// Every override is TOML, because that is what Codex parses. The MCP server is
// registered as dotted-key leaves rather than one nested value, so quoting
// stays local to each string.
func codexRestrictedArgs(h *toolHost, catalogPath string) ([]string, error) {
	catalog, err := restrict.TOMLString(catalogPath)
	if err != nil {
		return nil, err
	}
	settings := append([]string{"model_catalog_json=" + catalog}, restrict.CodexSettings()...)
	server, err := codexHostedServer(h)
	if err != nil {
		return nil, err
	}
	return codexOverrides(append(settings, server...)), nil
}

// codexHostedServer registers the tool channel's bridge as an MCP server.
// Restricted and sandboxed sessions both load it this way.
func codexHostedServer(h *toolHost) ([]string, error) {
	server, err := restrict.CodexMCPServer(h.cfg.Server, h.cfg.Bridge.Path, bridgeArgs(h), h.environment())
	if err != nil {
		return nil, err
	}
	// Without this the installed CLI refuses every hosted call with "MCP tool
	// call requires approval, but approval policy is never" — the tools are
	// advertised and unusable. It approves this one server, which the daemon owns
	// and whose tools it implements; no host approval policy is widened, and
	// approval_policy stays "never" for everything else.
	return append(server, "mcp_servers."+h.cfg.Server+`.default_tools_approval_mode="approve"`), nil
}

// codexOverrides passes each TOML setting as its own -c override.
func codexOverrides(settings []string) []string {
	args := make([]string, 0, len(settings)*2)
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	return args
}

// restrictedCatalogFor narrows an installed catalog to the selected model with
// its native execution surfaces removed. The harness's own coding instructions
// are kept: removing the tools is the restriction, and a session's scoped task
// instructions are added through the ordinary instruction parameters rather
// than by replacing the base prompt.
func restrictedCatalogFor(catalog []byte, model, effort string) ([]byte, error) {
	if len(catalog) == 0 {
		return nil, &CapabilityError{Engine: string(Codex), Code: CapabilityCatalogUnavailable, Phase: BeforeLaunch}
	}
	out, err := restrict.CodexCatalog(catalog, model, effort, nil)
	if err != nil {
		return nil, &CapabilityError{Engine: string(Codex), Code: CapabilityCatalogRestriction, Phase: BeforeLaunch, Tools: reasonOf(err)}
	}
	return out, nil
}

// reasonOf surfaces the shared package's fixed reason code so an operator can
// tell an unknown model from an unsupported effort without provider text.
func reasonOf(err error) []string {
	var reason *restrict.Error
	if errors.As(err, &reason) {
		return []string{reason.Code}
	}
	return nil
}
