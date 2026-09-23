package session

import (
	"context"
	"encoding/json"
	"errors"
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
	l, err := prepareLaunch(ctx, normalized)
	if err != nil {
		return err
	}
	l.host.close()
	return nil
}

// Capability failure codes. They are fixed library constants: no provider text,
// path or credential ever enters one.
const (
	CapabilityUnsupportedPlatform = "restricted_session_unsupported_platform"
	CapabilityNativeToolsPresent  = "native_tools_present"
	CapabilityHostedToolsMissing  = "hosted_tools_missing"
	CapabilityInstructionsMerged  = "inherited_instructions_merged"
	CapabilityChangedModel        = "changed_model"
	CapabilityChangedEffort       = "changed_effort"
	CapabilityProbeNoRequest      = "probe_made_no_request"
	CapabilityProbeUnreadable     = "probe_request_unreadable"
	CapabilityProbeTimeout        = "probe_timed_out"
	CapabilityProbeFailed         = "probe_could_not_run"
	CapabilityCatalogUnavailable  = "model_catalog_unavailable"
	CapabilityCatalogRestriction  = "model_catalog_restriction_failed"
	CapabilityServerNotLoaded     = "tool_server_not_loaded"
	CapabilityServerNameReserved  = "tool_server_name_reserved"
	CapabilityLoginUnavailable    = "harness_login_unavailable"
)

// Capability check phases. The distinction matters to an operator: one of these
// happened before the harness existed, the other after it started but still
// before it was given anything to do.
const (
	// BeforeLaunch: the check ran against a disposable home and a provider that
	// refuses inference. No credentialed process was started.
	BeforeLaunch = "before_launch"
	// BeforeFirstPrompt: the harness had started and advertised a surface that
	// disagreed with the session's. No prompt was sent; the session was closed.
	BeforeFirstPrompt = "before_first_prompt"
)

// CapabilityError reports that a restricted session could not be established.
// Tool names, when present, are the ones the check disagreed about, and they
// come from the caller's own configuration or from a fixed native-name
// comparison — never from free text.
type CapabilityError struct {
	Engine string
	Code   string
	Phase  string
	Tools  []string
}

func (e *CapabilityError) Error() string {
	message := map[string]string{
		CapabilityUnsupportedPlatform: "restricted worker sessions are not available on this platform",
		CapabilityNativeToolsPresent:  "the installed harness kept tools this session did not configure",
		CapabilityHostedToolsMissing:  "the installed harness did not offer the tools this session configured",
		CapabilityInstructionsMerged:  "the installed harness merged inherited instructions into its request",
		CapabilityChangedModel:        "the installed harness requested a different model",
		CapabilityChangedEffort:       "the installed harness requested a different reasoning effort",
		CapabilityProbeNoRequest:      "the installed harness made no request during the capability check",
		CapabilityProbeUnreadable:     "the capability check could not read the harness's request",
		CapabilityProbeTimeout:        "the capability check did not finish in time",
		CapabilityProbeFailed:         "the capability check could not be run",
		CapabilityCatalogUnavailable:  "the installed harness did not supply a model catalog to restrict",
		CapabilityCatalogRestriction:  "the selected model could not be restricted in the installed harness catalog",
		CapabilityServerNotLoaded:     "the installed harness did not load this session's tool server",
		CapabilityServerNameReserved:  "the installed harness reserves this tool server name; choose another",
		CapabilityLoginUnavailable:    "the selected harness home has no file-backed login to share with a restricted session; log in to that home first",
		CapabilitySandboxUnavailable:  "the installed harness could not be run under the requested sandbox",
		CapabilitySandboxNotEnforced:  "the installed harness's sandbox allowed writes or network access the session must not have",
	}[e.Code]
	if message == "" {
		message = "the restricted session configuration could not be established"
	}
	out := e.Engine + ": " + message
	if len(e.Tools) > 0 {
		out += " (" + strings.Join(e.Tools, ", ") + ")"
	}
	// Say what actually happened rather than one reassuring phrase for both: a
	// session that started and was closed is a different fact to report than one
	// that was never launched.
	switch e.Phase {
	case BeforeFirstPrompt:
		return out + "; the session was closed before any prompt was sent"
	default:
		return out + "; no session was started"
	}
}
func (e *CapabilityError) Unwrap() error { return ErrUnsupported }

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
	config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		h.cfg.Server: map[string]any{
			"type": "stdio", "command": h.cfg.Bridge.Path,
			"args": bridgeArgs(h), "env": h.environment(),
		},
	}})
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
		"--strict-mcp-config", "--mcp-config=" + string(config),
		"--disable-slash-commands", "--no-chrome",
		"--tools=",
		"--allowedTools=" + strings.Join(h.cfg.Qualified(), ","),
	}
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
	server, err := restrict.CodexMCPServer(h.cfg.Server, h.cfg.Bridge.Path, bridgeArgs(h), h.environment())
	if err != nil {
		return nil, err
	}
	settings = append(settings, server...)
	// Without this the installed CLI refuses every hosted call with "MCP tool
	// call requires approval, but approval policy is never" — the tools are
	// advertised and unusable. It approves this one server, which the daemon owns
	// and whose tools it implements; no host approval policy is widened, and
	// approval_policy stays "never" for everything else.
	settings = append(settings, "mcp_servers."+h.cfg.Server+`.default_tools_approval_mode="approve"`)
	args := make([]string, 0, len(settings)*2)
	for _, setting := range settings {
		args = append(args, "-c", setting)
	}
	return args, nil
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
