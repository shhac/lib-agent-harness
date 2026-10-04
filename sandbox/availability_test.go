package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func assertContentRefusal(t *testing.T, tool string, r Result, err error) {
	t.Helper()
	var refusal *RefusalError
	if !errors.As(err, &refusal) || refusal.Operation != tool || refusal.Code != RefusedNotOffered ||
		refusal.Capability.Availability != harness.Unsupported || refusal.Capability.Reason != FileToolsDisabledReason {
		t.Fatalf("%s: %+v %v", tool, r, err)
	}
	facts, ok := harness.ErrorFacts(err)
	if !ok || facts.Family != harness.FailureCapability || facts.Code != RefusedNotOffered {
		t.Fatal(facts)
	}
	refusedWith(t, r, RefusedNotOffered)
}

// Legacy content fixtures retain their positive controls on offered platforms,
// and explicitly check the containment contract on disabled platforms.
func contentDisabled(t *testing.T, tool string) bool {
	t.Helper()
	if ToolAvailability(tool) == "" {
		return false
	}
	w, _, _ := testWorkspace(t)
	r, err := w.readFile(context.Background(), json.RawMessage(`{"path":"a.txt"}`))
	if tool == workbenchSearchFiles {
		r, err = w.searchFiles(context.Background(), json.RawMessage(`{"pattern":"inside"}`))
	}
	if tool == workbenchEditFile {
		r, err = w.writeFile(context.Background(), tool, json.RawMessage(`{"path":"a.txt","old":"inside","new":"x"}`))
	}
	assertContentRefusal(t, tool, r, err)
	return true
}
func TestContentToolsRefuseBeforeIO(t *testing.T) {
	if ToolAvailability(workbenchReadFile) == "" {
		return
	}
	w, work, outside := testWorkspace(t)
	if err := os.Link(filepath.Join(outside, "secret.txt"), filepath.Join(work, "linked")); err != nil {
		t.Fatal(err)
	}
	w.openStep = func() { t.Error("content open reached") }
	w.step = func() { t.Error("content read reached") }
	w.contentStep = func(tool, stage string) { t.Errorf("%s target %s reached", tool, stage) }
	w.writeFault = func(string) error { t.Error("write stage reached"); return nil }
	for _, tool := range []string{workbenchReadFile, workbenchSearchFiles, workbenchEditFile} {
		calls := []func(context.Context, json.RawMessage) (Result, error){}
		switch tool {
		case workbenchReadFile:
			calls = append(calls, w.Read, w.readFile)
		case workbenchSearchFiles:
			calls = append(calls, w.Search, w.searchFiles)
		case workbenchEditFile:
			calls = append(calls, w.Edit, func(ctx context.Context, raw json.RawMessage) (Result, error) { return w.writeFile(ctx, tool, raw) })
		}
		for _, raw := range []string{`{"path":"a.txt","pattern":"inside","old":"inside","new":"x"}`, `{"path":"linked","pattern":"OUTSIDE","old":"OUTSIDE","new":"x"}`, `{"path":"missing/child","pattern":"x","old":"x","new":"y"}`, `{"pattern":"x"}`, `not json`} {
			for _, call := range calls {
				r, err := call(context.Background(), json.RawMessage(raw))
				assertContentRefusal(t, tool, r, err)
			}
		}
	}
	// Instance-local hooks do not affect a separate workspace.
	other, _, _ := testWorkspace(t)
	if r, err := other.List(context.Background(), json.RawMessage(`{}`)); err != nil || r.IsError {
		t.Fatal(r, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		call func(context.Context, json.RawMessage) (Result, error)
	}{
		{workbenchReadFile, w.Read}, {workbenchSearchFiles, w.Search}, {workbenchEditFile, w.Edit},
	} {
		r, err := tc.call(cancelled, json.RawMessage(`{}`))
		switch {
		case errors.Is(err, context.Canceled):
		case errors.Is(err, ErrUnsupported):
			assertContentRefusal(t, tc.name, r, err)
		case err == nil && tc.name == workbenchEditFile:
			refusedWith(t, r, wbWriteFailed)
		default:
			t.Fatal(r, err)
		}
	}
	var wg sync.WaitGroup
	for range 20 {
		for _, tc := range []struct {
			name string
			call func(context.Context, json.RawMessage) (Result, error)
		}{{workbenchReadFile, w.Read}, {workbenchSearchFiles, w.Search}, {workbenchEditFile, w.Edit}} {
			wg.Go(func() {
				r, err := tc.call(context.Background(), json.RawMessage(`{"path":"a.txt","pattern":"inside","old":"inside","new":"x"}`))
				assertContentRefusal(t, tc.name, r, err)
			})
		}
	}
	wg.Wait()
	if _, err := os.Stat(filepath.Join(work, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("parent created", err)
	}
	entries, err := os.ReadDir(work)
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected creation: %v %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(work, "a.txt"))
	if err != nil || string(data) != "inside\n" {
		t.Fatalf("workspace changed: %q %v", data, err)
	}
	w.Close()
	for _, call := range []func(context.Context, json.RawMessage) (Result, error){w.Read, w.Search, w.Edit} {
		if _, err := call(context.Background(), json.RawMessage(`{}`)); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	}

}

func mustArgs(args any) json.RawMessage { raw, _ := json.Marshal(args); return raw }

func effectiveContentCode(tool, original string) string {
	if ToolAvailability(tool) != "" {
		return RefusedNotOffered
	}
	return original
}
