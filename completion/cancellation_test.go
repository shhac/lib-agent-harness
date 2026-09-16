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
)

// Cancellation can arrive during any preparatory subprocess. It must retain its
// identity and never consume a reservation until the actual inference boundary.
func TestCompletionCancellationAcrossSubprocessBoundaries(t *testing.T) {
	t.Setenv("USER", "synthetic-native-login-owner")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, engine := range []string{"codex", "claude"} {
		for _, stage := range []string{"catalog", "probe_failure", "probe_verified", "inference"} {
			if engine == "claude" && stage == "catalog" {
				continue
			}
			t.Run(engine+"/"+stage, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reservations, invocations := 0, 0
				cfg := Config{Engine: engine, Model: "test-model", Effort: "high", CodexBin: binary, ClaudeBin: binary, CodexHome: t.TempDir(), BeforeRequest: func(context.Context) error { reservations++; return nil }}
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
				_, _, err := Complete(ctx, cfg, nil, Tools())
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
	for _, engine := range []string{"codex", "claude"} {
		ctx, cancel := context.WithCancel(context.Background())
		_, err := discoverModels(ctx, Config{Engine: engine}, func(context.Context, Config, func(io.Reader, io.Writer) error) error {
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
	message, _, err := parseCodex([]byte(data), Tools())
	if err != nil || message.Content != "Final answer" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}
