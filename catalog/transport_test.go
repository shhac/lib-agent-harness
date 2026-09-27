package catalog

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
)

// Provider variables are seeded with obvious synthetic values so that a
// regression which forwarded them would record the synthetic string, never a
// real ambient key. No native auth file is read.
func seedSyntheticProviderKeys(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "synthetic-openai-key-not-a-credential")
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-anthropic-key-not-a-credential")
	t.Setenv("XAI_API_KEY", "synthetic-xai-key-not-a-credential")
}

func cliProvider(engine harness.Engine, binary, home string) harness.Provider {
	return harness.Provider{Engine: engine, CLI: harness.CLI{Binary: binary, Home: home}}
}

func testBinary(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

func readInvocation(t *testing.T, home string) fixtureReport {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "invocation.json"))
	if err != nil {
		t.Fatalf("synthetic CLI recorded no invocation: %v", err)
	}
	var got fixtureReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Each engine must be launched with its own catalog invocation and its own
// configured login home, through the real subprocess transport.
func TestTransportInvokesEachEngineWithItsOwnInvocation(t *testing.T) {
	binary := testBinary(t)
	for _, tc := range []struct {
		engine    harness.Engine
		homeKey   string
		wantModel string
		wantArgs  []string
		absent    []string
		wantEnv   map[string]string
	}{
		{harness.Codex, "CODEX_HOME", "fixture-codex-model",
			[]string{"app-server", "--listen", "stdio://", "-c", "analytics.enabled=false"},
			[]string{"--input-format", "--safe-mode", "--no-leader"}, nil},
		{harness.Claude, "CLAUDE_CONFIG_DIR", "fixture-claude-model",
			[]string{"--safe-mode", "-p", "--input-format", "stream-json", "--output-format", "--verbose"},
			[]string{"app-server", "--listen", "--no-leader"}, nil},
		{harness.Grok, "GROK_HOME", "grok-4.7",
			[]string{"agent", "--no-leader", "stdio"},
			[]string{"app-server", "--safe-mode", "--input-format"},
			map[string]string{"GROK_TELEMETRY_ENABLED": "0", "GROK_CLAUDE_MCPS_ENABLED": "0"}},
	} {
		t.Run(string(tc.engine), func(t *testing.T) {
			seedSyntheticProviderKeys(t)
			home := t.TempDir()
			started := time.Now()
			models, err := Discover(context.Background(), cliProvider(tc.engine, binary, home))
			if err != nil {
				t.Fatalf("discovery failed: %v", err)
			}
			if len(models) != 1 || models[0].ID != tc.wantModel {
				t.Fatalf("unexpected catalog: %+v", models)
			}
			if time.Since(started) > 10*time.Second {
				t.Fatal("discovery waited for the child instead of stopping it")
			}
			got := readInvocation(t, home)
			for _, want := range tc.wantArgs {
				if !slices.Contains(got.Args, want) {
					t.Fatalf("%s invocation missing %q: %v", tc.engine, want, got.Args)
				}
			}
			for _, banned := range tc.absent {
				if slices.Contains(got.Args, banned) {
					t.Fatalf("%s invocation carried another engine's %q: %v", tc.engine, banned, got.Args)
				}
			}
			if got.Env[tc.homeKey] != home {
				t.Fatalf("%s did not receive its configured home: %+v", tc.engine, got.Env)
			}
			for key, value := range tc.wantEnv {
				if got.Env[key] != value {
					t.Fatalf("%s missing %s=%s: %+v", tc.engine, key, value, got.Env)
				}
			}
			for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY"} {
				if _, leaked := got.Env[key]; leaked {
					t.Fatalf("%s reached the catalog subprocess", key)
				}
			}
		})
	}
}

// Discovery must not hand one engine another's environment, even a home set
// in the parent: a Provider can no longer name the other engine's home, so the
// ambient one is the remaining route.
func TestTransportDoesNotCrossEngineEnvironments(t *testing.T) {
	seedSyntheticProviderKeys(t)
	binary := testBinary(t)
	claudeHome, codexHome, grokHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("GROK_HOME", grokHome)
	if _, err := Discover(context.Background(), cliProvider(harness.Claude, binary, claudeHome)); err != nil {
		t.Fatal(err)
	}
	got := readInvocation(t, claudeHome)
	for _, key := range []string{"CODEX_HOME", "GROK_HOME", "GROK_TELEMETRY_ENABLED"} {
		if _, ok := got.Env[key]; ok {
			t.Fatalf("Claude discovery inherited %s: %+v", key, got.Env)
		}
	}
	for _, home := range []string{codexHome, grokHome} {
		if _, err := os.Stat(filepath.Join(home, "invocation.json")); err == nil {
			t.Fatal("Claude discovery launched another engine's catalog subprocess")
		}
	}
}

// An empty Grok home keeps an ambient GROK_HOME, as Codex keeps CODEX_HOME.
func TestGrokEmptyHomeKeepsTheAmbientHome(t *testing.T) {
	ambient := t.TempDir()
	t.Setenv("GROK_HOME", ambient)
	if _, err := Discover(context.Background(), cliProvider(harness.Grok, testBinary(t), "")); err != nil {
		t.Fatal(err)
	}
	if got := readInvocation(t, ambient); got.Env["GROK_HOME"] != ambient {
		t.Fatalf("%+v", got.Env)
	}
}

// Cancellation during the exchange must unblock the pipes and reap the child,
// for every engine. runCLI only returns after its `<-done`, which waits on the
// child's own Run, so returning at all is the proof that the child terminated;
// the process package's job tests cover descendant containment.
func TestTransportCancellationUnblocksPipesAndReapsChild(t *testing.T) {
	binary := testBinary(t)
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok} {
		t.Run(string(engine), func(t *testing.T) {
			seedSyntheticProviderKeys(t)
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, hangMarker), nil, 0600); err != nil {
				t.Fatal(err)
			}
			command, _, err := cliCatalog(cliProvider(engine, binary, home))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel) // Installed before any wait, so a failure cannot hang.
			live := make(chan struct{})
			returned := make(chan error, 1)
			go func() {
				returned <- runCLI(ctx, command, func(reader io.Reader, _ io.Writer) error {
					// The hang fixture announces itself once and then answers
					// nothing, so the first read completing proves a live child
					// and the second can only be released by cancellation.
					if _, err := reader.Read(make([]byte, 1)); err != nil {
						return err
					}
					close(live)
					_, err := reader.Read(make([]byte, 1))
					return err
				})
			}()
			deadline := time.After(30 * time.Second)
			select {
			case <-live:
			case err := <-returned:
				t.Fatalf("transport returned before the child was live: %v", err)
			case <-deadline:
				t.Fatal("synthetic CLI never announced itself")
			}
			cancel()
			select {
			case err := <-returned:
				if err == nil {
					t.Fatal("cancelled transport reported success")
				}
			case <-deadline:
				t.Fatal("cancellation did not unblock the transport")
			}
		})
	}
}

// A hung CLI is stopped by discovery's own bound, reported as a typed timeout.
func TestTransportTimeoutIsTyped(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, hangMarker), nil, 0600); err != nil {
		t.Fatal(err)
	}
	d := discoverer{timeout: 500 * time.Millisecond, run: runCLI}
	_, err := d.discover(context.Background(), cliProvider(harness.Grok, testBinary(t), home))
	requireFailure(t, err, harness.Grok, harness.FailureProcess, "deadline_exceeded")
	if facts, _ := harness.ErrorFacts(err); facts.Cause != harness.CauseTimeout {
		t.Fatalf("%+v", facts)
	}
}
