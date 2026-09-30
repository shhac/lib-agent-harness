package session

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/catalog"
	"github.com/shhac/lib-agent-harness/completion"
)

// answeringModel stands in for the endpoint: it answers every request with
// text and records the tools each one offered.
type answeringModel struct {
	mu    sync.Mutex
	tools [][]completion.Tool
}

func (m *answeringModel) complete(_ context.Context, _ completion.Config, _ []completion.Message, tools []completion.Tool) (completion.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = append(m.tools, tools)
	return completion.Result{Message: completion.Message{Role: "assistant", Content: "Done."}, Usage: harness.Usage{Known: true, Input: 4, Output: 1}}, nil
}

func (m *answeringModel) requests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tools)
}

func catalogOptions(t *testing.T) (Options, *answeringModel) {
	t.Helper()
	model := &answeringModel{}
	o := apiOptions(t, "https://gateway.invalid/v1", nopHandler())
	o.complete = model.complete
	return o, model
}

func withoutTools(model string) *catalog.Model {
	return &catalog.Model{ID: model, Name: model, Parameters: []string{"temperature", "max_tokens"}, ParametersKnown: true}
}

func requireWithoutTools(t *testing.T, err error) {
	t.Helper()
	var refused *UnsupportedError
	if !errors.As(err, &refused) || refused.Code != RefusedModelWithoutTools || refused.Operation != "model" {
		t.Fatalf("not refused for tool calling: %v", err)
	}
	facts, ok := harness.ErrorFacts(err)
	if !ok || facts.Code != RefusedModelWithoutTools || facts.Family != harness.FailureCapability || facts.Engine != harness.OpenAICompatible {
		t.Fatalf("facts %+v", facts)
	}
}

// snapshot reads every file under dir, so a refusal can be shown to have
// changed nothing.
func snapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		files[path] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func sameFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for path, data := range a {
		if other, ok := b[path]; !ok || !bytes.Equal(data, other) {
			return false
		}
	}
	return true
}

func TestAPISessionRefusesAModelWithoutToolCallingBeforeLaunch(t *testing.T) {
	o, model := catalogOptions(t)
	o.CatalogModel = withoutTools(o.Model)
	s, err := Start(context.Background(), o)
	if s != nil {
		closeAPI(t, s)
		t.Fatal("started")
	}
	requireWithoutTools(t, err)
	// Open refuses the same way rather than falling back to a fresh session.
	if s, _, err := Open(context.Background(), o, nil); s != nil {
		closeAPI(t, s)
		t.Fatal("Open started a model without tool calling")
	} else {
		requireWithoutTools(t, err)
	}
	// Resolved names the concrete model an alias selects; it matches too.
	aliased := withoutTools("provider/alias")
	aliased.Resolved = o.Model
	o.CatalogModel = aliased
	_, err = Start(context.Background(), o)
	requireWithoutTools(t, err)
	if model.requests() != 0 {
		t.Fatal("a refused session sent a request")
	}
	if entries, err := os.ReadDir(o.RuntimeHome); err != nil || len(entries) != 0 {
		t.Fatalf("a refused session left files: %v %v", entries, err)
	}
}

// A workbench adds tools, so the same check covers it, ahead of the
// workbench's own checks.
func TestAPISessionRefusesAWorkbenchOnAModelWithoutToolCalling(t *testing.T) {
	enableWorkbench(t)
	o := workbenchOptions(t, nopHandler())
	o.CatalogModel = withoutTools(o.Model)
	_, err := normalize(o)
	requireWithoutTools(t, err)
	o.CatalogModel = &catalog.Model{ID: o.Model, Parameters: []string{"tools"}, ParametersKnown: true}
	if _, err = normalize(o); err != nil {
		t.Fatalf("a model with tool calling was refused: %v", err)
	}
}

func TestAPISessionGoesAheadWhenToolCallingIsListedOrUnknown(t *testing.T) {
	for name, entry := range map[string]*catalog.Model{
		"no entry":           nil,
		"parameters unknown": {ID: "synthetic-model"},
		"stale values":       {ID: "synthetic-model", Parameters: []string{"seed"}},
		"with tools":         {ID: "synthetic-model", Parameters: []string{"tools", "seed"}, ParametersKnown: true},
	} {
		t.Run(name, func(t *testing.T) {
			o, model := catalogOptions(t)
			o.CatalogModel = entry
			s := startAPI(t, o)
			if done := runAPITurnToEnd(t, s, "Go."); done.result.Status != "completed" {
				t.Fatalf("%+v %v", done.result, done.err)
			}
			if model.requests() != 1 || len(model.tools[0]) == 0 {
				t.Fatal("the request did not carry the tools")
			}
		})
	}
}

// The entry is advisory and not part of a Ref, so it does not move the
// digest, and the session keeps no copy of it.
func TestAPISessionCatalogModelLeavesTheRefAlone(t *testing.T) {
	o, _ := catalogOptions(t)
	plain, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	o.CatalogModel = &catalog.Model{ID: o.Model, Parameters: []string{"tools"}, ParametersKnown: true}
	checked, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	if checked.CatalogModel != nil {
		t.Fatal("the session kept the caller's entry")
	}
	if apiReference(checked, "s1") != apiReference(plain, "s1") {
		t.Fatal("CatalogModel moved the digest")
	}
}

func TestCatalogModelMustDescribeTheModel(t *testing.T) {
	o, _ := catalogOptions(t)
	for name, entry := range map[string]*catalog.Model{
		"other id":       {ID: "another-model", Parameters: []string{"tools"}, ParametersKnown: true},
		"other resolved": {ID: "provider/alias", Resolved: "another-model"},
		"empty":          {},
	} {
		o.CatalogModel = entry
		if _, err := normalize(o); !isRefusal(err, RefusedConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
	codex := Options{Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Home: t.TempDir()}}, WorkDir: t.TempDir(), CatalogModel: &catalog.Model{ID: "gpt"}}
	if _, err := normalize(codex); !isRefusal(err, RefusedConflict) {
		t.Fatalf("a CLI session accepted CatalogModel: %v", err)
	}
}

func isRefusal(err error, code string) bool {
	var refused *UnsupportedError
	return errors.As(err, &refused) && refused.Code == code
}

// A refused Resume touches neither the transcript nor its lock: the
// conversation resumes as it was once the entry is corrected.
func TestAPISessionResumeRefusalLeavesTheConversationAlone(t *testing.T) {
	o, model := catalogOptions(t)
	s := startAPI(t, o)
	runAPITurnToEnd(t, s, "Start.")
	ref := s.Ref()
	closeAPI(t, s)
	if _, err := s.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, o.RuntimeHome)

	refused := o
	refused.CatalogModel = withoutTools(o.Model)
	resumed, err := Resume(context.Background(), refused, ref)
	if resumed != nil {
		closeAPI(t, resumed)
		t.Fatal("resumed")
	}
	requireWithoutTools(t, err)
	if !sameFiles(before, snapshot(t, o.RuntimeHome)) {
		t.Fatal("a refused Resume changed the conversation's files")
	}
	if model.requests() != 1 {
		t.Fatal("a refused Resume sent a request")
	}

	resumed, err = Resume(context.Background(), o, ref)
	if err != nil {
		t.Fatalf("the conversation was left locked or changed: %v", err)
	}
	defer closeAPI(t, resumed)
	if done := runAPITurnToEnd(t, resumed, "Again."); done.result.Status != "completed" {
		t.Fatalf("%+v %v", done.result, done.err)
	}
}
