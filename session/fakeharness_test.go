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
	// fakePersistEnv makes a fake session keep its conversation the way the
	// installed CLIs do, and refuse to resume one it does not have. Claude writes
	// a transcript under its config directory's projects folder; Codex keeps a
	// thread record in its home and rejects thread/resume without one.
	fakePersistEnv = "AGENT_HARNESS_TEST_FAKE_PERSIST"
)

// fakeCompactCue in a turn's input makes the fake compact that conversation
// during the turn, reporting it the way each installed CLI does.
const fakeCompactCue = "[compact]"

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
	logInvocation("args:" + string(mustMarshal(args)))
	out := json.NewEncoder(os.Stdout)
	persist := os.Getenv(fakePersistEnv) != ""
	if persist && slices.Contains(args, "--resume") && !fakeClaudeHasConversation(session) {
		// What the installed CLI does, checked against 2.1.282: a line on standard
		// error, an error result naming the session, and exit status 1 — with no
		// init frame and no reply to the initialize request.
		_, _ = os.Stderr.WriteString("No conversation found with session ID: " + session + "\n")
		_ = out.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "session_id": session, "errors": []string{"No conversation found with session ID: " + session}})
		return 1
	}
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
			var message struct {
				Message struct{ Content string } `json:"message"`
			}
			_ = json.Unmarshal(input.Bytes(), &message)
			logInvocation("input:" + string(mustMarshal(message.Message.Content)))
			if persist && !fakeClaudeRecord(session, input.Bytes()) {
				return 2
			}
			if strings.Contains(message.Message.Content, fakeCompactCue) {
				reply = append(reply, map[string]any{"type": "system", "subtype": "compact_boundary", "session_id": session, "uuid": "boundary", "compact_metadata": map[string]any{"trigger": "auto", "pre_tokens": 1000}})
			}
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

// fakeClaudeProject is where the installed CLI keeps a working directory's
// transcripts, written independently of the library's own rule so the two can
// disagree: every character that is not a letter or digit of the resolved
// working directory becomes a dash. Test paths stay under the length at which
// the CLI starts abbreviating.
func fakeClaudeProject() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
		cwd = resolved
	}
	return filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(cwd, "-"))
}

// fakeClaudeRecord appends a user frame to the session's transcript.
func fakeClaudeRecord(session string, frame []byte) bool {
	dir := fakeClaudeProject()
	if dir == "" || os.MkdirAll(dir, 0700) != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(dir, session+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Write(append(append([]byte(nil), frame...), '\n'))
	return err == nil
}

// fakeClaudeHasConversation looks for the transcript in any project folder, as
// the installed CLI's resume lookup does, and requires a recorded message.
func fakeClaudeHasConversation(session string) bool {
	matches, _ := filepath.Glob(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "*", session+".jsonl"))
	for _, match := range matches {
		raw, err := os.ReadFile(match)
		if err == nil && bytes.Contains(raw, []byte(`"type":"user"`)) {
			return true
		}
	}
	return false
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
	persist := os.Getenv(fakePersistEnv) != "" && provider == nil
	threads := filepath.Join(os.Getenv("CODEX_HOME"), "fake-threads")
	if provider != nil {
		logInvocation("probe")
	} else {
		logInvocation("session")
		logInvocation("args:" + string(mustMarshal(args)))
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
			if persist {
				if str(frame, "method") == "thread/resume" {
					var p struct {
						ThreadID string `json:"threadId"`
					}
					_ = json.Unmarshal(frame["params"], &p)
					logInvocation("resume:" + p.ThreadID)
					if _, err := os.Stat(filepath.Join(threads, p.ThreadID)); err != nil {
						// The installed CLI's answer, checked against 0.156.1.
						if out.Encode(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": "no rollout found for thread id " + p.ThreadID}}) != nil {
							return 2
						}
						continue
					}
				} else if os.MkdirAll(threads, 0700) != nil || os.WriteFile(filepath.Join(threads, "fake-thread"), nil, 0600) != nil {
					return 2
				}
			}
		case "thread/compact/start":
			if out.Encode(map[string]any{"id": id, "result": map[string]any{}}) != nil {
				return 2
			}
			if !fakeCodexTurn(out, "compact-turn", true) {
				return 2
			}
			continue
		case "turn/start":
			if out.Encode(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": "fake-turn"}}}) != nil {
				return 2
			}
			if provider == nil {
				var p struct {
					Input []struct{ Text string } `json:"input"`
				}
				_ = json.Unmarshal(frame["params"], &p)
				text := ""
				if len(p.Input) > 0 {
					text = p.Input[0].Text
				}
				logInvocation("input:" + string(mustMarshal(text)))
				if !fakeCodexTurn(out, "fake-turn", strings.Contains(text, fakeCompactCue)) {
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

// fakeCodexTurn finishes a turn, compacting the thread during it when asked,
// with the item the installed CLI's schema names for a completed compaction.
func fakeCodexTurn(out *json.Encoder, turn string, compact bool) bool {
	frames := []map[string]any{}
	if turn == "compact-turn" {
		frames = append(frames, map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "fake-thread", "turn": map[string]any{"id": turn}}})
	}
	if compact {
		item := map[string]any{"type": "contextCompaction", "id": "compaction"}
		frames = append(frames,
			map[string]any{"method": "item/started", "params": map[string]any{"threadId": "fake-thread", "turnId": turn, "item": item}},
			map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "fake-thread", "turnId": turn, "item": item}})
	}
	frames = append(frames, map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "fake-thread", "turn": map[string]any{"id": turn, "status": "completed"}}})
	for _, frame := range frames {
		if out.Encode(frame) != nil {
			return false
		}
	}
	return true
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
