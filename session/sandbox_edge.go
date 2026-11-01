package session

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/edgeproxy"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
)

// Edge opts into confinement of the entire CLI process tree. macOS alone is
// offered, and every installed binary must pass the pre-launch canary.
type Edge struct {
	// AllowHosts is an exact DNS hostname[:port] list. No wildcards or IP
	// literals. Omitted ports mean 443. Caller grants may resolve to private IPs.
	AllowHosts []string
	// ClaudeToken is a caller-supplied setup-token, used only in the private
	// Claude child environment. Its shell can read it; provider egress is a
	// permitted outward channel. It never enters references or proof keys.
	ClaudeToken string
	// BaseURL is an optional HTTPS provider endpoint. Its authority is admitted
	// by the proxy and used by the CLI. No userinfo, query or fragment.
	BaseURL string
}

const edgeRevision = "seatbelt-edge-v4-provider-v5"

func normalizeEdge(o Options) (Options, error) {
	if o.Sandbox == nil || o.Sandbox.Edge == nil {
		return o, nil
	}
	c := harness.Support(o.Provider.Engine, harness.Session, harness.SandboxEdge)
	if !c.Usable() {
		return o, &UnsupportedError{Engine: o.Provider.Engine, Operation: "sandbox_edge", Code: RefusedEdgeUnavailable, Capability: c}
	}
	conflict := func(reason string) (Options, error) { return o, refuse(o, "sandbox_edge", RefusedEdgeConflict, reason) }
	if o.Restriction != nil || o.Browser {
		return conflict("Sandbox.Edge cannot be combined with Restriction or Browser")
	}
	if o.Sandbox.LoopbackPorts != nil {
		if err := refuseSelectedPorts(o); err != nil {
			return o, err
		}
	}
	if o.Sandbox.LoopbackLocalOnly {
		return conflict(harness.LoopbackLocalOnlySeatbeltReason)
	}
	if o.Provider.Engine == harness.Claude && o.Sandbox.Web {
		return conflict("Claude WebFetch and WebSearch are unavailable under the edge")
	}
	if o.RuntimeHome == "" {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "Sandbox.Edge requires an existing private Options.RuntimeHome outside the workspace")
	}
	if o.Provider.Engine == harness.Claude && o.Sandbox.Edge.ClaudeToken == "" {
		return o, refuse(o, "sandbox_edge", RefusedEdgeTokenRequired, "provide Edge.ClaudeToken from claude setup-token")
	}
	if strings.ContainsAny(o.Sandbox.Edge.ClaudeToken, "\x00\r\n") {
		return conflict("invalid Claude setup-token")
	}
	for _, entry := range o.Env {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasSuffix(upper, "_PROXY") || upper == "HOME" || upper == "TMPDIR" || strings.HasSuffix(upper, "_HOME") || upper == "CLAUDE_CONFIG_DIR" || strings.Contains(upper, "CA_BUNDLE") || strings.Contains(upper, "CERT") || upper == "NODE_TLS_REJECT_UNAUTHORIZED" || upper == "CLAUDE_CODE_OAUTH_TOKEN" || upper == "OPENAI_API_KEY" || upper == "ANTHROPIC_API_KEY" || upper == "ANTHROPIC_AUTH_TOKEN" || upper == "OPENAI_BASE_URL" || upper == "ANTHROPIC_BASE_URL" {
			return conflict("the edge owns proxy, trust and home environment variables")
		}
	}
	edge := *o.Sandbox.Edge
	edge.AllowHosts = nil
	for _, value := range o.Sandbox.Edge.AllowHosts {
		d, err := edgeproxy.ParseDestination(value, true)
		if err != nil {
			return conflict("Edge.AllowHosts must contain exact DNS hostname[:port] destinations")
		}
		if o.Provider.Engine == harness.Codex && d.Host == "auth.openai.com" {
			return conflict("Codex edge cannot admit auth.openai.com: refresh native login outside the edge")
		}
		edge.AllowHosts = append(edge.AllowHosts, net.JoinHostPort(d.Host, strconv.Itoa(int(d.Port))))
	}
	slices.Sort(edge.AllowHosts)
	edge.AllowHosts = slices.Compact(edge.AllowHosts)
	if edge.BaseURL != "" {
		u, err := url.Parse(edge.BaseURL)
		if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return conflict("Edge.BaseURL must be an HTTPS URL without credentials, query or fragment")
		}
		if _, err = edgeproxy.ParseDestination(u.Host, true); err != nil {
			return conflict("Edge.BaseURL must name an exact DNS destination")
		}
		destination, _ := edgeproxy.ParseDestination(u.Host, true)
		if o.Provider.Engine == harness.Codex && destination.Host == "auth.openai.com" {
			return conflict("Codex edge cannot use the token-refresh host as BaseURL")
		}
	}
	s := *o.Sandbox
	s.Edge = &edge
	o.Sandbox = &s
	return o, nil
}

func edgeDestinations(o Options) []edgeproxy.Destination {
	defaults := []string{"api.openai.com", "chatgpt.com"}
	if o.Provider.Engine == harness.Claude {
		defaults = []string{"api.anthropic.com"}
	}
	var out []edgeproxy.Destination
	for _, host := range defaults {
		d, _ := edgeproxy.ParseDestination(host, false)
		out = append(out, d)
	}
	for _, host := range o.Sandbox.Edge.AllowHosts {
		d, _ := edgeproxy.ParseDestination(host, true)
		if !slices.ContainsFunc(out, func(x edgeproxy.Destination) bool { return x.Host == d.Host && x.Port == d.Port }) {
			out = append(out, d)
		}
	}
	if base := o.Sandbox.Edge.BaseURL; base != "" {
		u, _ := url.Parse(base)
		d, _ := edgeproxy.ParseDestination(u.Host, true)
		if !slices.ContainsFunc(out, func(x edgeproxy.Destination) bool { return x.Host == d.Host && x.Port == d.Port }) {
			out = append(out, d)
		}
	}
	return out
}

type edgeLaunch struct {
	proxy    edgeTransport
	plan     sandboxbridge.EdgePlan
	identity string
	secrets  []string
	once     sync.Once
}

type edgeTransport interface {
	Endpoint() string
	Credential() string
	Failed() <-chan struct{}
	Close()
}

func (l *launch) closeEdge() {
	if l != nil && l.edge != nil {
		l.edge.once.Do(l.edge.proxy.Close)
	}
}

func edgeFailure(o Options, code string) error {
	return &CapabilityError{Engine: o.Provider.Engine, Code: code, Phase: BeforeLaunch}
}

func edgeIdentity(o Options) (string, error) {
	binary, _, err := binaryIdentity(o)
	if err != nil {
		return "", edgeFailure(o, CapabilityEdgeConfinementFailed)
	}
	pathEnv := ""
	for _, entry := range environment(o) {
		key, value, _ := strings.Cut(entry, "=")
		if key == "PATH" {
			pathEnv = value
		}
	}
	identity, err := nativecli.EdgeIdentityWithPath(binary, pathEnv)
	if err != nil {
		return "", edgeFailure(o, CapabilityEdgeConfinementFailed)
	}
	return identity, nil
}

func edgeArgs(o Options) []string {
	if o.Provider.Engine == harness.Claude {
		return claudeSandboxArgs(o)
	}
	args := codexSandboxArgs(*o.Sandbox)
	// Remove the permission-profile selection: the enclosing OS boundary owns
	// reads, writes and egress. Legacy thread sandbox is deliberately explicit.
	var out []string
	for i := 0; i < len(args); i += 2 {
		if strings.HasPrefix(args[i+1], "default_permissions=") || strings.HasPrefix(args[i+1], "permissions.") {
			continue
		}
		out = append(out, args[i:i+2]...)
	}
	return out
}

func edgeEnvironment(o Options, p edgeTransport) []string {
	if resolved, err := filepath.EvalSymlinks(o.RuntimeHome); err == nil {
		o.RuntimeHome = resolved
	}
	env := environment(o)
	values := []string{"HOME=" + o.RuntimeHome, "HTTPS_PROXY=" + p.Endpoint(), "HTTP_PROXY=" + p.Endpoint(), "NO_PROXY=", "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "MAX_RETRIES=0"}
	if o.Sandbox.Loopback {
		values = append(values, "NO_PROXY=localhost,127.0.0.1,::1")
	}
	if o.Provider.Engine == harness.Claude {
		values = append(values, "CLAUDE_CONFIG_DIR="+filepath.Join(o.RuntimeHome, "claude"), "CLAUDE_CODE_OAUTH_TOKEN="+o.Sandbox.Edge.ClaudeToken, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	}
	if base := o.Sandbox.Edge.BaseURL; base != "" {
		if o.Provider.Engine == harness.Claude {
			values = append(values, "ANTHROPIC_BASE_URL="+base)
		} else {
			values = append(values, "OPENAI_BASE_URL="+base)
		}
	}
	env = nativecli.Override(env, values...)
	// Override folds key case for portable secret removal. Unix proxy clients
	// need both spellings present, so add aliases after replacing old values.
	noProxy := ""
	if o.Sandbox.Loopback {
		noProxy = "localhost,127.0.0.1,::1"
	}
	return append(env, "https_proxy="+p.Endpoint(), "http_proxy="+p.Endpoint(), "no_proxy="+noProxy)
}

func edgePlan(o Options, p edgeTransport, socket string) (sandboxbridge.EdgePlan, error) {
	u, _ := url.Parse(p.Endpoint())
	port, _ := strconv.Atoi(u.Port())
	bridge := ""
	toolState, secret := "", ""
	if o.Sandbox.Tools != nil {
		bridge = o.Sandbox.Tools.Bridge.Path
		toolState = o.Sandbox.Tools.Dir
		secret = secretPath(toolState)
	}
	plan, err := sandboxbridge.BuildEdge(sandboxbridge.EdgeConfig{Work: o.WorkDir, Runtime: o.RuntimeHome, Binary: o.Provider.CLI.Binary, CLIHome: o.Provider.CLI.Home, Socket: socket, Bridge: bridge, ToolState: toolState, Secret: secret, Read: o.Sandbox.Read, Env: edgeEnvironment(o, p), Write: o.Sandbox.Write, Loopback: o.Sandbox.Loopback, ProxyPort: port})
	if err != nil {
		if errors.Is(err, nativecli.ErrEdgeInterpreter) {
			return plan, refuse(o, "sandbox_edge", RefusedEdgeConflict, "npm CLI needs a resolvable Node interpreter on the launch PATH")
		}
		if errors.Is(err, nativecli.ErrEdgeNative) {
			return plan, refuse(o, "sandbox_edge", RefusedEdgeConflict, "npm Codex needs its platform native executable installed")
		}
		if err.Error() == "edge_install_credentials" {
			return plan, refuse(o, "sandbox_edge", RefusedEdgeConflict, "CLI installation overlaps the selected login home or contains a native credential file; use a separate credential-free installation")
		}
		return plan, edgeFailure(o, CapabilityEdgeConfinementFailed)
	}
	return plan, nil
}

func proveEdge(ctx context.Context, o Options, lease *os.File) (*launch, error) {
	p, err := edgeproxy.New(edgeDestinations(o))
	if err != nil {
		_ = lease.Close()
		return nil, refuse(o, "sandbox_edge", RefusedEdgeProxyUnavailable, "the per-session provider proxy could not be opened")
	}
	l := &launch{extra: edgeArgs(o), edge: &edgeLaunch{proxy: p, secrets: []string{p.Credential(), p.Endpoint(), base64.StdEncoding.EncodeToString([]byte("edge:" + p.Credential())), o.Sandbox.Edge.ClaudeToken}}}
	fail := func(err error) (*launch, error) {
		l.closeEdge()
		if l.host != nil {
			l.host.close()
		} else {
			_ = lease.Close()
		}
		return nil, err
	}
	if l.edge.plan, err = edgePlan(o, p, ""); err != nil {
		return fail(err)
	}
	if l.edge.identity, err = edgeIdentity(o); err != nil {
		return fail(err)
	}
	if err = verifyEdge(ctx, o, l); err != nil {
		return fail(err)
	}
	return l, nil
}

func prepareEdge(ctx context.Context, o Options, lease *os.File) (*launch, error) {
	l, err := proveEdge(ctx, o, lease)
	if err != nil {
		return nil, err
	}
	p := l.edge.proxy
	fail := func(err error) (*launch, error) {
		l.closeEdge()
		if l.host != nil {
			l.host.close()
		} else {
			_ = lease.Close()
		}
		return nil, err
	}
	// Only a settled complete proof precedes writable native state or login.
	if err = os.MkdirAll(filepath.Join(o.RuntimeHome, "tmp"), 0700); err != nil {
		return fail(edgeFailure(o, CapabilityEdgeConfinementFailed))
	}
	if o.Provider.Engine == harness.Codex {
		if err = prepareCodexSandbox(ctx, o, l); err != nil {
			return fail(err)
		}
	} else {
		if err = os.MkdirAll(filepath.Join(o.RuntimeHome, "claude"), 0700); err != nil {
			return fail(edgeFailure(o, CapabilityEdgeConfinementFailed))
		}
	}
	if o.Sandbox.Tools != nil {
		l.host, err = newToolHost(*o.Sandbox.Tools, lease)
		if err != nil {
			return fail(err)
		}
		l.host.edge = true
		l.edge.secrets = append(l.edge.secrets, string(l.host.secret))
		hosted, e := engines[o.Provider.Engine].sandbox.hostTools(l.host)
		if e != nil {
			return fail(e)
		}
		l.extra = append(l.extra, hosted...)
		provedKey := l.edge.plan.Key
		l.edge.plan, err = edgePlan(o, p, l.host.socket)
		if err != nil {
			return fail(err)
		}
		if l.edge.plan.Key != provedKey {
			return fail(edgeFailure(o, CapabilityEdgeConfinementFailed))
		}
	} else {
		_ = lease.Close()
	}
	return l, nil
}
