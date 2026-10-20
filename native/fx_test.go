package native

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestFxArgsBindEveryValue(t *testing.T) {
	c := Config{
		Provider: harness.Provider{Engine: harness.Fx},
		Model:    "openai/gpt-5.4-nano",
		Effort:   "low",
		Fx:       FxOptions{PermissionMode: "auto"},
		Args:     []string{"--provider-order", "gateway"},
	}
	r := Request{Prompt: "-review this", ResumeSession: "fx-session-1", WorkDir: "/work", AppendInstructions: "be brief"}
	if err := validate(c, r); err != nil {
		t.Fatal(err)
	}
	got := buildArgs(c, r, nil)
	// fx takes the request as trailing arguments, so the prompt is last and
	// everything after "--" is prompt text.
	want := []string{
		"ask", "--json", "--model", "openai/gpt-5.4-nano", "--effort", "low", "--auto",
		"--resume-id", "fx-session-1", "--provider-order", "gateway", "--", "be brief\n\n-review this",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%q want %q", got, want)
	}
}

func TestFxPermissionModeIsRefusedWhenItIsNotOne(t *testing.T) {
	c := Config{Provider: harness.Provider{Engine: harness.Fx}, Fx: FxOptions{PermissionMode: "yolo"}}
	err := validate(c, Request{Prompt: "hi"})
	if err == nil || err.Code != CodeUnsupportedOption {
		t.Fatalf("permission mode: %v", err)
	}
}

func TestFxRefusesAHomeItCannotMove(t *testing.T) {
	c := Config{Provider: harness.Provider{Engine: harness.Fx, CLI: harness.CLI{Home: "/somewhere/else"}}}
	err := validate(c, Request{Prompt: "hi"})
	if err == nil || err.Code != CodeHomeUnsupported {
		t.Fatalf("home: %v", err)
	}
}

func TestFxOptionsForAnotherEngineAreRefused(t *testing.T) {
	c := Config{Provider: harness.Provider{Engine: harness.Fx}, Grok: GrokOptions{PermissionMode: "dontAsk"}}
	err := validate(c, Request{Prompt: "hi"})
	if err == nil || err.Code != CodeOptionForOtherEngine {
		t.Fatalf("foreign options: %v", err)
	}
}

func TestFxManagedFlagsAreRefusedInArgs(t *testing.T) {
	c := Config{Provider: harness.Provider{Engine: harness.Fx}, Args: []string{"--model", "other"}}
	err := validate(c, Request{Prompt: "hi"})
	if err == nil || err.Code != CodeManagedFlagInArgs {
		t.Fatalf("managed flag: %v", err)
	}
}

// The shapes below are trimmed from `fx ask --json` as fx 0.0.14 writes it.
func TestFxStreamReadsTheLiveDocument(t *testing.T) {
	var transcript bytes.Buffer
	var events []Event
	s, err := NewStream(harness.Fx, &transcript, StreamOptions{OnEvent: func(e Event) { events = append(events, e) }})
	if err != nil {
		t.Fatal(err)
	}
	s.UserPrompt("inspect the project")
	// fx writes its notices to standard error, which the library reads through
	// the same stream: they are not part of the run's record.
	_, _ = s.Write([]byte("[notice] skill discovery warning: something was skipped\n"))
	_, _ = s.Write([]byte(`{"output":"I found one issue.","final_output":"I found one issue.","exit_code":0,"model":"openai/gpt-5.4-nano","resolved_provider":"openai","session_id":"fx-session-1","steps":1,"tool_calls":[{"name":"read_file","status":"success"}],"usage":{"input_tokens":12,"output_tokens":3}}` + "\n"))
	s.Close()
	r := s.Snapshot()
	if r.SessionID != "fx-session-1" || string(r.Report) != `"I found one issue."` {
		t.Fatalf("report: %+v", r)
	}
	if r.Usage != (harness.Usage{Known: true, Input: 12, Output: 3}) {
		t.Fatalf("usage: %+v", r.Usage)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"session", "tool_start", "tool_end", "message", "usage"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("events %q want %q", kinds, want)
	}
	if strings.Contains(transcript.String(), "skill discovery warning") {
		t.Fatalf("a notice reached the transcript:\n%s", transcript.String())
	}
}

func TestFxStreamKeepsAFailedRunAsAFailure(t *testing.T) {
	var events []Event
	s, err := NewStream(harness.Fx, nil, StreamOptions{OnEvent: func(e Event) { events = append(events, e) }})
	if err != nil {
		t.Fatal(err)
	}
	s.UserPrompt("hi")
	_, _ = s.Write([]byte(`{"output":"API request failed · HTTP 502 · model_not_found: Model 'x' not found\n","final_output":"","exit_code":1,"model":"x","session_id":"fx-session-2","steps":0,"tool_calls":[],"usage":{"input_tokens":null,"output_tokens":null}}` + "\n"))
	s.Close()
	r := s.Snapshot()
	if r.Failure == "" || r.Report != nil {
		t.Fatalf("a failed run kept a report: %+v", r)
	}
	if r.Usage.Known {
		t.Fatal("unreported usage was read as zero")
	}
	if _, err = s.Report(); err == nil {
		t.Fatal("a failed run reported a response")
	}
}

// The document fx 0.0.14 writes when an asking run reaches a write: its
// output is what the assistant was about to do, and the error names why the
// run stopped.
func TestFxStreamNamesTheErrorRatherThanTheAssistantText(t *testing.T) {
	s, err := NewStream(harness.Fx, nil, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.UserPrompt("hi")
	_, _ = s.Write([]byte(`{"output":"I will create n.txt, then confirm completion.","final_output":"","error":"NonInteractivePermissionRequired","exit_code":1,"model":"","resolved_provider":null,"session_id":"","steps":0,"tool_calls":[{"name":"write_file","status":"error"}],"usage":{"input_tokens":11599,"output_tokens":70}}` + "\n"))
	s.Close()
	r := s.Snapshot()
	if r.Failure != "NonInteractivePermissionRequired" || r.Report != nil {
		t.Fatalf("failure: %+v", r)
	}
}

func TestFxStreamWithoutADocumentIsNotAResponse(t *testing.T) {
	s, err := NewStream(harness.Fx, nil, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.UserPrompt("hi")
	_, _ = s.Write([]byte("something else\n"))
	s.Close()
	if _, err = s.Report(); err == nil {
		t.Fatal("a run with no document reported a response")
	}
}

func TestFxRunRefusesAStructuredRequest(t *testing.T) {
	c := Config{Provider: harness.Provider{Engine: harness.Fx}}
	err := validate(c, Request{Prompt: "hi", Schema: `{"type":"object"}`})
	if err == nil || err.Code != CodeUnsupportedOption {
		t.Fatalf("schema: %v", err)
	}
}

func TestFxRunCarriesTheCallersInstructionsWithTheRequest(t *testing.T) {
	r := Request{Prompt: "the request", AppendInstructions: "the instructions"}
	if got := fxPrompt(r); got != "the instructions\n\nthe request" {
		t.Fatalf("prompt %q", got)
	}
}
