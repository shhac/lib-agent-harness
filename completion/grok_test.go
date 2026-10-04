//go:build !windows

package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// grokFake stands in for the installed CLI. Its probe sends the request a
// Grok 1.0.41 launch would send to the configured provider, and its real run
// prints a stream; every credential is a synthetic string.
type grokFake struct {
	t                          *testing.T
	versions, probes, launches int
	// tamper edits the probe's model request before it is sent.
	tamper func(map[string]any)
	// stream is the real run's output; nil prints a successful proposal.
	stream []string
	// transcript edits the real run's persisted transcript.
	transcript func([]byte) []byte
	// refresh, when set, is written as the runtime login during the real run.
	refresh string
	// seen records the real run's arguments, environment and prompt.
	args   []string
	env    []string
	prompt string
	// order records probe, before-request and launch.
	order []string
}

func flagValues(args []string) map[string]string {
	values := map[string]string{}
	for _, arg := range args {
		name, value, _ := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		values[name] = value
	}
	return values
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, key+"="); ok {
			return value
		}
	}
	return ""
}

var probeURL = regexp.MustCompile(`base_url = "([^"]+)"`)

func (f *grokFake) run(_ context.Context, _ string, args []string, dir string, env []string, input string) ([]byte, error) {
	t := f.t
	if input != "" {
		t.Error("Grok reads its prompt from a file, never stdin")
	}
	if len(args) == 1 && args[0] == "--version" {
		f.versions++
		return []byte("grok 1.0.41 (synthetic) [stable]\n"), nil
	}
	flags := flagValues(args)
	if flags["cwd"] != dir {
		t.Errorf("working directory %q vs --cwd %q", dir, flags["cwd"])
	}
	prompt, err := os.ReadFile(flags["prompt-file"])
	if err != nil {
		t.Fatal(err)
	}
	grokHome := envValue(env, "GROK_HOME")
	config, err := os.ReadFile(filepath.Join(grokHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if match := probeURL.FindSubmatch(config); match != nil {
		f.probes++
		f.order = append(f.order, "probe")
		return f.probe(string(match[1]), flags, string(prompt), grokHome, dir)
	}
	f.launches++
	f.order = append(f.order, "launch")
	f.args, f.env, f.prompt = args, env, string(prompt)
	if f.refresh != "" {
		if err := os.WriteFile(filepath.Join(grokHome, grokCredentialFile), []byte(f.refresh), 0600); err != nil {
			t.Fatal(err)
		}
	}
	transcript := grokTranscript(t, dir, string(prompt), map[string]any{"type": "assistant", "content": "{}"})
	if f.transcript != nil {
		transcript = f.transcript(transcript)
	}
	writeGrokSession(t, grokHome, dir, transcript)
	stream := f.stream
	if stream == nil {
		stream = []string{grokCatalog, `{"type":"text","data":"{}"}`, grokEnd}
	}
	return []byte(strings.Join(stream, "\n") + "\n"), nil
}

func (f *grokFake) probe(base string, flags map[string]string, prompt, grokHome, work string) ([]byte, error) {
	t := f.t
	schema := json.RawMessage(flags["json-schema"])
	want := grokExpectation{model: grokProbeModel, effort: flags["reasoning-effort"], schema: schema, prompt: prompt, work: work}
	request := grokRequest(want)
	request["input"].([]any)[0].(map[string]any)["content"] = flags["system-prompt-override"]
	if f.tamper != nil {
		f.tamper(request)
	}
	for _, body := range [][]byte{[]byte(grokTitleBody), encode(t, request)} {
		response, err := http.Post(base+"/responses", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("the probe provider did not refuse inference: %d", response.StatusCode)
		}
	}
	writeGrokSession(t, grokHome, work, grokTranscript(t, work, prompt))
	return []byte(grokCatalog + "\n" + `{"type":"error","message":"Internal error: {\"http_status\": 400}"}` + "\n"), errors.New("exit status 1")
}

func writeGrokSession(t *testing.T, grokHome, work string, transcript []byte) {
	t.Helper()
	dir := filepath.Join(grokHome, "sessions", strings.ReplaceAll(url.PathEscape(work), "/", "%2F"), "01a0e3f5-session")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chat_history.jsonl"), transcript, 0600); err != nil {
		t.Fatal(err)
	}
}

type grokSetup struct {
	cfg    Config
	fake   *grokFake
	root   string
	source string
}

func newGrokSetup(t *testing.T) *grokSetup {
	t.Helper()
	grokProofs.Lock()
	grokProofs.evidence = map[string]grokEvidence{}
	grokProofs.Unlock()
	t.Setenv("OPENAI_API_KEY", "secret-ambient-key")
	t.Setenv("XAI_API_KEY", "secret-ambient-key")
	// Grok encodes the whole work path into one filename (255-byte limit).
	// Avoid t.TempDir's long test-name component, while staying under TMPDIR.
	shortRoot, err := os.MkdirTemp("", "g-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortRoot) })
	root, err := filepath.EvalSymlinks(shortRoot)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, grokCredentialFile), []byte(`{"token":"synthetic-login"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.toml"), []byte("[mcp_servers.owner]\ncommand = \"/bin/sh\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &grokSetup{root: root, source: source}
	s.fake = &grokFake{t: t}
	s.cfg = Config{Provider: cliProvider(harness.Grok, "grok-synthetic-not-on-path", source), Model: "grok-4.7", Effort: "high", WorkDirRoot: root, run: s.fake.run}
	s.cfg.BeforeRequest = func(context.Context) error { s.fake.order = append(s.fake.order, "before"); return nil }
	return s
}

func TestGrokCompleteProvesThenLaunchesInAPrivateRuntimeHome(t *testing.T) {
	testenv.RequireLoopback(t) // Constrained CLI preflight starts a local refusal provider.
	s := newGrokSetup(t)
	s.fake.refresh = `{"token":"synthetic-refreshed"}`
	result, err := Complete(context.Background(), s.cfg, userMessage, Tools())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.fake.order, ",") != "probe,before,launch" {
		t.Fatalf("order %v", s.fake.order)
	}
	if result.Message.Content != "done" || len(result.Message.ToolCalls) != 1 || result.Message.ToolCalls[0].Function.Name != "read_state" {
		t.Fatalf("message %+v", result.Message)
	}
	if !result.Usage.Known || result.Usage.Input != 15 || !result.Usage.CacheKnown || !result.Cost.Known {
		t.Fatalf("accounting %+v %+v", result.Usage, result.Cost)
	}

	flags := flagValues(s.fake.args)
	for name, want := range map[string]string{
		"output-format": "streaming-json", "model": "grok-4.7", "reasoning-effort": "high",
		"tools": "read_file", "disallowed-tools": "read_file,search_tool,use_tool",
		"system-prompt-override": codexInstructions,
	} {
		if flags[name] != want {
			t.Errorf("--%s=%q", name, flags[name])
		}
	}
	for _, name := range []string{"no-subagents", "verbatim", "disable-web-search", "json-schema", "prompt-file"} {
		if _, ok := flags[name]; !ok {
			t.Errorf("missing --%s", name)
		}
	}
	if _, ok := flags["single"]; ok {
		t.Error("the prompt was put on the command line")
	}
	for _, arg := range s.fake.args {
		if !strings.HasPrefix(arg, "--") {
			t.Errorf("unbound argument %q", arg)
		}
	}

	runtimeHome := filepath.Join(s.root, grokRuntimeHome)
	if envValue(s.fake.env, "GROK_HOME") != runtimeHome {
		t.Fatalf("GROK_HOME %q", envValue(s.fake.env, "GROK_HOME"))
	}
	home := envValue(s.fake.env, "HOME")
	if home == "" || !strings.HasPrefix(home, filepath.Join(s.root, "model-runs")) {
		t.Fatalf("HOME is not private: %q", home)
	}
	for _, entry := range s.fake.env {
		if strings.Contains(entry, "secret") || strings.HasPrefix(entry, "USER=") {
			t.Fatalf("parent environment leaked: %s", entry)
		}
	}
	for _, want := range append(append([]string{}, grokSwitches...), "GROK_MEMORY=0", "GROK_CODEX_RULES_ENABLED=0") {
		if envValue(s.fake.env, strings.Split(want, "=")[0]) != strings.Split(want, "=")[1] {
			t.Errorf("missing %s", want)
		}
	}
	config, _ := os.ReadFile(filepath.Join(runtimeHome, "config.toml"))
	skills, _ := grokSkillsConfig(runtimeHome)
	if string(config) != grokConfig+skills || !strings.Contains(skills, filepath.Join(runtimeHome, "bundled")) {
		t.Fatalf("runtime configuration %q", config)
	}
	if raw, _ := os.ReadFile(filepath.Join(s.source, grokCredentialFile)); string(raw) != `{"token":"synthetic-refreshed"}` {
		t.Fatalf("the refreshed login was not written back: %s", raw)
	}
	if entries, _ := os.ReadDir(filepath.Join(runtimeHome, "sessions")); len(entries) != 0 {
		t.Fatal("the run's persisted session was kept")
	}
	if entries, _ := os.ReadDir(filepath.Join(s.root, "model-runs")); len(entries) != 0 {
		t.Fatal("scratch was kept")
	}

	// The same binary and configuration reuse the proof; a changed schema does not.
	if _, err = Complete(context.Background(), s.cfg, userMessage, Tools()); err != nil {
		t.Fatal(err)
	}
	if s.fake.probes != 1 || s.fake.launches != 2 {
		t.Fatalf("probes %d launches %d", s.fake.probes, s.fake.launches)
	}
	s.fake.stream = []string{grokCatalog, `{"type":"end","stopReason":"end_turn","usage":{"input_tokens":1,"output_tokens":1},"structuredOutput":{"content":"x","tool_calls":[]}}`}
	if _, err = Complete(context.Background(), s.cfg, userMessage, nil); err != nil {
		t.Fatal(err)
	}
	if s.fake.probes != 2 {
		t.Fatal("a different tool catalog reused another configuration's proof")
	}
}

func TestGrokUnprovenProbeRefusesBeforeAnyCredentialedWork(t *testing.T) {
	testenv.RequireLoopback(t) // Constrained CLI preflight starts a local refusal provider.
	for _, tc := range []struct {
		name   string
		tamper func(map[string]any)
		code   string
	}{
		{"native tools", func(r map[string]any) { r["tools"] = []any{map[string]any{"type": "function", "name": "read_file"}} }, "probe_unexpected_tools"},
		{"extra reminder", func(r map[string]any) {
			r["input"] = append(r["input"].([]any), map[string]any{"type": "message", "role": "user", "content": "<system-reminder>workflows</system-reminder>"})
		}, "probe_unexpected_instructions"},
		{"changed schema", func(r map[string]any) {
			r["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "strict": true, "schema": map[string]any{}}}
		}, "probe_changed_schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newGrokSetup(t)
			s.fake.tamper = tc.tamper
			_, err := Complete(context.Background(), s.cfg, userMessage, Tools())
			failure := requireDiagnostic(t, err, harness.Grok, PhasePreflight, tc.code)
			if facts := failure.HarnessFacts(); facts.Family != harness.FailureCapability {
				t.Fatalf("%+v", facts)
			}
			if s.fake.launches != 0 || strings.Contains(strings.Join(s.fake.order, ","), "before") {
				t.Fatalf("credentialed work after an unproven probe: %v", s.fake.order)
			}
			// Nothing unproven is cached: the next call probes again.
			s.fake.tamper = nil
			if _, err = Complete(context.Background(), s.cfg, userMessage, Tools()); err != nil {
				t.Fatal(err)
			}
			if s.fake.probes != 2 {
				t.Fatalf("probes %d", s.fake.probes)
			}
		})
	}
}

func TestGrokRunIsJudgedByItsStreamAndTranscript(t *testing.T) {
	testenv.RequireLoopback(t) // Constrained CLI preflight starts a local refusal provider.
	for _, tc := range []struct {
		name       string
		stream     []string
		transcript func([]byte) []byte
		code       string
	}{
		{"native tool catalog", []string{`{"type":"available_commands","tools":["read_file"]}`, grokEnd}, nil, "unexpected_native_tool_catalog"},
		{"native tool call", []string{grokCatalog, `{"type":"tool_call","toolName":"run_terminal_cmd"}`, grokEnd}, nil, "unexpected_native_tool_call"},
		{"stopped early", []string{grokCatalog, `{"type":"end","stopReason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`}, nil, "max_tokens"},
		{"remote reminder", nil, func(raw []byte) []byte {
			return append(raw, []byte(`{"type":"user","content":[{"type":"text","text":"<system-reminder>nudge</system-reminder>"}]}`+"\n")...)
		}, "unexpected_native_instructions"},
		{"changed vendor rules", nil, func(raw []byte) []byte {
			return bytes.Replace(raw, []byte("State points directly."), []byte("Load AGENTS.md."), 1)
		}, "unexpected_native_instructions"},
		{"no transcript", nil, func([]byte) []byte { return nil }, "unexpected_native_instructions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newGrokSetup(t)
			s.fake.stream, s.fake.transcript = tc.stream, tc.transcript
			result, err := Complete(context.Background(), s.cfg, userMessage, Tools())
			failure := requireDiagnostic(t, err, harness.Grok, PhaseResponse, tc.code)
			if facts := failure.HarnessFacts(); facts.Family != harness.FailureRequest {
				t.Fatalf("a request may have started, so this is not a capability refusal: %+v", facts)
			}
			if result.Message.Content != "" || result.Message.ToolCalls != nil {
				t.Fatal("a refused run returned a proposal")
			}
			if entries, _ := os.ReadDir(filepath.Join(s.root, grokRuntimeHome, "sessions")); len(entries) != 0 {
				t.Fatal("a refused run's session was kept")
			}
		})
	}
}

func TestGrokPreflightRefusals(t *testing.T) {
	s := newGrokSetup(t)
	cfg := s.cfg
	cfg.WorkDirRoot = ""
	_, err := Complete(context.Background(), cfg, userMessage, Tools())
	requireDiagnostic(t, err, harness.Grok, PhasePreflight, "work_dir_root_required")

	cfg = s.cfg
	cfg.Provider.CLI.Home = t.TempDir()
	_, err = Complete(context.Background(), cfg, userMessage, Tools())
	failure := requireDiagnostic(t, err, harness.Grok, PhasePreflight, "grok_login_unavailable")
	if failure.Cause != harness.CauseAuthentication {
		t.Fatalf("%+v", failure)
	}

	cfg = s.cfg
	cfg.Provider.CLI.Home = filepath.Join(s.root, grokRuntimeHome)
	if err = os.MkdirAll(cfg.Provider.CLI.Home, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = Complete(context.Background(), cfg, userMessage, Tools())
	requireDiagnostic(t, err, harness.Grok, PhasePreflight, "grok_home_is_runtime")

	cfg = s.cfg
	cfg.MaxContextBytes = 1024
	_, err = Complete(context.Background(), cfg, []Message{{Role: "user", Content: strings.Repeat("x", 2048)}}, Tools())
	var tooLarge *RequestError
	if !errors.As(err, &tooLarge) || tooLarge.Engine != harness.Grok || tooLarge.Code != "context_bytes" || tooLarge.Cause != harness.CauseContextLimit {
		t.Fatalf("%v", err)
	}
	if s.fake.probes+s.fake.launches != 0 {
		t.Fatal("a refused configuration started Grok")
	}
}

func TestGrokRequestHookErrorStopsTheLaunch(t *testing.T) {
	testenv.RequireLoopback(t) // Constrained CLI preflight starts a local refusal provider.
	s := newGrokSetup(t)
	stop := errors.New("budget exhausted")
	s.cfg.BeforeRequest = func(context.Context) error { return stop }
	if _, err := Complete(context.Background(), s.cfg, userMessage, Tools()); !errors.Is(err, stop) {
		t.Fatalf("%v", err)
	}
	if s.fake.probes != 1 || s.fake.launches != 0 {
		t.Fatalf("probes %d launches %d", s.fake.probes, s.fake.launches)
	}
}

func TestGrokRunFailurePrefersTheStream(t *testing.T) {
	exit := exec.Command("sh", "-c", "exit 3").Run()
	stream := streamOf(grokCatalog, `{"type":"error","message":"Internal error: {\"http_status\": 503}"}`)
	var failure *RequestError
	if !errors.As(grokRunFailure(context.Background(), exit, stream), &failure) || failure.Code != "http_503" || failure.Cause != harness.CauseUnavailable || failure.ExitCode == nil || *failure.ExitCode != 3 {
		t.Fatalf("%#v", failure)
	}
	if !errors.As(grokRunFailure(context.Background(), exit, streamOf(grokCatalog)), &failure) || failure.Phase != PhaseProcess || failure.ExitCode == nil || *failure.ExitCode != 3 {
		t.Fatalf("%#v", failure)
	}
	if !errors.As(grokRunFailure(context.Background(), context.DeadlineExceeded, streamOf()), &failure) || failure.Cause != harness.CauseTimeout {
		t.Fatalf("%#v", failure)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(grokRunFailure(ctx, context.Canceled, streamOf()), context.Canceled) {
		t.Fatal("cancellation was not returned as such")
	}
}
