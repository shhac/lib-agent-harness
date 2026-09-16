package completion

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Provider variables are seeded with obvious synthetic values so that a
// regression which forwarded them would record the synthetic string, never a
// real ambient key. No native auth file is read.
const (
	syntheticOpenAIKey    = "synthetic-openai-key-not-a-credential"
	syntheticAnthropicKey = "synthetic-anthropic-key-not-a-credential"
)

func seedSyntheticProviderKeys(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", syntheticOpenAIKey)
	t.Setenv("ANTHROPIC_API_KEY", syntheticAnthropicKey)
}

func readCatalogInvocation(t *testing.T, home string) catalogFixtureReport {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "invocation.json"))
	if err != nil {
		t.Fatalf("synthetic CLI recorded no invocation: %v", err)
	}
	var got catalogFixtureReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Each engine must be launched with its own catalog invocation and its own
// configured login home, through the real subprocess transport.
func TestModelTransportInvokesEachEngineWithItsOwnInvocation(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		engine    string
		homeKey   string
		wantModel string
		wantArgs  []string
		absent    []string
	}{
		{"codex", "CODEX_HOME", "fixture-codex-model",
			[]string{"app-server", "--listen", "stdio://", "-c", "analytics.enabled=false"},
			[]string{"--input-format", "--safe-mode"}},
		{"claude", "CLAUDE_CONFIG_DIR", "fixture-claude-model",
			[]string{"--safe-mode", "-p", "--input-format", "stream-json", "--output-format", "--verbose"},
			[]string{"app-server", "--listen"}},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			seedSyntheticProviderKeys(t)
			home := t.TempDir()
			cfg := Config{Engine: tc.engine, CodexBin: binary, ClaudeBin: binary, CodexHome: home, ClaudeHome: home}
			models, err := DiscoverModels(context.Background(), cfg)
			if err != nil {
				t.Fatalf("discovery failed: %v", err)
			}
			if len(models) != 1 || models[0].ID != tc.wantModel {
				t.Fatalf("unexpected catalog: %+v", models)
			}
			got := readCatalogInvocation(t, home)
			for _, want := range tc.wantArgs {
				if !slices.Contains(got.Args, want) {
					t.Fatalf("%s invocation missing %q: %v", tc.engine, want, got.Args)
				}
			}
			for _, banned := range tc.absent {
				if slices.Contains(got.Args, banned) {
					t.Fatalf("%s invocation carried the other engine's %q: %v", tc.engine, banned, got.Args)
				}
			}
			if got.Env[tc.homeKey] != home {
				t.Fatalf("%s did not receive its configured home: %+v", tc.engine, got.Env)
			}
			for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
				if _, leaked := got.Env[key]; leaked {
					t.Fatalf("%s reached the catalog subprocess", key)
				}
			}
		})
	}
}

// Discovery must not hand one engine the other's environment. Claude discovery
// in particular must not carry CODEX_HOME.
func TestModelTransportDoesNotCrossEngineEnvironments(t *testing.T) {
	seedSyntheticProviderKeys(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	claudeHome, codexHome := t.TempDir(), t.TempDir()
	cfg := Config{Engine: "claude", ClaudeBin: binary, CodexBin: binary, ClaudeHome: claudeHome, CodexHome: codexHome}
	if _, err := DiscoverModels(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	got := readCatalogInvocation(t, claudeHome)
	if _, ok := got.Env["CODEX_HOME"]; ok {
		t.Fatalf("Claude discovery inherited the Codex home: %+v", got.Env)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "invocation.json")); err == nil {
		t.Fatal("Claude discovery launched a Codex catalog subprocess")
	}
}

// Cancellation during the exchange must unblock the pipes and reap the child,
// for either engine. runModelTransport only returns after its `<-done`, which
// waits on the child's own Run, so returning at all is the proof that the child
// terminated; the process package's job tests cover descendant containment.
func TestModelTransportCancellationUnblocksPipesAndReapsChild(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		engine string
		cfg    func(home string) Config
	}{
		{"codex", func(home string) Config {
			return Config{Engine: "codex", CodexBin: binary, CodexHome: home}
		}},
		{"claude", func(home string) Config {
			return Config{Engine: "claude", ClaudeBin: binary, ClaudeHome: home}
		}},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			seedSyntheticProviderKeys(t)
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, catalogHangMarker), nil, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel) // Installed before any wait, so a failure cannot hang.
			live := make(chan struct{})
			returned := make(chan error, 1)
			go func() {
				returned <- runModelTransport(ctx, tc.cfg(home), func(reader io.Reader, _ io.Writer) error {
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
			if _, err := os.Stat(filepath.Join(home, "invocation.json")); err != nil {
				t.Fatalf("synthetic CLI never started: %v", err)
			}
		})
	}
}
