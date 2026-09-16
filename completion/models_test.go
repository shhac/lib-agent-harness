package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

const modelCatalogFixture = `{"id":1,"result":{}}
{"method":"notification"}
{"id":2,"result":{"data":[{"model":"test-thinker","displayName":"Test Thinker","description":"Thorough","defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"low","description":"Quick"},{"reasoningEffort":"high","description":"Thorough"}],"isDefault":true},{"model":"hidden","hidden":true}],"nextCursor":"second"}}
{"id":3,"result":{"data":[{"model":"test-builder","displayName":"Test Builder","defaultReasoningEffort":"low","supportedReasoningEfforts":[{"reasoningEffort":"low"}]}],"nextCursor":null}}
`

func TestModelCatalogProtocolAndPagination(t *testing.T) {
	var requests bytes.Buffer
	models, err := readModelCatalog(strings.NewReader(modelCatalogFixture), &requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Name != "Test Thinker" || !models[0].IsDefault || models[0].DefaultEffort != "high" || len(models[0].Efforts) != 2 || models[0].Efforts[0].Description != "Quick" {
		t.Fatalf("bad model mapping: %+v", models)
	}
	decoder := json.NewDecoder(&requests)
	methods := []string{"initialize", "initialized", "model/list", "model/list"}
	for i, method := range methods {
		var message struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := decoder.Decode(&message); err != nil {
			t.Fatal(err)
		}
		if message.Method != method {
			t.Fatalf("request %d: %+v", i, message)
		}
		if i == 1 && message.ID != 0 {
			t.Fatal("initialized should be a notification")
		}
		if i == 2 && (message.Params["includeHidden"] != false || message.Params["limit"] != float64(100)) {
			t.Fatal(message.Params)
		}
		if i == 3 && message.Params["cursor"] != "second" {
			t.Fatal(message.Params)
		}
	}
	if decoder.More() {
		t.Fatal("unexpected additional request")
	}
}

func TestModelDiscoveryPreservesProfileAndSanitizesErrors(t *testing.T) {
	cfg := Config{Engine: "codex", CodexHome: "/test/chosen-login", CodexBin: "chosen-codex"}
	called := false
	_, err := discoverModels(context.Background(), cfg, func(ctx context.Context, actual Config, exchange func(io.Reader, io.Writer) error) error {
		called = true
		if actual.CodexHome != cfg.CodexHome || actual.CodexBin != cfg.CodexBin {
			t.Fatal(actual)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Second {
			t.Fatal("missing discovery deadline")
		}
		return errors.New("secret-token from subprocess")
	})
	if !called || err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatal(err)
	}
	cfg.Engine = "openai-compatible"
	called = false
	_, err = discoverModels(context.Background(), cfg, func(context.Context, Config, func(io.Reader, io.Writer) error) error { called = true; return nil })
	if called || err == nil {
		t.Fatal("unsupported engine invoked Codex")
	}
}

func TestModelCatalogFailsClosed(t *testing.T) {
	cases := []string{
		`{"id":1,"error":{"message":"secret"}}`,
		"{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":{}}",
		"{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":{\"data\":[],\"nextCursor\":\"x\"}}\n{\"id\":3,\"result\":{\"data\":[],\"nextCursor\":\"x\"}}",
		strings.Repeat("x", 1<<20+1),
	}
	for _, input := range cases {
		models, err := readModelCatalog(strings.NewReader(input), io.Discard)
		if err == nil || models != nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid catalog accepted or raw diagnostic leaked")
		}
	}
}
