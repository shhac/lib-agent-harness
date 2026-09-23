package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// A real child process speaking only the inspection protocol. No models,
// accounts or credentials are used. Any thread/start or user prompt fails.
func init() {
	if os.Getenv("LIB_HARNESS_TELEMETRY_FIXTURE") != "1" {
		return
	}
	os.Exit(telemetryFixture())
}
func telemetryFixture() int {
	engine := Codex
	home := os.Getenv("CODEX_HOME")
	if slices.Contains(os.Args, "--safe-mode") {
		engine = Claude
		home = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	if home == "" {
		return 10
	}
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		if os.Getenv(key) != "" {
			return 11
		}
	}
	cwd, _ := os.Getwd()
	if cwd == home || !strings.Contains(cwd, "agent-harness-inspect-") {
		return 12
	}
	if os.WriteFile(filepath.Join(home, "started"), []byte(cwd), 0600) != nil {
		return 13
	}
	mode := os.Getenv("LIB_HARNESS_TELEMETRY_MODE")
	scanner := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var m map[string]json.RawMessage
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			return 14
		}
		method, id, params := str(m, "method"), m["id"], map[string]any{}
		_ = json.Unmarshal(m["params"], &params)
		if engine == Claude {
			if str(m, "type") != "control_request" {
				return 15
			}
			var req map[string]json.RawMessage
			_ = json.Unmarshal(m["request"], &req)
			method = str(req, "subtype")
			id = m["request_id"]
			_ = json.Unmarshal(m["request"], &params)
		}
		result := json.RawMessage(`{}`)
		switch method {
		case "initialize":
			if engine == Codex {
				if os.WriteFile(filepath.Join(home, "initialize"), m["params"], 0600) != nil {
					return 22
				}
			}
			if engine == Claude {
				result = json.RawMessage(`{"account":{"email":"fixture@example.test","subscriptionType":"max","apiProvider":"firstParty"}}`)
			}
		case "initialized":
			if os.WriteFile(filepath.Join(home, "initialized"), nil, 0600) != nil {
				return 23
			}
			continue
		case "account/read":
			if engine != Codex || params["refreshToken"] != false {
				return 16
			}
			result = json.RawMessage(`{"requiresOpenaiAuth":true,"account":{"type":"chatgpt","email":"fixture@example.test","planType":"pro"}}`)
		case "account/rateLimits/read", "get_usage":
			if engine == Claude && (method != "get_usage" || params["skip_behaviors"] != true) {
				return 17
			}
			if engine == Codex && method != "account/rateLimits/read" {
				return 18
			}
			if mode == "hang" {
				os.WriteFile(filepath.Join(home, "waiting"), nil, 0600)
				time.Sleep(time.Minute)
				return 19
			}
			if mode == "unsupported" {
				if engine == Codex {
					_ = out.Encode(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "private diagnostic must-not-escape"}})
				} else {
					_ = out.Encode(map[string]any{"type": "control_response", "response": map[string]any{"request_id": id, "subtype": "error", "error": "Unsupported control request subtype: get_usage; private diagnostic must-not-escape"}})
				}
				continue
			}
			result = json.RawMessage(`{"rateLimits":{"primary":{"usedPercent":12.5,"windowDurationMins":300,"resetsAt":1900000000}}}`)
			if engine == Claude {
				result = json.RawMessage(`{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":12.5,"resets_at":"2030-01-01T00:00:00Z"}}}`)
			}
		default:
			return 20
		}
		reply := map[string]any{"id": id, "result": result}
		if engine == Claude {
			reply = map[string]any{"type": "control_response", "response": map[string]any{"request_id": id, "subtype": "success", "response": result}}
		}
		if out.Encode(reply) != nil {
			return 21
		}
	}
	return 0
}

func telemetryFixtureOptions(t *testing.T, engine Engine) Options {
	t.Helper()
	t.Setenv("LIB_HARNESS_TELEMETRY_FIXTURE", "1")
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		t.Setenv(key, "synthetic-must-not-reach-child")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Options{Engine: engine, Binary: bin, Home: t.TempDir(), WorkDir: "must-not-use-project", Instructions: Instructions{Mode: Append, Text: "must-not-send"}}
}

func TestInspectUsesConfiguredCLIAndHomeWithoutInference(t *testing.T) {
	for _, engine := range []Engine{Codex, Claude} {
		t.Run(string(engine), func(t *testing.T) {
			o := telemetryFixtureOptions(t, engine)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, err := Inspect(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Account.Known() || !got.Quota.Known() || len(got.Quota.Windows) != 1 || *got.Quota.Windows[0].UsedPercent != 12.5 {
				t.Fatalf("bad inspection: %+v", got)
			}
			if got.Capabilities.Account.Availability != Native || got.Capabilities.Quota.Availability != Native || got.Capabilities.Start.Availability != Unknown {
				t.Fatal("inspection falsely acknowledged session support")
			}
			cwd, err := os.ReadFile(filepath.Join(o.Home, "started"))
			if err != nil {
				t.Fatal("child did not receive selected home")
			}
			if _, err = os.Stat(string(cwd)); !os.IsNotExist(err) {
				t.Fatal("inspection scratch directory not cleaned")
			}
			if engine == Codex {
				requireSessionHandshake(t, o.Home)
			}
		})
	}
}

// Inspect opens the app-server exactly as a session and its probe do. A
// handshake that differed would make an inspection evidence about a client the
// library never runs.
func requireSessionHandshake(t *testing.T, home string) {
	t.Helper()
	sent, err := os.ReadFile(filepath.Join(home, "initialize"))
	if err != nil {
		t.Fatal("inspection did not initialize the app-server")
	}
	var session json.RawMessage
	w := &fakeWire{requestFn: func(_ string, p map[string]any) (json.RawMessage, error) {
		session, _ = json.Marshal(p)
		return json.RawMessage(`{}`), nil
	}}
	if err = codexHandshake(testContext(t), w, false); err != nil {
		t.Fatal(err)
	}
	var inspected, opened any
	if json.Unmarshal(sent, &inspected) != nil || json.Unmarshal(session, &opened) != nil || !reflect.DeepEqual(inspected, opened) {
		t.Fatalf("inspection initialized with %s; sessions initialize with %s", sent, session)
	}
	if _, err = os.Stat(filepath.Join(home, "initialized")); err != nil {
		t.Fatal("inspection did not complete the handshake")
	}
}
func TestInspectOlderCLIHasPartialDataAndSanitizedError(t *testing.T) {
	for _, engine := range []Engine{Codex, Claude} {
		t.Run(string(engine), func(t *testing.T) {
			o := telemetryFixtureOptions(t, engine)
			t.Setenv("LIB_HARNESS_TELEMETRY_MODE", "unsupported")
			got, err := Inspect(testContext(t), o)
			if !errors.Is(err, ErrUnsupported) || !got.Account.Known() || got.Quota.Known() || got.Capabilities.Quota.Availability != Unsupported {
				t.Fatalf("partial response lost: %+v %v", got, err)
			}
			raw, _ := json.Marshal(got)
			if strings.Contains(string(raw)+err.Error(), "must-not-escape") {
				t.Fatal("native diagnostic exposed")
			}
		})
	}
}
func TestInspectCancellationTerminatesCLI(t *testing.T) {
	for _, engine := range []Engine{Codex, Claude} {
		t.Run(string(engine), func(t *testing.T) {
			o := telemetryFixtureOptions(t, engine)
			t.Setenv("LIB_HARNESS_TELEMETRY_MODE", "hang")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := Inspect(ctx, o); done <- err }()
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err := os.Stat(filepath.Join(o.Home, "waiting")); err == nil {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("inspection returned before live child: %v", err)
				case <-deadline.C:
					t.Fatal("child did not reach blocked read")
				case <-ticker.C:
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled inspection: %v", err)
				}
			case <-deadline.C:
				t.Fatal("inspection failed to reap cancelled child")
			}
		})
	}
}
