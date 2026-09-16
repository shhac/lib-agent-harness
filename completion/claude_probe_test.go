package completion

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClaudeProbeAllowsOnlyBoundedSafeCompatibilityRequests(t *testing.T) {
	for _, tc := range []struct {
		name       string
		count      int
		unsafeLast bool
		want       string
	}{
		{"single", 1, false, ""},
		{"compatibility fallback", 2, false, ""},
		{"unsafe fallback", 2, true, "unexpected tools"},
		{"request limit", 5, false, "request limit"},
		{"startup failure", 0, false, "made no request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := []byte(`{"type":"object","properties":{"content":{"type":"string"}}}`)
			cfg := Config{Effort: "high", Timeout: time.Second}
			cfg.run = func(_ context.Context, _ string, args []string, _ string, env []string, _ string) ([]byte, error) {
				base := ""
				for _, e := range env {
					if strings.HasPrefix(e, "ANTHROPIC_BASE_URL=") {
						base = strings.TrimPrefix(e, "ANTHROPIC_BASE_URL=")
					}
				}
				res, err := http.Head(base + "/api/hello")
				if err != nil {
					return nil, err
				}
				res.Body.Close()
				for i := 0; i < tc.count; i++ {
					name := "StructuredOutput"
					if tc.unsafeLast && i == tc.count-1 {
						name = "Bash"
					}
					body, _ := json.Marshal(map[string]any{
						"tools":         []any{map[string]any{"name": name, "input_schema": json.RawMessage(schema)}},
						"system":        []any{map[string]string{"type": "text", "text": codexInstructions}},
						"output_config": map[string]string{"effort": "high"},
					})
					res, err := http.Post(base, "application/json", strings.NewReader(string(body)))
					if err != nil {
						return nil, err
					}
					res.Body.Close()
				}
				return nil, nil
			}
			err := probeClaude(context.Background(), cfg, "fixture", nil, t.TempDir(), nil, schema)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("want %q; got %v", tc.want, err)
			}
		})
	}
}
