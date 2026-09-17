package completion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireDiagnostic(t *testing.T, err error, engine string, phase ErrorPhase, code string) *RequestError {
	t.Helper()
	var failure *RequestError
	if !errors.As(err, &failure) || failure.Engine != engine || failure.Phase != phase || failure.Code != code {
		t.Fatalf("want %s/%s/%s; got %#v (%v)", engine, phase, code, failure, err)
	}
	if failure.Retryable() {
		t.Fatal("local setup failure permits a provider retry")
	}
	encoded, _ := json.Marshal(failure)
	if strings.Contains(string(encoded)+fmt.Sprintf("%#v", failure)+err.Error(), "secret") {
		t.Fatalf("diagnostic retained unsafe data: %v", err)
	}
	if failure.diagnosticDetail() == "" {
		t.Fatalf("missing actionable detail for %s", code)
	}
	return failure
}

func TestCompletePreflightDiagnostics(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, engine := range []string{"claude", "codex"} {
		for _, scenario := range []struct{ name, code string }{
			{"missing model", "model_required"},
			{"limits", "invalid_limits"},
			{"home", engine + "_home_invalid"},
			{"scratch", "scratch_directory"},
			{"tool catalog", "invalid_tool_catalog"},
			{"probe startup", "probe_no_requests"},
			{"probe timeout", "probe_timeout"},
			{"missing executable", "executable_not_found"},
		} {
			t.Run(engine+"/"+scenario.name, func(t *testing.T) {
				cfg := Config{Engine: engine, Model: "test-model", Effort: "high", CodexHome: t.TempDir(), ClaudeHome: t.TempDir(), CodexBin: binary, ClaudeBin: binary}
				cfg.BeforeRequest = func(context.Context) error { t.Fatal("preflight failure reached request hook"); return nil }
				cfg.run = func(_ context.Context, _ string, args []string, _ string, _ []string, _ string) ([]byte, error) {
					if len(args) > 0 && args[0] == "debug" {
						return []byte(testCatalog), nil
					}
					if scenario.name == "probe timeout" {
						return nil, fmt.Errorf("secret: %w", context.DeadlineExceeded)
					}
					return []byte("secret provider output"), errors.New("secret startup error")
				}
				var tools []Tool
				switch scenario.name {
				case "missing model":
					cfg.Model = ""
				case "limits":
					cfg.MaxContextBytes = -1
				case "home":
					cfg.ClaudeHome = "secret-relative"
					cfg.CodexHome = "secret-relative"
				case "scratch":
					cfg.WorkDirRoot = filepath.Join(t.TempDir(), "secret-missing")
				case "tool catalog":
					tools = []Tool{{Type: "secret-invalid"}}
				case "missing executable":
					cfg.ClaudeBin = filepath.Join(t.TempDir(), "secret-missing")
					cfg.CodexBin = cfg.ClaudeBin
					cfg.run = nil
				}
				_, _, err := Complete(context.Background(), cfg, []Message{{Role: "user", Content: "secret prompt"}}, tools)
				requireDiagnostic(t, err, engine, PhasePreflight, scenario.code)
				if scenario.name == "probe timeout" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("lost deadline identity")
				}
			})
		}
	}
	_, _, err = Complete(context.Background(), Config{Engine: "secret-engine"}, nil, nil)
	requireDiagnostic(t, err, "", PhasePreflight, "unsupported_engine")
}

func TestCodexCatalogDiagnostics(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, catalog, effort, code string
		err                         error
	}{
		{name: "read failure", err: errors.New("secret"), code: "catalog_read_failed"},
		{name: "timeout", err: context.DeadlineExceeded, code: "catalog_timeout"},
		{name: "invalid JSON", catalog: "secret", code: "invalid_model_catalog"},
		{name: "missing effort catalog", catalog: `{"models":[{"slug":"test-model"}]}`, code: "missing_effort_catalog"},
		{name: "unsupported effort", catalog: testCatalog, effort: "secret", code: "unsupported_effort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Engine: "codex", CodexBin: binary, CodexHome: t.TempDir(), Model: "test-model", Effort: tc.effort}
			cfg.run = func(context.Context, string, []string, string, []string, string) ([]byte, error) {
				return []byte(tc.catalog), tc.err
			}
			_, _, err := Complete(context.Background(), cfg, nil, nil)
			requireDiagnostic(t, err, "codex", PhasePreflight, tc.code)
		})
	}
}

func TestStartDiagnosticsDiscardOSPaths(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{&exec.Error{Name: "secret", Err: exec.ErrNotFound}, "executable_not_found"},
		{&os.PathError{Op: "chdir", Path: "secret", Err: os.ErrNotExist}, "working_directory_unavailable"},
		{&os.PathError{Op: "chdir", Path: "secret", Err: os.ErrPermission}, "working_directory_unavailable"},
		{&os.PathError{Op: "fork/exec", Path: "secret", Err: os.ErrPermission}, "executable_permission"},
		{&exec.Error{Name: "secret", Err: exec.ErrDot}, "executable_relative"},
		{&os.PathError{Op: "fork/exec", Path: "secret", Err: errors.New("secret")}, "process_start_failed"},
	} {
		for _, engine := range []string{"claude", "codex"} {
			requireDiagnostic(t, processRequestFailure(engine, []byte("secret"), tc.err), engine, PhaseProcess, tc.code)
		}
	}
}

func TestProbeMismatchDiagnostics(t *testing.T) {
	for reason, code := range map[string]string{
		"invalid or oversized request":       "probe_invalid_request",
		"unexpected tools":                   "probe_unexpected_tools",
		"invalid output schema":              "probe_invalid_schema",
		"changed output schema":              "probe_changed_schema",
		"changed reasoning effort":           "probe_changed_effort",
		"unexpected system instruction type": "probe_instruction_type",
		"unexpected system instructions":     "probe_unexpected_instructions",
		"missing application instructions":   "probe_missing_instructions",
		"secret future reason":               "probe_mismatch",
	} {
		requireDiagnostic(t, preflightFailure("claude", claudeProbeCode(reason)), "claude", PhasePreflight, code)
	}
	for _, tc := range []struct{ body, code string }{
		{"secret", "probe_invalid_request"},
		{`{"model":"secret"}`, "probe_changed_model"},
		{`{"model":"test","reasoning":{"effort":"secret"}}`, "probe_changed_effort"},
		{`{"model":"test","tools":["secret"]}`, "probe_unexpected_tools"},
		{`{"model":"test","instructions":"# AGENTS.md instructions secret"}`, "probe_unexpected_instructions"},
	} {
		requireDiagnostic(t, preflightFailure("codex", codexProbeMismatch([]byte(tc.body), "test", "")), "codex", PhasePreflight, tc.code)
	}
}

func TestCompletionPreservesCallerBeforeRequestError(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, engine := range []string{"claude", "codex"} {
		t.Run(engine, func(t *testing.T) {
			sentinel := errors.New("caller reservation denied")
			hookErr := fmt.Errorf("caller context: %w", sentinel)
			cfg := Config{Engine: engine, Model: "test-model", Effort: "high", CodexBin: binary, ClaudeBin: binary, CodexHome: t.TempDir(), BeforeRequest: func(context.Context) error { return hookErr }}
			cfg.run = func(_ context.Context, _ string, args []string, _ string, env []string, _ string) ([]byte, error) {
				if args[0] == "debug" {
					return []byte(testCatalog), nil
				}
				if url := environmentValue(env, "ANTHROPIC_BASE_URL"); url != "" {
					if err := answerClaudeProbe(args, url); err != nil {
						t.Fatal(err)
					}
					return nil, errors.New("local rejection")
				}
				if url := findProbeURL(args); url != "" {
					response, err := http.Post(url, "application/json", strings.NewReader(`{"model":"test-model","reasoning":{"effort":"high"}}`))
					if err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
					return nil, errors.New("local rejection")
				}
				t.Fatal("inference ran after rejected hook")
				return nil, nil
			}
			_, _, err := Complete(context.Background(), cfg, nil, nil)
			if err != hookErr || !errors.Is(err, sentinel) {
				t.Fatalf("caller hook error replaced: %v", err)
			}
		})
	}
}
