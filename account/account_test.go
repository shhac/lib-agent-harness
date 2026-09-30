package account

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/session"
)

// Verified wire data, with identifiers replaced.
const (
	codexAccountFixture     = `{"account":{"type":"chatgpt","email":"fixture@example.test","planType":"prolite"},"requiresOpenaiAuth":true}`
	codexRateLimitsFixture  = `{"ordinaryUsageAllowed":true,"rateLimits":{"limitId":"codex","limitName":null,"normalModelSlug":null,"primary":{"usedPercent":2,"windowDurationMins":10080,"resetsAt":1791113081},"secondary":null,"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},"individualLimit":null,"spendControlReached":false,"planType":"prolite","rateLimitReachedType":null},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":2,"windowDurationMins":10080,"resetsAt":1791113081},"secondary":null,"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},"spendControlReached":false,"rateLimitReachedType":null}},"rateLimitResetCredits":{"availableCount":2,"credits":[{"id":"fixture-credit","resetType":"codexRateLimits","status":"available","grantedAt":1788582130,"expiresAt":1791174130,"title":"Full reset","description":"must-not-escape"}]},"accountId":"fixture-account","rateLimitUpsell":null}`
	claudeInitFixture       = `{"account":{"email":"fixture@example.test","subscriptionType":"max","apiProvider":"firstParty"}}`
	claudeUsageFixture      = `{"session":{"total_cost_usd":0},"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":11,"resets_at":"2026-09-27T19:59:59.718097+00:00","limit_dollars":null,"used_dollars":null,"remaining_dollars":null,"locked_reason":null},"seven_day":{"utilization":3,"resets_at":"2026-10-04T14:59:59.718121+00:00","limit_dollars":null,"used_dollars":null,"remaining_dollars":null,"locked_reason":null},"seven_day_opus":null,"iguana_necktie":{"utilization":0,"resets_at":"2026-11-05T07:59:00+00:00","limit_dollars":250,"used_dollars":0,"remaining_dollars":250,"locked_reason":null},"extra_usage":{"is_enabled":false,"monthly_limit":3750,"used_credits":251,"utilization":6.69,"currency":"GBP","decimal_places":2,"disabled_reason":"out_of_credits","user_disabled":false,"spend_limit_reached":false,"credits_ever_enabled":true,"daily":null,"weekly":null}},"limits":[{"kind":"session","group":"session","percent":11,"severity":"normal","resets_at":"2026-09-27T19:59:59.718097+00:00","scope":null,"is_active":true},{"kind":"weekly_all","group":"weekly","percent":3,"severity":"normal","resets_at":"2026-10-04T14:59:59.718121+00:00","scope":null,"is_active":false},{"kind":"weekly_scoped","group":"weekly","percent":0,"severity":"normal","resets_at":"2026-10-04T15:00:00+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false}],"spend":{"used":{"amount_minor":251,"currency":"GBP","exponent":2},"limit":{"amount_minor":3750,"currency":"GBP","exponent":2},"percent":7,"severity":"normal","enabled":false,"disabled_reason":"out_of_credits","cap":{"money":{"amount_minor":3750,"currency":"GBP","exponent":2},"credits":null},"balance":null,"auto_reload":null,"can_purchase_credits":false,"can_toggle":false}}`
	grokSubscriptionFixture = `{"authenticated":true,"meta":{"email":"fixture@example.test","auth_mode":"Oidc","team_id":"fixture-team","team_name":null,"is_zdr":false,"team_role":null,"coding_data_retention_opt_out":true,"show_resolved_model":false,"gate":null,"subscription_tier":"Free","feedback_trace_offer":false,"backend_billed":false}}`
)

// The test binary doubles as each CLI, speaking only the inspection protocol.
// Any other method, including Grok's session/new, ends it with a failure.
//
// Grok launches with an allowlisted environment, so the test's variables
// never reach it: its fixture is recognized by its argv, which no go test
// command line contains, and reads its mode from the home it was given.
func init() {
	if os.Getenv("LIB_HARNESS_ACCOUNT_FIXTURE") != "1" && !slices.Equal(os.Args[1:], []string{"agent", "--no-leader", "stdio"}) {
		return
	}
	os.Exit(fixture())
}

func fixture() int {
	args := os.Args[1:]
	switch {
	case slices.Equal(args, []string{"agent", "--no-leader", "stdio"}):
		return grokFixture()
	case slices.Contains(args, "app-server"):
		return sessionFixture(harness.Codex)
	case slices.Contains(args, "--safe-mode"):
		return sessionFixture(harness.Claude)
	}
	return 30
}

func grokFixture() int {
	home := os.Getenv("GROK_HOME")
	if home == "" {
		return 10
	}
	if os.Getenv("XAI_API_KEY") != "" || os.Getenv("LIB_HARNESS_ACCOUNT_FIXTURE") != "" {
		return 14 // the parent's environment reached the launch
	}
	for _, entry := range nativecli.GrokReducedTelemetry {
		key, value, _ := strings.Cut(entry, "=")
		if os.Getenv(key) != value {
			return 11
		}
	}
	if cwd, _ := os.Getwd(); !strings.Contains(cwd, "agent-harness-account-") {
		return 12
	}
	written, _ := os.ReadFile(filepath.Join(home, "mode"))
	mode := string(written)
	if mode == "exit" {
		return 3
	}
	methods, err := os.Create(filepath.Join(home, "methods"))
	if err != nil {
		return 13
	}
	defer methods.Close()
	out := json.NewEncoder(os.Stdout)
	notice := func() {
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "_x.ai/mcp/servers_updated", "params": map[string]any{"servers": []any{}}})
	}
	lines := bufio.NewScanner(os.Stdin)
	for lines.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(lines.Bytes(), &m) != nil {
			return 14
		}
		fmt.Fprintln(methods, m.Method)
		reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
		switch m.Method {
		case "initialize":
			notice()
			// A request from the agent carries an id too; it is not the answer.
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "_x.ai/client/ping", "params": map[string]any{}})
			reply["result"] = json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}`)
		case "_x.ai/auth/check_subscription":
			notice()
			switch mode {
			case "hang":
				_ = os.WriteFile(filepath.Join(home, "waiting"), nil, 0o600)
				time.Sleep(time.Minute)
				return 15
			case "unsupported":
				reply["error"] = map[string]any{"code": -32601, "message": "Method not found", "data": "private diagnostic must-not-escape"}
			case "rejected":
				reply["error"] = map[string]any{"code": -32000, "message": "private diagnostic must-not-escape"}
			case "malformed":
				reply["result"] = json.RawMessage(`{"meta":{"email":"fixture@example.test"}}`)
			case "logged_out":
				reply["result"] = json.RawMessage(`{"authenticated":false,"meta":null}`)
			default:
				reply["result"] = json.RawMessage(grokSubscriptionFixture)
			}
		default:
			return 20
		}
		if out.Encode(reply) != nil {
			return 21
		}
	}
	return 0
}

func sessionFixture(engine harness.Engine) int {
	out := json.NewEncoder(os.Stdout)
	lines := bufio.NewScanner(os.Stdin)
	for lines.Scan() {
		var m map[string]json.RawMessage
		if json.Unmarshal(lines.Bytes(), &m) != nil {
			return 14
		}
		var method string
		id := m["id"]
		_ = json.Unmarshal(m["method"], &method)
		if engine == harness.Claude {
			var req struct{ Subtype string }
			_ = json.Unmarshal(m["request"], &req)
			method, id = req.Subtype, m["request_id"]
		}
		var result string
		switch method {
		case "initialize":
			result = `{}`
			if engine == harness.Claude {
				result = claudeInitFixture
			}
		case "initialized":
			continue
		case "account/read":
			result = codexAccountFixture
		case "account/rateLimits/read":
			result = codexRateLimitsFixture
		case "get_usage":
			result = claudeUsageFixture
		default:
			return 20
		}
		reply := map[string]any{"id": id, "result": json.RawMessage(result)}
		if engine == harness.Claude {
			reply = map[string]any{"type": "control_response", "response": map[string]any{"request_id": id, "subtype": "success", "response": json.RawMessage(result)}}
		}
		if out.Encode(reply) != nil {
			return 21
		}
	}
	return 0
}

func fixtureProvider(t *testing.T, engine harness.Engine, mode string) harness.Provider {
	t.Helper()
	t.Setenv("LIB_HARNESS_ACCOUNT_FIXTURE", "1")
	t.Setenv("LIB_HARNESS_ACCOUNT_MODE", mode)
	t.Setenv("XAI_API_KEY", "synthetic-key-must-not-reach-grok")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	return harness.Provider{Engine: engine, CLI: harness.CLI{Binary: bin, Home: home}}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func requireFacts(t *testing.T, err error, want harness.Facts) {
	t.Helper()
	facts, ok := harness.ErrorFacts(err)
	if !ok {
		t.Fatalf("untyped error: %v", err)
	}
	facts.ExitCode = nil
	if facts != want {
		t.Fatalf("facts: got %+v, want %+v", facts, want)
	}
	if strings.Contains(err.Error(), "must-not-escape") {
		t.Fatal("provider text in error")
	}
}

func TestGrokSubscription(t *testing.T) {
	p := fixtureProvider(t, harness.Grok, "")
	report, err := inspectGrok(testContext(t), p, p.CLI.Binary)
	if err != nil {
		t.Fatal(err)
	}
	a := report.Account
	if report.Engine != harness.Grok || !a.Known() || a.LoggedIn == nil || !*a.LoggedIn || a.Email != "fixture@example.test" || a.Plan != "Free" || a.AuthMethod != "Oidc" || a.Organization != "" {
		t.Fatalf("account: %+v", a)
	}
	if report.Quota.Known() || report.Quota.Reason == "" || report.Quota.Windows != nil || report.Credits.Known() || report.Credits.Reason == "" || report.Credits.Balance != nil {
		t.Fatalf("unsupported parts must be unknown with a reason: %+v", report)
	}
	methods, err := os.ReadFile(filepath.Join(p.CLI.Home, "methods"))
	if err != nil || string(methods) != "initialize\n_x.ai/auth/check_subscription\n" {
		t.Fatalf("methods sent: %q %v", methods, err)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), "fixture-team") {
		t.Fatal("unrecognized account fields escaped")
	}
}

func TestGrokLoggedOut(t *testing.T) {
	p := fixtureProvider(t, harness.Grok, "logged_out")
	report, err := inspectGrok(testContext(t), p, p.CLI.Binary)
	if err != nil || !report.Account.Known() || report.Account.LoggedIn == nil || *report.Account.LoggedIn || report.Account.Plan != "" {
		t.Fatalf("logout lost: %+v %v", report.Account, err)
	}
}

func TestGrokFailuresAreTyped(t *testing.T) {
	for mode, want := range map[string]harness.Facts{
		"unsupported": {Engine: harness.Grok, Operation: harness.Account, Family: harness.FailureCapability, Code: CodeMethodUnavailable},
		"rejected":    {Engine: harness.Grok, Operation: harness.Account, Family: harness.FailureRequest, Cause: harness.CauseUnknown, Code: CodeRejected},
		"malformed":   {Engine: harness.Grok, Operation: harness.Account, Family: harness.FailureRequest, Code: CodeInvalidResponse},
		"exit":        {Engine: harness.Grok, Operation: harness.Account, Family: harness.FailureProcess, Code: CodeProcessExited, Retryable: true},
	} {
		t.Run(mode, func(t *testing.T) {
			p := fixtureProvider(t, harness.Grok, mode)
			report, err := inspectGrok(testContext(t), p, p.CLI.Binary)
			requireFacts(t, err, want)
			if report.Account.Known() || report.Account.Reason == "" {
				t.Fatalf("failed inspection reported an account: %+v", report.Account)
			}
			if mode == "exit" {
				if facts, _ := harness.ErrorFacts(err); facts.ExitCode == nil || *facts.ExitCode != 3 {
					t.Fatalf("exit status lost: %+v", facts)
				}
			}
		})
	}
}

func TestGrokTimeout(t *testing.T) {
	p := fixtureProvider(t, harness.Grok, "hang")
	defer func(d time.Duration) { grokTimeout = d }(grokTimeout)
	grokTimeout = 500 * time.Millisecond
	_, err := inspectGrok(testContext(t), p, p.CLI.Binary)
	requireFacts(t, err, harness.Facts{Engine: harness.Grok, Operation: harness.Account, Family: harness.FailureRequest, Cause: harness.CauseTimeout, Code: CodeTimedOut, Retryable: true})
}

func TestGrokCancellationTerminatesCLI(t *testing.T) {
	p := fixtureProvider(t, harness.Grok, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := inspectGrok(ctx, p, p.CLI.Binary); done <- err }()
	deadline := time.After(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(p.CLI.Home, "waiting")); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("returned before the CLI was waiting: %v", err)
		case <-deadline:
			t.Fatal("CLI never reached the request")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled inspection: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled inspection did not reap the CLI")
	}
}

func TestInspectThroughSession(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude} {
		t.Run(string(engine), func(t *testing.T) {
			report, err := Inspect(testContext(t), fixtureProvider(t, engine, ""))
			if err != nil {
				t.Fatal(err)
			}
			if report.Engine != engine || !report.Account.Known() || !report.Quota.Known() || !report.Credits.Known() {
				t.Fatalf("report: %+v", report)
			}
			if report.Quota.Windows[0].Kind == "" || report.Credits.Enabled == nil || *report.Credits.Enabled {
				t.Fatalf("normalized parts lost: %+v", report)
			}
			raw, _ := json.Marshal(report)
			if strings.Contains(string(raw), "must-not-escape") || strings.Contains(string(raw), "fixture-account") {
				t.Fatal("provider text escaped")
			}
		})
	}
}

func TestInspectGatesBeforeLaunch(t *testing.T) {
	ctx := testContext(t)
	for name, tc := range map[string]struct {
		provider harness.Provider
		want     harness.Facts
	}{
		"api engine":   {harness.Provider{Engine: harness.OpenAICompatible}, harness.Facts{Engine: harness.OpenAICompatible, Operation: harness.Account, Family: harness.FailureCapability, Code: CodeUnsupportedEngine}},
		"unknown":      {harness.Provider{Engine: "fixture"}, harness.Facts{Engine: "fixture", Operation: harness.Account, Family: harness.FailureCapability, Code: CodeUnsupportedEngine}},
		"api config":   {harness.Provider{Engine: harness.Codex, API: harness.API{BaseURL: "https://example.test"}}, harness.Facts{Engine: harness.Codex, Operation: harness.Account, Family: harness.FailurePreflight, Code: "api_config_for_cli_engine"}},
		"no such file": {harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Binary: filepath.Join(t.TempDir(), "claude")}}, harness.Facts{Engine: harness.Claude, Operation: harness.Account, Family: harness.FailurePreflight, Code: CodeNotInstalled}},
	} {
		t.Run(name, func(t *testing.T) {
			report, err := Inspect(ctx, tc.provider)
			requireFacts(t, err, tc.want)
			if report.Account.Known() || report.Account.Reason == "" || report.Quota.Reason == "" || report.Credits.Reason == "" {
				t.Fatalf("refused inspection must still explain every part: %+v", report)
			}
		})
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(cancelled, harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Binary: "must-not-launch"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled before launch: %v", err)
	}
}

// Grok's inspection is reachable exactly when the library claims it.
func TestInspectGrokFollowsSupport(t *testing.T) {
	report, err := Inspect(testContext(t), fixtureProvider(t, harness.Grok, ""))
	if !harness.Support(harness.Grok, harness.Account, harness.Available).Usable() {
		requireFacts(t, err, harness.Facts{Engine: harness.Grok, Operation: harness.Account, Family: harness.FailureCapability, Code: CodeUnsupportedEngine})
		return
	}
	if err != nil || !report.Account.Known() {
		t.Fatalf("supported Grok inspection failed: %+v %v", report, err)
	}
}

func TestSessionFailuresBecomeAccountFacts(t *testing.T) {
	exit := 2
	for name, tc := range map[string]struct {
		err  error
		want harness.Facts
		is   error
	}{
		"process":     {&session.ProcessError{Engine: harness.Codex, Code: session.ProcessExited, ExitCode: &exit}, harness.Facts{Engine: harness.Codex, Operation: harness.Account, Family: harness.FailureProcess, Code: CodeProcessExited, Retryable: true}, session.ErrTransport},
		"unsupported": {errors.Join(nil, session.ErrUnsupported), harness.Facts{Engine: harness.Codex, Operation: harness.Account, Family: harness.FailureCapability, Code: CodeMethodUnavailable}, session.ErrUnsupported},
		"refused":     {&session.UnsupportedError{Engine: harness.Codex, Code: session.RefusedHome}, harness.Facts{Engine: harness.Codex, Operation: harness.Account, Family: harness.FailurePreflight, Code: session.RefusedHome}, session.ErrUnsupported},
		"protocol":    {errors.Join(session.ErrProtocol, session.ErrRejected), harness.Facts{Engine: harness.Codex, Operation: harness.Account, Family: harness.FailureRequest, Cause: harness.CauseUnknown, Code: CodeRejected}, session.ErrProtocol},
		"malformed":   {session.ErrProtocol, harness.Facts{Engine: harness.Codex, Operation: harness.Account, Family: harness.FailureRequest, Code: CodeInvalidResponse}, session.ErrProtocol},
		"deadline":    {context.DeadlineExceeded, harness.Facts{Engine: harness.Codex, Operation: harness.Account, Family: harness.FailureRequest, Cause: harness.CauseTimeout, Code: CodeTimedOut, Retryable: true}, context.DeadlineExceeded},
		"closed":      {session.ErrClosed, harness.Facts{Engine: harness.Codex, Operation: harness.Account, Family: harness.FailureProcess, Code: CodeTransport, Retryable: true}, session.ErrClosed},
	} {
		t.Run(name, func(t *testing.T) {
			err := fromSession(harness.Codex, tc.err)
			requireFacts(t, err, tc.want)
			if !errors.Is(err, tc.is) {
				t.Fatal("underlying error lost")
			}
		})
	}
}
