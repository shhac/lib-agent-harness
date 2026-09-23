//go:build !windows

package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A fake harness is this test binary re-executed as an installed CLI. It speaks
// just enough of each native protocol to drive the real probe and the real
// launch, and it makes the outbound provider request a scenario asks for. It
// never contacts anything but the loopback provider the probe itself started.
//
// The probe runs a harness with an environment stripped to a disposable home,
// so the scenario cannot travel in the caller's environment. A small script
// carries it instead, and doubles as the binary whose identity the verification
// cache records.
const (
	fakeScenarioEnv = "AGENT_HARNESS_TEST_FAKE"
	fakeLogEnv      = "AGENT_HARNESS_TEST_FAKE_LOG"
	// fakeRefreshEnv names a login a fake Codex session writes into its runtime
	// home, standing in for the harness refreshing its credential.
	fakeRefreshEnv = "AGENT_HARNESS_TEST_FAKE_REFRESH"
)

// Probe scenarios: what the fake harness sends to the provider it was given.
const (
	fakeClean    = "clean"    // exactly the hosted tools
	fakeBuiltIn  = "builtin"  // the hosted tools and a built-in
	fakeNoTools  = "no-tools" // an auxiliary request carrying nothing
	fakeSilent   = "silent"   // no request at all
	fakeGarbage  = "garbage"  // a body that is not JSON
	fakeListed   = "listed"   // Codex: deferred tools, served over the channel
	fakeUnlisted = "unlisted" // Codex: deferred tools, never asked for
)

// fakeHarness writes an executable standing in for an installed CLI and returns
// its path and the file it logs each invocation to.
func fakeHarness(t *testing.T, scenario string, env ...string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "invocations")
	script := "#!/bin/sh\nunset LIB_HARNESS_SESSION_FIXTURE\n"
	for _, entry := range append([]string{fakeScenarioEnv + "=" + scenario, fakeLogEnv + "=" + log}, env...) {
		key, value, _ := strings.Cut(entry, "=")
		script += "export " + key + "=" + shellQuote(value) + "\n"
	}
	script += "exec " + shellQuote(os.Args[0]) + ` "$@"` + "\n"
	binary := filepath.Join(dir, "harness")
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return binary, log
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

// invocations counts how often the fake ran as a given kind of process.
func invocations(t *testing.T, log, kind string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if line == kind {
			count++
		}
	}
	return count
}

// runFakeHarness is the fake's whole life. It returns the exit status.
func runFakeHarness(scenario string) int {
	args := os.Args[1:]
	switch {
	case len(args) > 0 && args[0] == "sandbox":
		return fakeCodexCanary(scenario, args)
	case slices.Contains(args, "sandbox") && slices.Contains(args, "status"):
		return fakeClaudeSandboxStatus(scenario)
	case len(args) > 0 && args[0] == "-p":
		return fakeClaude(scenario, args)
	case len(args) > 1 && args[0] == "debug" && args[1] == "models":
		logInvocation("catalog")
		_, err := os.Stdout.WriteString(`{"models":[{"slug":"picked","default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"high"}],"base_instructions":"native coding instructions","shell_type":"local"}]}`)
		if err != nil {
			return 2
		}
		return 0
	case len(args) > 0 && args[0] == "app-server":
		return fakeCodex(scenario, args)
	}
	return 2
}

func logInvocation(kind string) {
	f, err := os.OpenFile(os.Getenv(fakeLogEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	defer f.Close()
	if _, err = f.WriteString(kind + "\n"); err != nil {
		os.Exit(2)
	}
}

// fakeClaude reads the probe's user frame and sends one request describing the
// scenario's tool surface, or, launched for real, runs a minimal session.
func fakeClaude(scenario string, args []string) int {
	var hosted []string
	session := ""
	for i, arg := range args {
		if value, found := strings.CutPrefix(arg, "--allowedTools="); found {
			hosted = strings.Split(value, ",")
		}
		if (arg == "--session-id" || arg == "--resume") && i+1 < len(args) {
			session = args[i+1]
		}
	}
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 4096), MaxFrameBytes)
	if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" {
		logInvocation("probe")
		var frame struct{ Type string }
		if !input.Scan() || json.Unmarshal(input.Bytes(), &frame) != nil || frame.Type != "user" {
			return 2
		}
		tools := []map[string]any{}
		names := hosted
		switch scenario {
		case fakeBuiltIn:
			names = append(append([]string(nil), hosted...), "Bash")
		case fakeNoTools:
			names = nil
		case fakeSilent:
			return 0
		case fakeGarbage:
			return fakePost(base+"/v1/messages", []byte("not a request"))
		}
		for _, name := range names {
			tools = append(tools, map[string]any{"name": name, "input_schema": map[string]any{"type": "object"}})
		}
		body, _ := json.Marshal(map[string]any{"model": "picked", "tools": tools, "messages": []any{map[string]any{"role": "user", "content": "Capability check only."}}})
		return fakePost(base+"/v1/messages", body)
	}
	logInvocation("session")
	out := json.NewEncoder(os.Stdout)
	server := ""
	if len(hosted) > 0 {
		server = strings.Split(hosted[0], "__")[1]
	}
	// Emitted before any prompt, as the installed CLI does, so the session's
	// startup cross-check has something to judge.
	if out.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": session, "tools": hosted, "mcp_servers": []any{map[string]any{"name": server, "status": "connected"}}}) != nil {
		return 2
	}
	for input.Scan() {
		var frame map[string]json.RawMessage
		if json.Unmarshal(input.Bytes(), &frame) != nil {
			return 2
		}
		var reply []map[string]any
		switch str(frame, "type") {
		case "control_request":
			reply = append(reply, map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": str(frame, "request_id"), "response": map[string]any{}}})
		case "user":
			reply = append(reply,
				map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"id": "reply", "content": []any{map[string]any{"type": "text", "text": "fake answer"}}}},
				map[string]any{"type": "result", "subtype": "success", "session_id": session, "is_error": false, "result": "fake answer", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
		}
		for _, message := range reply {
			if out.Encode(message) != nil {
				return 2
			}
		}
	}
	return 0
}

var fakeBaseURL = regexp.MustCompile(`base_url="([^"]+)"`)

// fakeCodex answers the app-server handshake. Probed, it sends the deferred
// request shape the installed CLI sends, optionally after asking the tool
// channel for its tools the way a harness loading an MCP server does.
func fakeCodex(scenario string, args []string) int {
	overrides := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-c" {
			key, value, _ := strings.Cut(args[i+1], "=")
			overrides[key] = value
		}
	}
	provider := fakeBaseURL.FindStringSubmatch(overrides["model_providers.harness_probe"])
	if provider != nil {
		logInvocation("probe")
	} else {
		logInvocation("session")
		if refreshed := os.Getenv(fakeRefreshEnv); refreshed != "" {
			if os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), codexCredentialFile), []byte(refreshed), 0600) != nil {
				return 2
			}
		}
	}
	out := json.NewEncoder(os.Stdout)
	input := bufio.NewScanner(os.Stdin)
	input.Buffer(make([]byte, 4096), MaxFrameBytes)
	for input.Scan() {
		var frame map[string]json.RawMessage
		if json.Unmarshal(input.Bytes(), &frame) != nil {
			return 2
		}
		id := frame["id"]
		var result any
		switch str(frame, "method") {
		case "initialized":
			continue
		case "initialize":
			result = map[string]any{}
		case "thread/start", "thread/resume":
			result = fakeCodexThread(scenario, overrides)
		case "turn/start":
			if out.Encode(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": "fake-turn"}}}) != nil {
				return 2
			}
			if provider == nil {
				if out.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "fake-thread", "turn": map[string]any{"id": "fake-turn", "status": "completed"}}}) != nil {
					return 2
				}
				continue
			}
			if scenario == fakeListed && !fakeListTools(overrides) {
				return 2
			}
			if status := fakePost(provider[1]+"/responses", fakeCodexRequest()); status != 0 {
				return status
			}
			continue
		default:
			if out.Encode(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "unsupported"}}) != nil {
				return 2
			}
			continue
		}
		if out.Encode(map[string]any{"id": id, "result": result}) != nil {
			return 2
		}
	}
	return 0
}

// fakeCodexRequest is the shape captured from the installed CLI: MCP tools
// deferred behind tool_search, with only the mediated helpers present.
func fakeCodexRequest() []byte {
	body, _ := json.Marshal(map[string]any{
		"model":     "picked",
		"reasoning": map[string]any{"effort": "high"},
		"input": []any{
			map[string]any{"type": "additional_tools", "role": "system", "tools": []any{
				map[string]any{"type": "namespace", "name": "functions", "tools": []any{
					map[string]any{"type": "function", "name": "list_mcp_resources"},
					map[string]any{"type": "function", "name": "read_mcp_resource"},
				}},
				map[string]any{"type": "tool_search"},
			}},
			map[string]any{"type": "message", "role": "user", "content": "Capability check only."},
		},
	})
	return body
}

// fakeListTools connects to the tool channel named in the launch overrides and
// asks for its tools, as a harness loading the configured MCP server does.
func fakeListTools(overrides map[string]string) bool {
	var socket, secretFile string
	for key, value := range overrides {
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			continue
		}
		switch {
		case strings.HasSuffix(key, ".env."+BridgeSocketEnv):
			socket = unquoted
		case strings.HasSuffix(key, ".env."+BridgeSecretEnv):
			secretFile = unquoted
		}
	}
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return false
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return false
	}
	defer conn.Close()
	if conn.SetDeadline(time.Now().Add(10*time.Second)) != nil {
		return false
	}
	hello, _ := json.Marshal(map[string]string{"secret": strings.TrimSpace(string(secret))})
	list, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	if _, err = conn.Write(append(append(hello, '\n'), append(list, '\n')...)); err != nil {
		return false
	}
	reply := bufio.NewScanner(conn)
	reply.Buffer(make([]byte, 4096), MaxToolRequestBytes)
	return reply.Scan() && bytes.Contains(reply.Bytes(), []byte(`"tools"`))
}

func fakePost(url string, body []byte) int {
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return 2
	}
	_ = response.Body.Close()
	return 0
}
