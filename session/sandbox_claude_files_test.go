//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestClaudeReferenceAdmissionKeepsNativeAndShellWritesDenied(t *testing.T) {
	root := t.TempDir()
	o, err := normalize(Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: t.TempDir(), Sandbox: &Sandbox{Write: true, Read: []string{root}}})
	if err != nil {
		t.Fatal(err)
	}
	root = o.Sandbox.Read[0]
	var doc struct {
		Permissions struct {
			AdditionalDirectories, Allow, Deny                                        []string
			BlockReadsOutsideWorkingDirectories, BlockWritesOutsideWorkingDirectories bool
		}
		Sandbox struct {
			Enabled, FailIfUnavailable, AllowUnsandboxedCommands bool
			Filesystem                                           struct{ AllowRead, DenyWrite []string }
		}
	}
	if err := json.Unmarshal([]byte(claudeSandboxSettings(o)), &doc); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(doc.Permissions.AdditionalDirectories, []string{root}) || !doc.Permissions.BlockReadsOutsideWorkingDirectories || !doc.Permissions.BlockWritesOutsideWorkingDirectories {
		t.Fatal("reference admission widened native boundaries")
	}
	for _, rule := range []string{"Edit(/" + root + ")", "Edit(/" + root + "/**)", "Read(/" + root + "/.git)", "Read(/" + root + "/**/.aws/**)"} {
		if !slices.Contains(doc.Permissions.Deny, rule) {
			t.Errorf("missing protected rule %s", rule)
		}
	}
	if !slices.Contains(doc.Sandbox.Filesystem.DenyWrite, root) || !slices.Contains(doc.Sandbox.Filesystem.DenyWrite, filepath.Join(o.WorkDir, ".git")) {
		t.Fatal("reference admission widened shell writes")
	}
	if !doc.Sandbox.Enabled || !doc.Sandbox.FailIfUnavailable || doc.Sandbox.AllowUnsandboxedCommands {
		t.Fatal("sandbox fallback was enabled")
	}
}

func TestClaudeReferenceNativeProofRefusesUnsupportedRuntime(t *testing.T) {
	binary, _ := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, harness.Claude, binary, true)
	o.Sandbox.Read = []string{t.TempDir()}
	// A settings echo from a fixture is never native file-tool proof.
	err := VerifySandbox(context.Background(), o)
	var capability *CapabilityError
	if !errors.As(err, &capability) || capability.Phase != BeforeLaunch {
		t.Fatalf("unqualified runtime admitted: %v", err)
	}
}

func TestClaudeFileResultsRequireMatchingStructuredResult(t *testing.T) {
	message := func(raw string) []struct {
		Content json.RawMessage `json:"content"`
	} {
		return []struct {
			Content json.RawMessage `json:"content"`
		}{{json.RawMessage(raw)}}
	}
	if _, _, found := claudeFileResult(message(`[{"type":"tool_result","tool_use_id":"other","content":"ok"}]`), "wanted"); found {
		t.Fatal("accepted unrelated result")
	}
	text, failed, found := claudeFileResult(message(`[{"type":"tool_result","tool_use_id":"wanted","is_error":true,"content":[{"type":"text","text":"permission denied"}]}]`), "wanted")
	if !found || !failed || text != "permission denied\n" {
		t.Fatal("lost structured denial")
	}
	for _, raw := range []string{`null`, `"permission denied"`, `[{"type":"text","text":"permission denied"}]`} {
		if _, _, found := claudeFileResult(message(raw), "wanted"); found {
			t.Fatal("accepted missing tool result")
		}
	}
}

func TestClaudeReferenceCacheIncludesEffectiveTools(t *testing.T) {
	binary, _ := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, harness.Claude, binary, true)
	o.Policy.ClaudeTools = []string{"Read"}
	normalized, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	before, err := sandboxKey(normalized, &launch{extra: sandboxArgs(normalized)})
	if err != nil {
		t.Fatal(err)
	}
	normalized.Policy.ClaudeTools = []string{"Read", "Write", "Edit", "Glob", "Grep"}
	after, err := sandboxKey(normalized, &launch{extra: sandboxArgs(normalized)})
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("file-tool proof reused across wider tool policy")
	}
}
func TestClaudeReferenceCannotContainWorkspace(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{root, work} {
		if _, err := normalize(Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: work, Sandbox: &Sandbox{Write: true, Read: []string{ref}}}); err == nil {
			t.Fatal("accepted reference containing workspace")
		}
	}
}
func TestClaudeEdgeReferenceAdmissionUnchanged(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: t.TempDir(), Sandbox: &Sandbox{Write: true, Read: []string{t.TempDir()}, Edge: &Edge{}}}
	var doc struct{ Permissions map[string]json.RawMessage }
	if err := json.Unmarshal([]byte(claudeSandboxSettings(o)), &doc); err != nil {
		t.Fatal(err)
	}
	if _, found := doc.Permissions["additionalDirectories"]; found {
		t.Fatal("unqualified native Edge reference admission")
	}
	if _, found := doc.Permissions["blockWritesOutsideWorkingDirectories"]; found {
		t.Fatal("Edge native policy changed")
	}
}

func TestClaudeSkillReferenceCannotContainWorkspace(t *testing.T) {
	skill := testSkill(t, "demo", false)
	for _, work := range []string{skill.Dir, filepath.Join(skill.Dir, "workspace")} {
		if err := os.MkdirAll(work, 0700); err != nil {
			t.Fatal(err)
		}
		_, err := normalize(Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: work, Skills: harness.Skills{Provided: []harness.Skill{skill}}, Sandbox: &Sandbox{Write: true}})
		if err == nil {
			t.Fatal("accepted skill reference containing workspace")
		}
	}
}
func TestClaudeReferenceNoFileToolsRemainsValid(t *testing.T) {
	o, err := normalize(Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: t.TempDir(), Policy: Policy{ClaudeTools: []string{}}, Sandbox: &Sandbox{Read: []string{t.TempDir()}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probeClaudeReferenceFiles(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeReferenceMissingPerlIsActionable(t *testing.T) {
	err := &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch, Reason: ClaudeReferenceShellRequiresPerl}
	if !strings.Contains(err.Error(), "requires Perl on PATH") {
		t.Fatal("missing native proof prerequisite has no remedy")
	}
}

func TestClaudeReferenceMissingPerlRefusesBeforeNativeLaunch(t *testing.T) {
	binary, log := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, harness.Claude, binary, true)
	o.Policy.ClaudeTools = []string{"Bash"}
	t.Setenv("PATH", t.TempDir())
	_, err := probeClaudeReferenceFiles(context.Background(), o)
	var capability *CapabilityError
	if !errors.As(err, &capability) || capability.Code != CapabilitySandboxUnavailable || capability.Reason != ClaudeReferenceShellRequiresPerl || capability.Phase != BeforeLaunch {
		t.Fatalf("wrong prerequisite refusal: %v", err)
	}
	data, _ := os.ReadFile(log)
	if len(data) > 0 {
		t.Fatal("missing proof prerequisite launched native CLI")
	}
}
