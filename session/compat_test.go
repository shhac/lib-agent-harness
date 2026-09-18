//go:build !windows

package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// An ordinary session's reference digest is persisted by callers and has to keep
// resuming across this change. The value below was computed from the digest
// shape that existed before restricted sessions were added; if it moves, every
// stored reference for an ordinary session stops resuming.
func TestOrdinaryReferenceDigestIsUnchanged(t *testing.T) {
	o, err := normalize(Options{Engine: Claude, Binary: "claude", Model: "haiku", Effort: "low", WorkDir: t.TempDir(), Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(struct {
		Binary, Model, Effort string
		Instructions          Instructions
		Policy                Policy
		ToolsSpecified        bool
	}{o.Binary, o.Model, o.Effort, o.Instructions, o.Policy, false})
	expected := digestOf(legacy)
	if got := reference(o, "s1").ConfigHash; got != expected {
		t.Fatalf("ordinary reference digest changed:\n got %s\nwant %s", got, expected)
	}
	// And the distinction that digest already carried still holds.
	withEmptyTools := o
	withEmptyTools.Policy.ClaudeTools = []string{}
	if reference(withEmptyTools, "s1") == reference(o, "s1") {
		t.Error("an explicitly empty tool list no longer changes the digest")
	}
}

func digestOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// A caller keeps its own Restriction and may launch twice from it. Normalizing
// must not write through that pointer, and the tool schemas a session was
// checked with must not be reachable for later mutation.
func TestNormalizeDoesNotMutateTheCallersRestriction(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}
	caller := &Restriction{Tools: ToolHost{
		Server:  "agent_workspace",
		Tools:   []ToolDefinition{{Name: "read_file", Schema: schema}},
		Handler: echoHandler(t), Dir: privateDir(t), Bridge: Bridge{Path: "/usr/bin/true"},
	}}
	o := Options{Engine: Claude, Binary: "claude", Model: "haiku", WorkDir: t.TempDir(), Home: t.TempDir(), Restriction: caller}
	normalized, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	if caller.Probe != 0 {
		t.Errorf("normalizing wrote a default into the caller's restriction: %v", caller.Probe)
	}
	if normalized.Restriction == caller {
		t.Fatal("the normalized session shares the caller's restriction pointer")
	}
	before := reference(normalized, "s1")
	// A caller editing its own schema afterwards must not change what this
	// session was checked with.
	schema["properties"].(map[string]any)["path"] = map[string]any{"type": "number"}
	caller.Tools.Tools[0].Name = "something_else"
	if reference(normalized, "s1") != before {
		t.Fatal("a later edit to the caller's definitions changed a live session's surface")
	}
	if got := normalized.Restriction.Tools.Tools[0].Name; got != "read_file" {
		t.Errorf("tool definitions were not frozen: %q", got)
	}
}

// The launch marker exists before the process does. A crash in between leaves a
// marker naming nothing, and that is reserved — not read as "nothing ran".
func TestUnidentifiedLaunchMarkerReservesTheAssignment(t *testing.T) {
	dir := privateDir(t)
	if err := recordLaunch(dir, launchRecord{Engine: "claude", Launch: dir, Started: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	out, err := Reclaim(context.Background(), dir)
	if !errors.Is(err, ErrUnreclaimed) || !errors.Is(err, ErrUncertainLaunch) {
		t.Fatalf("a marker with no process identity was not reserved: %+v %v", out, err)
	}
	if out.Confirmed || out.Terminated {
		t.Fatalf("an uncertain launch was reported as settled: %+v", out)
	}
	if !out.Found {
		t.Error("an uncertain launch was not reported as something to look at")
	}
}

// Recording the identity is what makes a harness findable later, so a session
// whose identity could not be written must not go on to do work.
func TestSessionRefusesToStartWhenItsIdentityCannotBeRecorded(t *testing.T) {
	dir := privateDir(t)
	// A directory where the marker file should be makes the write fail.
	if err := os.Mkdir(launchPath(dir), 0700); err != nil {
		t.Fatal(err)
	}
	err := recordLaunch(dir, launchRecord{Engine: "claude", Launch: dir})
	if err == nil {
		t.Fatal("an unwritable launch record was reported as recorded")
	}
}

// Finishing with an assignment clears the marker, but only on positive evidence
// that the harness is gone.
func TestReleaseClearsTheMarkerOnlyWhenNothingSurvives(t *testing.T) {
	dir := privateDir(t)
	if err := recordLaunch(dir, launchRecord{Engine: "claude", PID: 999999, Group: 999999, Launch: dir}); err != nil {
		t.Fatal(err)
	}
	out, err := Reclaim(context.Background(), dir)
	if err != nil || !out.Confirmed {
		t.Fatalf("a dead group was not confirmed absent: %+v %v", out, err)
	}
	if err = clearLaunchRecord(dir); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(launchPath(dir)); !os.IsNotExist(err) {
		t.Fatal("the marker survived a confirmed release")
	}
	// Clearing an absent marker is not an error: release is idempotent.
	if err = clearLaunchRecord(dir); err != nil {
		t.Fatalf("releasing twice failed: %v", err)
	}
}
