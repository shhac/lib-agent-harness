package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
	"github.com/shhac/lib-agent-harness/internal/skills"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

var finishTool = ToolDefinition{Name: "finish", Description: "Report the work.", Schema: map[string]any{"type": "object"}, Closing: true}

// workbenchOptions is an API session with a read-only workbench on a fresh
// workspace beside its runtime home, and the caller's finish tool.
func workbenchOptions(t *testing.T, handler ToolHandler) Options {
	t.Helper()
	o := apiOptions(t, "https://gateway.invalid/v1", handler, finishTool)
	o.WorkDir = t.TempDir()
	o.Workbench = &Workbench{}
	return o
}

func nopHandler() ToolHandler {
	return ToolHandlerFunc(func(context.Context, ToolCall) (ToolResult, error) { return ToolResult{Content: "ok"}, nil })
}

// symlink makes a link, skipping where the environment refuses links.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if errors.Is(err, syscall.Errno(1314)) {
		// Windows without the privilege to create links.
		err = fmt.Errorf("%w: %v", fs.ErrPermission, err)
	}
	testenv.SkipIfRefused(t, "creating a symbolic link", err)
}

func workbenchRefusal(t *testing.T, err error) string {
	t.Helper()
	var refused *UnsupportedError
	if !errors.As(err, &refused) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	return refused.Code
}

func TestWorkbenchIsRefusedForCLIEngines(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok} {
		o := Options{Provider: harness.Provider{Engine: engine, CLI: harness.CLI{Home: t.TempDir()}}, WorkDir: t.TempDir(), Workbench: &Workbench{}}
		if engine == harness.Grok {
			o.Policy.GrokPermission = GrokDenyWhenAsked
		}
		_, err := normalize(o)
		var unsupported *UnsupportedError
		if !errors.As(err, &unsupported) || unsupported.Code != RefusedNotOffered || unsupported.Operation != "workbench" {
			t.Errorf("%s: %v", engine, err)
		}
	}
}

func TestWorkbenchIsOffered(t *testing.T) {
	o := workbenchOptions(t, nopHandler())
	o.complete = (&scriptedModel{}).complete
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	closeAPI(t, s)
}

func TestWorkbenchNormalizeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, o *Options)
		code string
	}{
		{"no work dir", func(t *testing.T, o *Options) { o.WorkDir = "" }, RefusedWorkDir},
		{"relative work dir", func(t *testing.T, o *Options) { o.WorkDir = "repo" }, RefusedWorkDir},
		{"missing work dir", func(t *testing.T, o *Options) { o.WorkDir = filepath.Join(o.WorkDir, "absent") }, RefusedWorkDir},
		{"work dir is a file", func(t *testing.T, o *Options) {
			file := filepath.Join(o.WorkDir, "file")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			o.WorkDir = file
		}, RefusedWorkDir},
		{"work dir without a workbench", func(t *testing.T, o *Options) { o.Workbench = nil }, RefusedConflict},
		{"a result limit too small for its notes", func(t *testing.T, o *Options) { o.Restriction.Tools.MaxResultBytes = minWorkbenchResult - 1 }, RefusedLimit},
		{"runtime home inside work dir", func(t *testing.T, o *Options) { o.RuntimeHome = privateDirIn(t, o.WorkDir, "state") }, RefusedWorkDir},
		{"work dir inside runtime home", func(t *testing.T, o *Options) { o.WorkDir = privateDirIn(t, o.RuntimeHome, "repo") }, RefusedWorkDir},
		{"the same directory", func(t *testing.T, o *Options) { o.WorkDir = o.RuntimeHome }, RefusedWorkDir},
		{"runtime home reached through a link", func(t *testing.T, o *Options) {
			link := filepath.Join(t.TempDir(), "state")
			symlink(t, privateDirIn(t, o.WorkDir, "state"), link)
			o.RuntimeHome = link
		}, RefusedWorkDir},
		{"work dir reached through a link", func(t *testing.T, o *Options) {
			link := filepath.Join(t.TempDir(), "repo")
			symlink(t, o.RuntimeHome, link)
			o.WorkDir = link
		}, RefusedWorkDir},
		{"a different case", func(t *testing.T, o *Options) {
			work := privateDirIn(t, t.TempDir(), "Repo")
			if _, err := os.Stat(filepath.Join(filepath.Dir(work), "REPO")); err != nil {
				t.Skip("the temporary volume is case-sensitive")
			}
			o.WorkDir = filepath.Join(filepath.Dir(work), "REPO")
			o.RuntimeHome = privateDirIn(t, work, "state")
		}, RefusedWorkDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := workbenchOptions(t, nopHandler())
			tc.edit(t, &o)
			_, err := normalize(o)
			if code := workbenchRefusal(t, err); code != tc.code {
				t.Fatalf("code %s: %v", code, err)
			}
		})
	}
	t.Run("siblings", func(t *testing.T) {
		o := workbenchOptions(t, nopHandler())
		base := t.TempDir()
		o.WorkDir, o.RuntimeHome = privateDirIn(t, base, "repo"), privateDirIn(t, base, "repo-state")
		o.Restriction.Tools.MaxResultBytes = minWorkbenchResult
		n, err := normalize(o)
		if err != nil {
			t.Fatal(err)
		}
		resolved, _ := filepath.EvalSymlinks(o.WorkDir)
		if n.WorkDir != resolved || n.Workbench == nil {
			t.Fatalf("work dir %q, workbench %v", n.WorkDir, n.Workbench)
		}
	})
}

func privateDirIn(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWorkbenchReservesItsToolNames(t *testing.T) {
	for _, name := range workbenchReserved {
		if skills.IsTool(name) {
			t.Errorf("%s is also a skill tool", name)
		}
		o := workbenchOptions(t, nopHandler())
		o.Restriction.Tools.Tools = []ToolDefinition{finishTool, {Name: name, Schema: map[string]any{"type": "object"}}}
		if _, err := normalize(o); workbenchRefusal(t, err) != RefusedWorkbenchToolReserved {
			t.Errorf("%s: %v", name, err)
		}
		// Without a workbench the name is the caller's, and so is any result
		// limit.
		o.Workbench, o.WorkDir = nil, ""
		o.Restriction.Tools.MaxResultBytes = 100
		if _, err := normalize(o); err != nil {
			t.Errorf("%s without a workbench: %v", name, err)
		}
	}
	if len(workbenchReserved) != 6 {
		t.Fatalf("reserved %v", workbenchReserved)
	}
}

// The workbench is wrapped around the digest, so every existing digest,
// pinned in TestAPIRefIsStableAndCarriesNoCredential and
// TestPersistedRefsAreStable, stays what it was.
func TestWorkbenchRefIsPinned(t *testing.T) {
	o := Options{
		Provider: harness.Provider{Engine: harness.OpenAICompatible, API: harness.API{BaseURL: "https://gateway.invalid/v1", Dialect: harness.OpenAIChatCompletions,
			Credentials: func(context.Context) (string, error) { return apiToken, nil }, EffortParameter: harness.EffortReasoningObject}},
		Model: "xai/grok-4", Effort: "low", RuntimeHome: "/state", AccountIdentity: "team",
		Instructions: Instructions{Mode: Replace, Text: "be brief"},
		Restriction:  &Restriction{Tools: ToolHost{Server: "work", Tools: []ToolDefinition{readFile}}},
	}
	without := apiReference(o, "s1")
	o.WorkDir, o.Workbench = "/work", &Workbench{}
	got, _ := json.Marshal(apiReference(o, "s1"))
	const want = `{"engine":"openai-compatible","id":"s1","home":"/state","work_dir":"/work","account_identity":"team","config_hash":"925649c533f256d20efa049acf67580042263511fa1e1879331f533cc52b3aa5"}`
	if string(got) != want {
		t.Fatalf("persisted ref changed:\n got %s\nwant %s", got, want)
	}
	if apiReference(o, "s1").ConfigHash == without.ConfigHash {
		t.Fatal("the workbench did not move the digest")
	}
	moved := o
	moved.WorkDir = "/other"
	if apiReference(moved, "s1").ConfigHash == apiReference(o, "s1").ConfigHash {
		t.Fatal("WorkDir did not move the digest")
	}
}

func TestWorkbenchResumeMustMatch(t *testing.T) {
	o := workbenchOptions(t, nopHandler())
	s := startAPI(t, o)
	ref := s.Ref()
	resolved, _ := filepath.EvalSymlinks(o.WorkDir)
	if ref.WorkDir != resolved {
		t.Fatalf("ref %+v", ref)
	}
	if _, err := s.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	transcript, _ := readTranscript(t, o.RuntimeHome, ref.ID)
	other := o
	other.WorkDir = t.TempDir()
	dropped := o
	dropped.Workbench, dropped.WorkDir = nil, ""
	for name, changed := range map[string]Options{"another work dir": other, "no workbench": dropped} {
		if _, err := Resume(context.Background(), changed, ref); !errors.Is(err, ErrIncompatibleResume) {
			t.Errorf("%s resumed: %v", name, err)
		}
	}
	if after, _ := readTranscript(t, o.RuntimeHome, ref.ID); len(after) != len(transcript) {
		t.Fatal("a refused resume touched the transcript")
	}
	resumed, err := Resume(context.Background(), o, ref)
	if err != nil {
		t.Fatal(err)
	}
	closeAPI(t, resumed)

	// A session that had no workbench cannot gain one on resume.
	plain := apiOptions(t, "https://gateway.invalid/v1", nopHandler(), finishTool)
	p := startAPI(t, plain)
	plainRef := p.Ref()
	if _, err = p.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	gained := plain
	gained.WorkDir, gained.Workbench = t.TempDir(), &Workbench{}
	if _, err = Resume(context.Background(), gained, plainRef); !errors.Is(err, ErrIncompatibleResume) {
		t.Fatalf("a workbench was added on resume: %v", err)
	}
}

// scriptedModel answers each request with the next step's calls, recording
// the tools and messages it was sent.
type scriptedModel struct {
	mu       sync.Mutex
	steps    [][]scriptedCall
	tools    [][]completion.Tool
	messages [][]completion.Message
}

func (m *scriptedModel) complete(_ context.Context, _ completion.Config, messages []completion.Message, tools []completion.Tool) (completion.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = append(m.tools, tools)
	m.messages = append(m.messages, append([]completion.Message(nil), messages...))
	result := completion.Result{Message: completion.Message{Role: "assistant", Content: "Done."}, Usage: harness.Usage{Known: true, Input: 1, Output: 1}}
	if len(m.steps) > 0 {
		result.Message.Content = ""
		for _, c := range m.steps[0] {
			call := completion.ToolCall{ID: c.id, Type: "function"}
			call.Function.Name, call.Function.Arguments = c.name, c.args
			result.Message.ToolCalls = append(result.Message.ToolCalls, call)
		}
		m.steps = m.steps[1:]
	}
	return result, nil
}

func offeredNames(tools []completion.Tool) []string {
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Function.Name)
	}
	return names
}

func TestWorkbenchToolsReachTheRequest(t *testing.T) {
	model := &scriptedModel{}
	o := workbenchOptions(t, nopHandler())
	o.Restriction.Tools.Tools = []ToolDefinition{readFileLike, finishTool}
	o.complete = model.complete
	s := startAPI(t, o)
	if done := runAPITurnToEnd(t, s, "Look."); done.err != nil {
		t.Fatal(done.err)
	}
	tools := model.tools[0]
	if names := strings.Join(offeredNames(tools), ","); names != "view,finish,read_file,list_files,search_files" {
		t.Fatalf("tools %s", names)
	}
	for i, def := range workbenchDefinitions(o) {
		got := tools[2+i]
		if got.Type != "function" || got.Function.Description != def.Description || !reflect.DeepEqual(got.Function.Parameters, def.Schema) {
			t.Errorf("%s offered as %+v", def.Name, got)
		}
	}
	// Every request carries them; pin their size so no edit bloats them.
	encoded, _ := json.Marshal(completionTools(workbenchDefinitions(o)))
	if len(encoded) != workbenchDefinitionsBytes {
		t.Fatalf("the workbench definitions encode to %d bytes, pinned at %d", len(encoded), workbenchDefinitionsBytes)
	}

	plainModel := &scriptedModel{}
	plain := apiOptions(t, "https://gateway.invalid/v1", nopHandler(), readFileLike, finishTool)
	plain.complete = plainModel.complete
	p := startAPI(t, plain)
	if done := runAPITurnToEnd(t, p, "Look."); done.err != nil {
		t.Fatal(done.err)
	}
	if names := strings.Join(offeredNames(plainModel.tools[0]), ","); names != "view,finish" {
		t.Fatalf("a session without a workbench offered %s", names)
	}
}

const workbenchDefinitionsBytes = 1710

var readFileLike = ToolDefinition{Name: "view", Description: "The caller's own view.", Schema: map[string]any{"type": "object"}}

// On the wire as well: the endpoint receives the workbench's tools after the
// caller's.
func TestWorkbenchToolsReachTheWire(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		_ = listener.Close()
	}
	testenv.SkipIfRefused(t, "listening on loopback", err)
	e := newEndpoint(t, answer("Done."))
	o := workbenchOptions(t, nopHandler())
	o.Provider.API.BaseURL = e.url
	s := startAPI(t, o)
	if done := runAPITurnToEnd(t, s, "Look."); done.err != nil {
		t.Fatal(done.err)
	}
	if names := strings.Join(e.seen()[0].toolNames(), ","); names != "finish,read_file,list_files,search_files" {
		t.Fatalf("tools on the wire %s", names)
	}
}

// A model lists, reads and then calls the caller's closing tool. The library
// answers its own calls; the caller's handler sees only its own.
func TestWorkbenchLoopCallsItsToolsBesideTheCallers(t *testing.T) {
	var mu sync.Mutex
	var calls []ToolCall
	o := workbenchOptions(t, echo(&calls, &mu))
	writeFile(t, filepath.Join(o.WorkDir, "docs", "notes.md"), "first\nsecond\n")
	model := &scriptedModel{steps: [][]scriptedCall{
		{{"c1", "list_files", `{}`}},
		{{"c2", "read_file", `{"path":"docs/notes.md"}`}},
		{{"c3", "read_file", `{"path":"../escape"}`}},
		{{"search", "search_files", `{"pattern":"second"}`}},
		{{"c4", "finish", `{"ok":true}`}},
	}}
	o.complete = model.complete
	s := startAPI(t, o)
	done := runAPITurnToEnd(t, s, "Review the notes.")
	if done.err != nil || done.result.Status != "completed" || !s.ToolsClosed() {
		t.Fatalf("%+v %v closed %v", done.result, done.err, s.ToolsClosed())
	}
	if len(calls) != 1 || calls[0].Name != "finish" {
		t.Fatalf("the caller's handler saw %+v", calls)
	}
	answered := func(step int, id string) string {
		for _, m := range model.messages[step] {
			if m.Role == "tool" && m.ToolCallID == id {
				return m.Content
			}
		}
		t.Fatalf("step %d has no answer to %s", step, id)
		return ""
	}
	if got := answered(1, "c1"); got != "docs/\ndocs/notes.md" {
		t.Fatalf("list_files answered %q", got)
	}
	if got := answered(2, "c2"); got != "first\nsecond\n" {
		t.Fatalf("read_file answered %q", got)
	}
	if got := answered(3, "c3"); !strings.Contains(got, "file_outside_workspace") {
		t.Fatalf("an escape answered %q", got)
	}
	var events []string
	for _, ev := range done.events {
		if ev.Kind == "tool_started" || ev.Kind == "tool_completed" {
			events = append(events, ev.Kind+":"+ev.Tool+":"+ev.Status)
		}
		if ev.Kind == "tool_started" && ev.Tool == "read_file" && ev.ItemID == "c2" && string(ev.Input) != `{"path":"docs/notes.md"}` {
			t.Errorf("read_file's input %s", ev.Input)
		}
		if ev.Kind == "tool_completed" && ev.ItemID == "c2" && ev.Output != "first\nsecond\n" {
			t.Errorf("read_file's output %q", ev.Output)
		}
	}
	if got := answered(4, "search"); got != "docs/notes.md:2: second" {
		t.Fatal(got)
	}
	want := "tool_started:list_files:running,tool_completed:list_files:completed,tool_started:read_file:running,tool_completed:read_file:completed,tool_started:read_file:running,tool_completed:read_file:failed,tool_started:search_files:running,tool_completed:search_files:completed,tool_started:finish:running,tool_completed:finish:completed"
	if strings.Join(events, ",") != want {
		t.Fatalf("events\n got %v\nwant %s", events, want)
	}
	records, _ := readTranscript(t, o.RuntimeHome, s.Ref().ID)
	results := 0
	for _, r := range records {
		if r.Type == recordToolResult {
			results++
		}
	}
	if results != 5 {
		t.Fatalf("%d results recorded", results)
	}
	if strings.Contains(answered(2, "c2")+answered(3, "c3"), o.WorkDir) {
		t.Fatal("a host path reached the model")
	}
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
