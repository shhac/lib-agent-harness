//go:build !windows

package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestJudgeLoopback(t *testing.T) {
	proved := strings.Join([]string{loopbackReached, loopbackBound, canaryRan}, "\n")
	for _, tc := range []struct {
		name        string
		output      string
		loopReached bool
		code        string
	}{
		{"proved", proved, true, ""},
		{"off-machine reached", proved + "\n" + loopbackOutside, true, CapabilitySandboxNotEnforced},
		{"loopback listener never saw the canary", proved, false, CapabilitySandboxUnavailable},
		{"no bind", loopbackReached + "\n" + canaryRan, true, CapabilitySandboxUnavailable},
		{"no loopback", loopbackBound + "\n" + canaryRan, true, CapabilitySandboxUnavailable},
		{"no client", canaryNoClient + "\n" + proved, true, CapabilitySandboxUnavailable},
		{"did not finish", loopbackReached + "\n" + loopbackBound, true, CapabilitySandboxUnavailable},
		{"refused by permissions", "Permission to use Bash has been denied", false, CapabilitySandboxUnavailable},
	} {
		err := judgeLoopback(tc.output, tc.loopReached)
		if tc.code == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if got := capabilityCode(t, err); got != tc.code {
			t.Errorf("%s: %s want %s", tc.name, got, tc.code)
		}
	}
}

// The real canary, run with no sandbox at all, must be caught: the witness it
// is handed is reachable, so the judge sees an open network.
func TestLoopbackCanaryUnsandboxedIsCaught(t *testing.T) {
	testenv.RequireLoopback(t) // Provider fixtures and canaries need a real loopback bind.
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("no nc on this machine")
	}
	loop, loopWait, loopReached, err := countingListener("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { loop.Close(); loopWait.Wait() }()
	witness, witnessWait, _, err := countingListener("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { witness.Close(); witnessWait.Wait() }()
	bindPort, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	script := loopbackCanary(loop.Addr().(*net.TCPAddr).Port, "127.0.0.1", witness.Addr().(*net.TCPAddr).Port, bindPort)
	output, err := exec.Command("/bin/sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	time.Sleep(300 * time.Millisecond)
	for _, want := range []string{loopbackReached, loopbackBound, loopbackOutside, canaryRan} {
		if !strings.Contains(string(output), want+"\n") {
			t.Errorf("canary output %q lacks %q", output, want)
		}
	}
	if got := capabilityCode(t, judgeLoopback(string(output), loopReached())); got != CapabilitySandboxNotEnforced {
		t.Fatalf("an open network judged %s", got)
	}
}

// flatCanary limits shell syntax and commands to the original nc shape.
// It does NOT approximate dontAsk approval: the old check accepted Perl -e,
// which the owner-run CLI refused. Only owner evidence proves auto-allow.
func flatCanary(script string) bool {
	var outside strings.Builder
	quoted := false
	for i := 0; i < len(script); i++ {
		c := script[i]
		if c == '\\' && !quoted {
			if i+1 >= len(script) {
				return false
			}
			i++
			outside.WriteByte(' ')
			continue
		}
		if c == '\'' {
			quoted = !quoted
			outside.WriteByte(' ')
			continue
		}
		if !quoted {
			outside.WriteByte(c)
		}
	}
	if quoted {
		return false
	}
	plain := outside.String()
	if strings.ContainsAny(plain, "(){}`;") || strings.Contains(plain, "$(") {
		return false
	}
	if regexp.MustCompile(`(^|[^a-zA-Z0-9_])exit([^a-zA-Z0-9_]|$)`).MatchString(plain) {
		return false
	}
	// Only &&, || and & join commands; a pipe is not an allowed join.
	plain = strings.ReplaceAll(plain, "||", "\n")
	plain = strings.ReplaceAll(plain, ">&", ">")
	plain = strings.ReplaceAll(plain, "<&", "<")
	if strings.Contains(plain, "|") {
		return false
	}
	for _, command := range regexp.MustCompile("[\n&]+").Split(plain, -1) {
		words := strings.Fields(command)
		if len(words) == 0 {
			continue
		}
		switch words[0] {
		case "nc", "echo", "sleep":
		case "command":
			if len(words) < 3 || words[1] != "-v" || words[2] != "nc" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func TestLoopbackCanaryIsFlat(t *testing.T) {
	if !flatCanary(loopbackCanary(1, "192.0.2.1", 443, 2)) {
		t.Fatal("legacy canary is not flat")
	}
	for _, bad := range []string{"(echo x)", "f() { echo x; }", "{ echo x; }", "exit 0", "echo $(id)", "echo `id`", "echo x | cat", "/usr/bin/perl -e 'print 1'", "python3 -c 'print(1)'", "sh -c 'echo x'"} {
		if flatCanary(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	if !flatCanary("echo '( ) { } exit $(id) `id`'") {
		t.Fatal("quoted shell syntax rejected")
	}
}

func TestClaudeInterfaceJudgment(t *testing.T) {
	attempts := sandboxprobe.InterfaceAttempts([]string{"192.0.2.1"}, false, true)
	base := "loopback\nbound\ncanary-ran\n"
	good := "interface-result:0:0\ninterface-result:1:0\ninterface-result:2:65\ninterface-canary-ran\n"
	for _, tc := range []struct{ name, output, code string }{
		{"proved", base + good, ""},
		{"escape wins", "outside\ninterface-result:no\n", CapabilitySandboxNotEnforced},
		{"missing loopback", good, CapabilitySandboxUnavailable},
		{"missing index", base + strings.ReplaceAll(good, "interface-result:1:0\n", ""), CapabilitySandboxUnavailable},
		{"duplicate", base + good + "interface-result:0:0\n", CapabilitySandboxUnavailable},
		{"garbled", base + strings.ReplaceAll(good, "1:0", "1:bad"), CapabilitySandboxUnavailable},
		{"missing Perl", base + strings.ReplaceAll(good, "1:0", "1:999"), CapabilitySandboxUnavailable},
		{"no interface terminator", base + strings.ReplaceAll(good, "interface-canary-ran\n", ""), CapabilitySandboxUnavailable},
		{"TCP denied", base + strings.ReplaceAll(good, "0:0", "0:13"), CapabilityLoopbackClaimChanged},
		{"UDP denied", base + strings.ReplaceAll(good, "1:0", "1:1"), CapabilityLoopbackClaimChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence, err := judgeClaudeLoopback(tc.output, true, attempts)
			if tc.code != "" {
				if capabilityCode(t, err) != tc.code || len(evidence.observations) != 0 {
					t.Fatal("wrong refusal or unsettled observations")
				}
			} else if err != nil || evidence.observations[2].Errno != 65 || evidence.detail != "binds on all local interfaces: observed allowed" {
				t.Fatalf("%+v %v", evidence, err)
			}
		})
	}
	unavailable := fmt.Sprintf("interface-result:0:%d\ninterface-result:1:%d\ninterface-result:2:65\ninterface-canary-ran\n", sandboxprobe.AddressUnavailableErrno(), sandboxprobe.AddressUnavailableErrno())
	evidence, err := judgeClaudeLoopback(base+unavailable, true, attempts)
	if err != nil || evidence.detail != "no interface binds available; wildcard binds tested" {
		t.Fatalf("%+v %v", evidence, err)
	}
}

func TestLoopbackEvidencePublication(t *testing.T) {
	cache := &verificationCache{seen: map[string]bool{}}
	attempts := sandboxprobe.InterfaceAttempts([]string{"192.0.2.1"}, false, true)
	evidence, err := judgeClaudeLoopback("loopback\nbound\ncanary-ran\ninterface-result:0:0\ninterface-result:1:0\ninterface-result:2:65\ninterface-canary-ran\n", true, attempts)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.settleSandbox(context.Background(), harness.Claude, "proved", evidence, nil); err != nil {
		t.Fatal(err)
	}
	evidence.observations[0].Errno = 13
	cache.mu.Lock()
	if !cache.seen["proved"] || cache.loopback["proved"].observations[0].Errno != 0 {
		t.Fatal("evidence missing or aliases caller")
	}
	cache.mu.Unlock()
	for _, output := range []string{"", "outside\n"} {
		evidence, err := judgeClaudeLoopback(output, true, attempts)
		if err == nil || len(evidence.observations) != 0 {
			t.Fatal("refusal retained observations")
		}
		if cache.settleSandbox(context.Background(), harness.Claude, "refused", evidence, err) == nil || cache.holds("refused") {
			t.Fatal("refusal published evidence")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := capabilityCode(t, cache.settleSandbox(ctx, harness.Claude, "cancelled", evidence, nil)); code != CapabilityProbeTimeout || cache.holds("cancelled") {
		t.Fatal("cancelled proof published evidence")
	}
}

func TestCanaryProviderScriptsOneCallAndReadsItsResult(t *testing.T) {
	testenv.RequireLoopback(t) // Provider fixtures and canaries need a real loopback bind.
	result := make(chan string, 1)
	server := &httptest.Server{Config: &http.Server{Handler: canaryProvider("echo canary", result)}, Listener: testenv.Listen(t, "tcp", "127.0.0.1:0")}
	server.Start()
	defer server.Close()
	post := func(body string) (*http.Response, string) {
		t.Helper()
		response, err := http.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		return response, string(raw)
	}
	turn := `{"stream":true,"tools":[{"name":"Read"},{"name":"Bash"}],"messages":[{"role":"user","content":"check"}]}`
	if response, _ := post(`{"stream":true,"tools":[{"name":"Read"}],"messages":[]}`); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a turn without a shell got %d", response.StatusCode)
	}
	response, body := post(turn)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, loopbackCanaryID) {
		t.Fatalf("first turn: %d %s", response.StatusCode, body)
	}
	var input struct {
		Command string `json:"command"`
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "input_json_delta") {
			continue
		}
		var event struct {
			Delta struct {
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(event.Delta.PartialJSON), &input); err != nil {
			t.Fatal(err)
		}
	}
	if input.Command != "echo canary" {
		t.Fatalf("scripted command %q", input.Command)
	}
	if response, _ := post(turn); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a second turn got %d", response.StatusCode)
	}
	var reply bytes.Buffer
	_ = json.NewEncoder(&reply).Encode(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "check"},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": loopbackCanaryID, "content": []any{map[string]any{"type": "text", "text": "loopback"}}}}},
	}})
	if response, _ := post(reply.String()); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the result request got %d", response.StatusCode)
	}
	select {
	case got := <-result:
		if strings.TrimSpace(got) != "loopback" {
			t.Fatalf("result %q", got)
		}
	default:
		t.Fatal("the canary's result was not read")
	}
}

func TestLoopbackSettingsAndRefusals(t *testing.T) {
	o := sandboxOptions(t, harness.Claude, "claude", true)
	if strings.Contains(claudeSandboxSettings(o), "allowLocalBinding") {
		t.Fatal("local binding without Loopback")
	}
	plain := reference(o, "id").ConfigHash
	o.Sandbox.Loopback = true
	var settings struct {
		Sandbox struct {
			Network map[string]any `json:"network"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(claudeSandboxSettings(o)), &settings); err != nil {
		t.Fatal(err)
	}
	network := settings.Sandbox.Network
	if network["allowLocalBinding"] != true || network["strictAllowlist"] != true || len(network["allowedDomains"].([]any)) != 0 {
		t.Fatalf("network %v", network)
	}
	if reference(o, "id").ConfigHash == plain {
		t.Fatal("Loopback does not change the session's digest")
	}

	codex := sandboxOptions(t, harness.Codex, "codex", true)
	codex.Sandbox.Loopback = true
	_, err := normalize(codex)
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Code != RefusedNotOffered {
		t.Fatalf("Codex loopback: %v", err)
	}
}

func TestVerificationCacheHasOnlySettledLoopbackEntries(t *testing.T) {
	cache := &verificationCache{seen: map[string]bool{}}
	cache.record("native")
	cache.recordLoopback("base-sandbox", loopbackEvidence{})
	if cache.loopback != nil {
		t.Fatal("non-loopback verification allocated observations")
	}
	if !cache.holds("native") || !cache.holds("base-sandbox") {
		t.Fatal("ordinary verification lost")
	}
	cache.recordLoopback("loopback", loopbackEvidence{observations: []sandboxprobe.InterfaceObservation{{Address: "192.0.2.1", Operation: "bind", Errno: 0}}})
	cache.record("another-native")
	if len(cache.loopback) != 1 {
		t.Fatal("non-loopback key polluted observations")
	}
}
