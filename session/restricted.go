package session

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
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
	// Probe bounds the pre-launch capability check. Default 60s.
	Probe time.Duration
	// SkipProbe runs a restricted session without proving the restriction first.
	// It exists for callers that have already proved this exact configuration in
	// this process, and for tests. It is not a way to run an unverified harness:
	// the post-startup check still applies, and a caller that sets this is
	// asserting the verification happened elsewhere.
	SkipProbe bool
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
)

// CapabilityError reports that a restricted session could not be established.
// Nothing was launched with the caller's login. Tools names, when present, are
// the ones the check disagreed about, and they come from the caller's own
// configuration or from a fixed native-name comparison — never from free text.
type CapabilityError struct {
	Engine string
	Code   string
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
	}[e.Code]
	if message == "" {
		message = "the restricted session configuration could not be established"
	}
	out := e.Engine + ": " + message
	if len(e.Tools) > 0 {
		out += " (" + strings.Join(e.Tools, ", ") + ")"
	}
	return out + "; no session was started"
}
func (e *CapabilityError) Unwrap() error { return ErrUnsupported }

// compareTools is the whole capability judgement: exactly the configured tools,
// nothing else. Extra tools are a disclosure path; missing tools mean the
// session cannot do its work and would improvise with whatever remained.
func compareTools(engine string, expected, actual []string) *CapabilityError {
	want := map[string]bool{}
	for _, name := range expected {
		want[name] = true
	}
	got := map[string]bool{}
	for _, name := range actual {
		got[name] = true
	}
	var extra, missing []string
	for name := range got {
		if !want[name] {
			extra = append(extra, name)
		}
	}
	for name := range want {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		return &CapabilityError{Engine: engine, Code: CapabilityNativeToolsPresent, Tools: extra}
	}
	if len(missing) > 0 {
		return &CapabilityError{Engine: engine, Code: CapabilityHostedToolsMissing, Tools: missing}
	}
	return nil
}

// mcpServers is the configuration both engines receive: one server, the
// caller's bridge, and the paths naming this session's private channel.
func mcpServers(h *toolHost) map[string]any {
	env := map[string]any{}
	for key, value := range h.environment() {
		env[key] = value
	}
	args := h.cfg.Bridge.Args
	if args == nil {
		args = []string{}
	}
	return map[string]any{h.cfg.Server: map[string]any{
		"type": "stdio", "command": h.cfg.Bridge.Path, "args": args, "env": env,
	}}
}

// claudeRestrictedArgs disables every inherited customization surface and
// allows exactly the hosted tools.
func claudeRestrictedArgs(h *toolHost) []string {
	config, _ := json.Marshal(map[string]any{"mcpServers": mcpServers(h)})
	return []string{
		"--setting-sources=", `--settings={"disableAllHooks":true}`,
		"--strict-mcp-config", "--mcp-config=" + string(config),
		"--disable-slash-commands", "--no-chrome",
		"--tools=" + strings.Join(h.cfg.Qualified(), ","),
	}
}

// codexRestrictedArgs pairs the shared catalog restriction with the settings
// that suppress inherited configuration, rules and project documents.
func codexRestrictedArgs(h *toolHost, catalogPath string) []string {
	args := []string{"--ignore-user-config", "--ignore-rules"}
	settings := append([]string{"model_catalog_json=" + strconv.Quote(catalogPath)}, restrict.CodexSettings()...)
	server, _ := json.Marshal(mcpServers(h)[h.cfg.Server])
	settings = append(settings, "mcp_servers."+h.cfg.Server+"="+string(server))
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
		return nil, &CapabilityError{Engine: string(Codex), Code: CapabilityCatalogUnavailable}
	}
	out, err := restrict.CodexCatalog(catalog, model, effort, nil)
	if err != nil {
		return nil, &CapabilityError{Engine: string(Codex), Code: CapabilityCatalogRestriction, Tools: reasonOf(err)}
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
