package completion

import (
	"context"
	"errors"
	"testing"
)

// The predicate is the fail-closed native-tool boundary: it decides, before any
// billable call, whether the CLI actually sent what was asked and nothing else.
func TestValidCodexProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"requested model, effort and no tools", `{"model":"test-model","reasoning":{"effort":"high"}}`, true},
		{"empty tools array is still tool-free", `{"model":"test-model","reasoning":{"effort":"high"},"tools":[]}`, true},
		{"unmodelled fields are ignored", `{"model":"test-model","reasoning":{"effort":"high"},"stream":true,"metadata":{"x":1}}`, true},
		{"malformed request", `{"model":"test-model"`, false},
		{"empty body", ``, false},
		{"substituted model", `{"model":"substitute","reasoning":{"effort":"high"}}`, false},
		{"substituted effort", `{"model":"test-model","reasoning":{"effort":"low"}}`, false},
		{"missing effort", `{"model":"test-model"}`, false},
		{"missing model", `{"reasoning":{"effort":"high"}}`, false},
		{"native tool leaked", `{"model":"test-model","reasoning":{"effort":"high"},"tools":[{"name":"shell"}]}`, false},
		{"global AGENTS instructions leaked", `{"model":"test-model","reasoning":{"effort":"high"},"instructions":"# AGENTS.md instructions\nalways do this"}`, false},
		{"AGENTS instructions leaked into a message", `{"model":"test-model","reasoning":{"effort":"high"},"input":[{"role":"user","content":"# AGENTS.md instructions"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validCodexProbe([]byte(tc.body), "test-model", "high"); got != tc.want {
				t.Fatalf("validCodexProbe(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// A CLI that reaches no provider at all proves nothing about its tools, so the
// probe must reject it rather than read silence as success.
func TestCodexProbeFailsClosedWithoutARequest(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	reserved := false
	cfg := Config{Engine: "codex", Model: "test-model", Effort: "high", CodexBin: "sh",
		BeforeRequest: func(context.Context) error { reserved = true; return nil }}
	inference := 0
	cfg.run = func(_ context.Context, _ string, args []string, _ string, _ []string, _ string) ([]byte, error) {
		if args[0] == "debug" {
			return []byte(testCatalog), nil
		}
		if findProbeURL(args) != "" {
			// Answer from cache: exit cleanly without contacting the provider.
			return nil, nil
		}
		inference++
		return nil, errors.New("unreachable")
	}
	if _, _, err := Complete(context.Background(), cfg, nil, Tools()); err == nil {
		t.Fatal("probe accepted a CLI that never contacted the provider")
	}
	if reserved || inference != 0 {
		t.Fatalf("billable work after an unproven probe: reserved=%v inference=%d", reserved, inference)
	}
}

// More than one outbound request is equally unproven: only the single verified
// request may satisfy the check.
func TestCodexProbeFailsClosedOnExtraRequests(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	reserved := false
	cfg := Config{Engine: "codex", Model: "test-model", Effort: "high", CodexBin: "sh",
		BeforeRequest: func(context.Context) error { reserved = true; return nil }}
	cfg.run = func(_ context.Context, _ string, args []string, _ string, _ []string, _ string) ([]byte, error) {
		if args[0] == "debug" {
			return []byte(testCatalog), nil
		}
		url := findProbeURL(args)
		if url == "" {
			t.Fatal("live execution after unproven probe")
		}
		for i := 0; i < 2; i++ {
			postProbe(t, url, `{"model":"test-model","reasoning":{"effort":"high"}}`)
		}
		return nil, errors.New("rejected")
	}
	if _, _, err := Complete(context.Background(), cfg, nil, Tools()); err == nil {
		t.Fatal("probe accepted a CLI that sent more than the verified request")
	}
	if reserved {
		t.Fatal("reserved a billable call after an unproven probe")
	}
}
