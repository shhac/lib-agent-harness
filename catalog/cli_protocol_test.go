package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness"
)

const codexCatalogFixture = `{"id":1,"result":{}}
{"method":"notification"}
{"id":2,"result":{"data":[{"model":"test-thinker","displayName":"Test Thinker","description":"Thorough","defaultReasoningEffort":"high","supportedReasoningEfforts":[{"reasoningEffort":"low","description":"Quick"},{"reasoningEffort":"high","description":"Thorough"}],"isDefault":true},{"model":"hidden","hidden":true}],"nextCursor":"second"}}
{"id":3,"result":{"data":[{"model":"test-builder","defaultReasoningEffort":"low","supportedReasoningEfforts":[{"reasoningEffort":"low"}]},{"model":"test-silent"}],"nextCursor":null}}
`

func TestCodexCatalogProtocolAndPagination(t *testing.T) {
	var requests bytes.Buffer
	models, err := readCodexCatalog(strings.NewReader(codexCatalogFixture), &requests)
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{ID: "test-thinker", Name: "Test Thinker", Description: "Thorough", DefaultEffort: "high", IsDefault: true, EffortsKnown: true,
			Efforts: []Effort{{ID: "low", Description: "Quick"}, {ID: "high", Description: "Thorough", Default: true}}},
		{ID: "test-builder", Name: "test-builder", DefaultEffort: "low", EffortsKnown: true, Efforts: []Effort{{ID: "low", Default: true}}},
		{ID: "test-silent", Name: "test-silent"},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("bad model mapping:\n%+v\nwant\n%+v", models, want)
	}
	decoder := json.NewDecoder(&requests)
	for i, method := range []string{"initialize", "initialized", "model/list", "model/list"} {
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

func TestCodexCatalogFailsClosed(t *testing.T) {
	for code, input := range map[string]string{
		"request_failed":      `{"id":1,"error":{"message":"secret"}}`,
		"invalid_catalog":     "{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":{}}",
		"pagination_repeated": "{\"id\":1,\"result\":{}}\n{\"id\":2,\"result\":{\"data\":[],\"nextCursor\":\"x\"}}\n{\"id\":3,\"result\":{\"data\":[],\"nextCursor\":\"x\"}}",
		"output_limit":        strings.Repeat("x", lineLimit+1),
		"invalid_response":    "secret not json",
		"missing_response":    `{"id":1,"result":{}}`,
	} {
		_, err := testDiscoverer(fakeRunner(input, nil, nil)).discover(context.Background(), cliProvider(harness.Codex, testBinary(t), t.TempDir()))
		requireFailure(t, err, harness.Codex, harness.FailureRequest, code)
	}
	var pages strings.Builder
	pages.WriteString("{\"id\":1,\"result\":{}}\n")
	for page := range codexPageLimit {
		fmt.Fprintf(&pages, "{\"id\":%d,\"result\":{\"data\":[],\"nextCursor\":\"c%d\"}}\n", page+2, page)
	}
	if _, err := readCodexCatalog(strings.NewReader(pages.String()), io.Discard); failureCode(err) != "page_limit" {
		t.Fatalf("pagination unbounded: %v", err)
	}
}

func TestClaudeCatalogReadsOnlyInitializationMetadata(t *testing.T) {
	fixture := `{"type":"system","subtype":"init"}
{"type":"control_response","response":{"subtype":"success","request_id":"other","response":{}}}
{"type":"control_response","response":{"subtype":"success","request_id":"agent-harness-models","response":{"account":{"token":"not-for-ui"},"commands":["unrelated"],"models":[{"value":"default","displayName":"Default"},{"value":"opus","displayName":"Opus","resolvedModel":"version-is-cli-owned","description":"Careful reasoning","supportedEffortLevels":["low","high"],"defaultEffortLevel":"high"},{"value":"haiku","displayName":"Haiku","supportsEffort":false},{"value":"opus","displayName":"Duplicate"}]}}}`
	var requests bytes.Buffer
	models, err := readClaudeCatalog(strings.NewReader(fixture), &requests)
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{ID: "default", Name: "Default", IsDefault: true},
		{ID: "opus", Resolved: "version-is-cli-owned", Name: "Opus", Description: "Careful reasoning", DefaultEffort: "high", EffortsKnown: true, Efforts: []Effort{{ID: "low"}, {ID: "high", Default: true}}},
		{ID: "haiku", Name: "Haiku", EffortsKnown: true, Efforts: []Effort{}},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("%+v", models)
	}
	encoded, _ := json.Marshal(models)
	if strings.Contains(string(encoded), "not-for-ui") || strings.Contains(string(encoded), "unrelated") {
		t.Fatal("non-model metadata leaked")
	}
	if strings.Contains(requests.String(), `"type":"user"`) || !strings.Contains(requests.String(), `"subtype":"initialize"`) {
		t.Fatal(requests.String())
	}
}

func TestClaudeCatalogFailsClosed(t *testing.T) {
	for code, input := range map[string]string{
		"request_failed":   `{"type":"control_response","response":{"subtype":"error","request_id":"agent-harness-models","error":"secret"}}`,
		"invalid_catalog":  `{"type":"control_response","response":{"subtype":"success","request_id":"agent-harness-models","response":{}}}`,
		"invalid_response": "secret",
		"missing_response": `{"type":"system"}`,
		"output_limit":     strings.Repeat(`{"type":"system","padding":"`+strings.Repeat("x", 1<<19)+`"}`+"\n", 9),
	} {
		_, err := testDiscoverer(fakeRunner(input, nil, nil)).discover(context.Background(), cliProvider(harness.Claude, testBinary(t), t.TempDir()))
		requireFailure(t, err, harness.Claude, harness.FailureRequest, code)
	}
}

func TestGrokCatalogMapsTheVerifiedInitializeReply(t *testing.T) {
	var written strings.Builder
	var command invocation
	input := `{"jsonrpc":"2.0","method":"_x.ai/announcements/update","params":{"text":"secret"}}
{"jsonrpc":"2.0","id":7,"method":"client/request","params":{}}
` + grokFixture + "\n"
	home := t.TempDir()
	models, err := testDiscoverer(fakeRunner(input, &command, &written)).discover(context.Background(), cliProvider(harness.Grok, testBinary(t), home))
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{{
		ID: "grok-4.7", Name: "Grok 4.7", Description: "SpaceXAI's latest frontier model", DefaultEffort: "high",
		EffortsKnown: true, IsDefault: true, ContextWindow: 500000,
		Efforts: []Effort{
			{ID: "xhigh", Description: "Maximum reasoning for the hardest tasks."},
			{ID: "high", Description: "Thorough reasoning and quality. Recommended.", Default: true},
			{ID: "medium", Description: "Strong quality with a faster turnaround."},
			{ID: "low", Description: "Fastest responses. Best for simple tasks."},
		},
	}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("%+v", models)
	}
	// Exactly one request, and never session/new: it runs the user's hooks
	// and creates a session.
	if written.String() != grokInitialize {
		t.Fatalf("requests: %q", written.String())
	}
	if !reflect.DeepEqual(command.args, []string{"agent", "--no-leader", "stdio"}) {
		t.Fatal(command.args)
	}
	for _, entry := range []string{"GROK_HOME=" + home, "GROK_TELEMETRY_ENABLED=0", "GROK_CODEX_MCPS_ENABLED=0", "GROK_DISABLE_AUTOUPDATER=1"} {
		if !contains(command.env, entry) {
			t.Fatalf("environment lacks %s: %v", entry, command.env)
		}
	}
	encoded, _ := json.Marshal(models)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("non-model metadata leaked")
	}
}

func failureCode(err error) string {
	var failure *Error
	if !errors.As(err, &failure) {
		return ""
	}
	return failure.Code
}

func contains(env []string, entry string) bool {
	for _, candidate := range env {
		if candidate == entry {
			return true
		}
	}
	return false
}

func TestGrokEffortsAreRecordedOnlyWhereStated(t *testing.T) {
	reply := func(models string) string {
		return `{"jsonrpc":"2.0","id":1,"result":{"_meta":{"modelState":{"currentModelId":"b","availableModels":[` + models + `]}}}}`
	}
	models, err := readGrokCatalog(strings.NewReader(reply(
		`{"modelId":"none","_meta":{"supportsReasoningEffort":false,"totalContextTokens":"secret"}},`+
			`{"modelId":"b","name":"B","_meta":{"totalContextTokens":1.5}},`+
			`{"modelId":"unlisted","_meta":{"supportsReasoningEffort":true,"reasoningEffort":"high"}},`+
			`{"modelId":"fallback","_meta":{"supportsReasoningEffort":true,"reasoningEffort":"low","reasoningEfforts":[{"id":"low"},{"value":"high"}]}},`+
			`{"_meta":{}}`)), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{ID: "none", Name: "none", EffortsKnown: true, Efforts: []Effort{}},
		{ID: "b", Name: "B", IsDefault: true},
		{ID: "unlisted", Name: "unlisted"},
		{ID: "fallback", Name: "fallback", DefaultEffort: "low", EffortsKnown: true, Efforts: []Effort{{ID: "low", Default: true}, {ID: "high"}}},
	}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("%+v", models)
	}
}

func TestGrokCatalogFailsClosed(t *testing.T) {
	for code, input := range map[string]string{
		"request_failed":   `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"secret"}}`,
		"invalid_catalog":  `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"_meta":{"agentVersion":"secret"}}}`,
		"invalid_response": "secret",
		"missing_response": `{"jsonrpc":"2.0","method":"_x.ai/mcp/servers_updated"}`,
		"output_limit":     strings.Repeat("x", lineLimit+1),
	} {
		_, err := testDiscoverer(fakeRunner(input, nil, nil)).discover(context.Background(), cliProvider(harness.Grok, testBinary(t), t.TempDir()))
		requireFailure(t, err, harness.Grok, harness.FailureRequest, code)
	}
	// The whole stream is bounded too, not just each line.
	notifications := strings.Repeat(`{"jsonrpc":"2.0","method":"n","params":{"p":"`+strings.Repeat("x", 1<<16)+`"}}`+"\n", 70)
	if _, err := readGrokCatalog(strings.NewReader(notifications+grokFixture), io.Discard); failureCode(err) != "output_limit" {
		t.Fatalf("unbounded stream: %v", err)
	}
}
