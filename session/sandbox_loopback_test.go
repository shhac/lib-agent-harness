//go:build !windows

package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
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

// Claude Code 2.1.283 in dontAsk mode auto-allows a sandboxed command only
// when it is a flat list of plain commands.
func TestLoopbackCanaryIsFlat(t *testing.T) {
	script := loopbackCanary(1, "192.0.2.1", 443, 2)
	for _, refused := range []string{"()", "(", "{", "exit"} {
		if strings.Contains(script, refused) {
			t.Errorf("canary contains %q:\n%s", refused, script)
		}
	}
}

func TestCanaryProviderScriptsOneCallAndReadsItsResult(t *testing.T) {
	result := make(chan string, 1)
	server := httptest.NewServer(canaryProvider("echo canary", result))
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
