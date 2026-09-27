package completion

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness"
)

// Cancellation can arrive during any preparatory subprocess. It must retain its
// identity and never consume a reservation until the actual inference boundary.
func TestCompletionCancellationAcrossSubprocessBoundaries(t *testing.T) {
	t.Setenv("USER", "synthetic-native-login-owner")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, engine := range cliEngines {
		for _, stage := range []string{"catalog", "probe_failure", "probe_verified", "inference"} {
			if engine == harness.Claude && stage == "catalog" {
				continue
			}
			t.Run(string(engine)+"/"+stage, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reservations, invocations := 0, 0
				home := ""
				if engine == harness.Codex {
					home = t.TempDir()
				}
				cfg := Config{Provider: cliProvider(engine, binary, home), Model: "test-model", Effort: "high", BeforeRequest: func(context.Context) error { reservations++; return nil }}
				run := func(_ context.Context, _ string, args []string, _ string, env []string, _ string) ([]byte, error) {
					if args[0] == "debug" {
						if stage == "catalog" {
							cancel()
							return nil, errors.New("synthetic subprocess failure")
						}
						return []byte(testCatalog), nil
					}
					probeURL := findProbeURL(args)
					claudeURL := environmentValue(env, "ANTHROPIC_BASE_URL")
					if probeURL != "" || claudeURL != "" {
						if stage == "probe_failure" {
							cancel()
							return nil, errors.New("synthetic probe failure")
						}
						if claudeURL != "" {
							if environmentValue(env, "USER") != "" {
								t.Fatal("dummy probe inherited keychain user")
							}
							if err := answerClaudeProbe(args, claudeURL); err != nil {
								t.Fatal(err)
							}
						} else {
							response, err := http.Post(probeURL, "application/json", strings.NewReader(`{"model":"test-model","reasoning":{"effort":"high"}}`))
							if err != nil {
								t.Fatal(err)
							}
							response.Body.Close()
						}
						if stage == "probe_verified" {
							cancel()
						}
						return nil, errors.New("probe deliberately rejects")
					}
					invocations++
					cancel()
					return nil, errors.New("synthetic inference failure")
				}
				cfg.run = run
				_, err := Complete(ctx, cfg, nil, Tools())
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
				want := 0
				if stage == "inference" {
					want = 1
				}
				if reservations != want || invocations != want {
					t.Fatalf("reservations=%d inference=%d want=%d", reservations, invocations, want)
				}
			})
		}
	}
}

func TestDiscoveryPreservesCancellation(t *testing.T) {
	for _, engine := range cliEngines {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := discoverModels(ctx, Config{Provider: harness.Provider{Engine: engine}}, func(context.Context, Config, func(io.Reader, io.Writer) error) error {
			cancel()
			return errors.New("synthetic transport error")
		})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s lost cancellation: %v", engine, err)
		}
	}
}

func TestActionArgumentsMustBeObject(t *testing.T) {
	for _, arguments := range []string{"null", "[]", "5", `"text"`, "true", "{}", "{\"key\":null}"} {
		data, _ := json.Marshal(map[string]any{"content": "", "tool_calls": []any{map[string]string{"name": "read_state", "arguments": arguments}}})
		_, err := parseActionEnvelope(data, Tools())
		wantOK := strings.HasPrefix(arguments, "{")
		if (err == nil) != wantOK {
			t.Fatalf("arguments %s: err=%v", arguments, err)
		}
	}
}

func TestCodexKeepsLastCompletedMessage(t *testing.T) {
	data := `{"type":"item.completed","item":{"type":"agent_message","text":"Checking the request."}}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"content\":\"Final answer\",\"tool_calls\":[]}"}}` + "\n" +
		`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":3}}`
	result, err := parseCodex([]byte(data), Tools())
	if err != nil || result.Message.Content != "Final answer" {
		t.Fatalf("message=%+v err=%v", result.Message, err)
	}
}
