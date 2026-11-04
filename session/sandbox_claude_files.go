package session

// Native file tools have a separate permission boundary from the shell sandbox.
// This provider does no inference: its only responses are fixed file-tool calls
// on disposable sentinels, executed by the installed, unmodified Claude CLI.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
	"github.com/shhac/lib-agent-harness/process"
)

// ClaudeReferenceShellRequiresPerl identifies the optional Bash proof prerequisite.
const ClaudeReferenceShellRequiresPerl = "claude_reference_shell_requires_perl"

type claudeFileCall struct {
	ID, Tool string
	Input    map[string]any
	Want     string
	Denied   bool
}
type claudeFileObservation struct {
	ID, Tool      string
	Denied        bool
	Detail        string
	Before, After string
}

// probeClaudeReferenceFiles proves the read-only-reference policy through native
// tools before a real login or a production reference is exposed. The launch
// policy and arguments use the ordinary builders. Only the disposable paths,
// account home and provider endpoint differ.
func probeClaudeReferenceFiles(ctx context.Context, raw Options) ([]claudeFileObservation, error) {
	failure := func(reason string) error {
		return &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch, Reason: reason}
	}
	root, err := os.MkdirTemp("", "agent-harness-reference-proof-")
	if err != nil {
		return nil, failure("claude_reference_fixture_unavailable")
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, failure("claude_reference_fixture_unavailable")
	}
	home := filepath.Join(root, "home")
	work, refs := filepath.Join(home, "work"), filepath.Join(home, "refs")
	for _, p := range []string{home, work, refs, filepath.Join(root, "temp"), filepath.Join(refs, ".git"), filepath.Join(refs, "nested", ".aws"), filepath.Join(home, "sibling")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, failure("claude_reference_fixture_unavailable")
		}
	}
	files := map[string]string{
		filepath.Join(refs, "read.txt"):                      "reference-proof-visible",
		filepath.Join(work, "edit.txt"):                      "workspace-before",
		filepath.Join(home, "sibling", "secret.txt"):         "outside-proof-secret",
		filepath.Join(home, "secret.txt"):                    "home-proof-secret",
		filepath.Join(refs, ".git", "config"):                "git-proof-secret",
		filepath.Join(refs, "nested", ".aws", "credentials"): "credential-proof-secret",
	}
	for p, data := range files {
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			return nil, failure("claude_reference_fixture_unavailable")
		}
	}
	for _, base := range []string{refs, work} {
		if err := os.Symlink(filepath.Join(home, "sibling"), filepath.Join(base, "escape")); err != nil {
			return nil, failure("claude_reference_fixture_unavailable")
		}
	}
	o := raw
	sandbox := *raw.Sandbox
	sandbox.Read = []string{refs}
	o.Sandbox = &sandbox
	o.WorkDir = work
	o.Provider.CLI.Home = home
	// Skills are caller inputs, not part of this disposable capability fixture.
	o.skills = nil
	l := &launch{extra: claudeSandboxArgs(o)}
	var calls []claudeFileCall
	add := func(id, tool string, input map[string]any, want string, denied bool) {
		if slices.Contains(o.Policy.ClaudeTools, tool) {
			calls = append(calls, claudeFileCall{id, tool, input, want, denied})
		}
	}
	read := func(id, p, want string, denied bool) { add(id, "Read", map[string]any{"file_path": p}, want, denied) }
	read("reference-read", filepath.Join(refs, "read.txt"), "reference-proof-visible", false)
	add("reference-glob", "Glob", map[string]any{"path": refs, "pattern": "*.txt"}, "read.txt", false)
	add("reference-grep", "Grep", map[string]any{"path": refs, "pattern": "reference-proof-visible", "output_mode": "content"}, "reference-proof-visible", false)
	add("reference-recursive-glob", "Glob", map[string]any{"path": refs, "pattern": "**/*"}, "read.txt", false)
	add("reference-recursive-private-grep", "Grep", map[string]any{"path": refs, "pattern": "proof-secret", "output_mode": "content"}, "No matches found", false)
	read("workspace-read", filepath.Join(work, "edit.txt"), "workspace-before", false)
	add("workspace-write", "Write", map[string]any{"file_path": filepath.Join(work, "new.txt"), "content": "workspace-created"}, "", !sandbox.Write)
	add("workspace-edit", "Edit", map[string]any{"file_path": filepath.Join(work, "edit.txt"), "old_string": "workspace-before", "new_string": "workspace-after"}, "", !sandbox.Write)
	add("reference-write", "Write", map[string]any{"file_path": filepath.Join(refs, "new.txt"), "content": "must-not-exist"}, "", true)
	add("reference-overwrite", "Write", map[string]any{"file_path": filepath.Join(refs, "read.txt"), "content": "must-not-change"}, "", true)
	add("reference-edit", "Edit", map[string]any{"file_path": filepath.Join(refs, "read.txt"), "old_string": "reference-proof-visible", "new_string": "must-not-change"}, "", true)
	for _, item := range []struct{ id, p string }{
		{"sibling", filepath.Join(home, "sibling", "secret.txt")},
		{"home", filepath.Join(home, "secret.txt")},
		{"git", filepath.Join(refs, ".git", "config")},
		{"credential", filepath.Join(refs, "nested", ".aws", "credentials")},
		{"reference-symlink", filepath.Join(refs, "escape", "secret.txt")},
		{"workspace-symlink", filepath.Join(work, "escape", "secret.txt")},
		{"traversal", refs + "/../sibling/secret.txt"},
	} {
		read(item.id+"-read", item.p, "", true)
		add(item.id+"-write", "Write", map[string]any{"file_path": filepath.Join(filepath.Dir(item.p), "must-not-exist.txt"), "content": "must-not-change"}, "", true)
	}
	for _, item := range []struct{ id, p string }{
		{"sibling", filepath.Join(home, "sibling")},
		{"home", home},
		{"git", filepath.Join(refs, ".git")},
		{"credential", filepath.Join(refs, "nested", ".aws")},
		{"symlink", filepath.Join(refs, "escape")},
	} {
		add(item.id+"-glob", "Glob", map[string]any{"path": item.p, "pattern": "**/*"}, "", true)
		add(item.id+"-grep", "Grep", map[string]any{"path": item.p, "pattern": "proof-secret", "output_mode": "content"}, "", true)
	}
	if slices.Contains(o.Policy.ClaudeTools, "Bash") {
		interpreter, err := exec.LookPath("perl")
		if err != nil {
			return nil, &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch, Reason: ClaudeReferenceShellRequiresPerl}
		}
		program := claudeReferenceShellProgram(home, work, refs, sandbox.Write)
		script := filepath.Join(work, "reference-canary.pl")
		if err := os.WriteFile(script, []byte(program), 0600); err != nil {
			return nil, failure("claude_reference_fixture_unavailable")
		}
		files[script] = program
		add("reference-shell", "Bash", map[string]any{"command": sandboxprobe.ShellQuote(interpreter) + " " + sandboxprobe.ShellQuote(script), "description": "Disposable reference permission proof"}, "reference-shell-proof-ok", false)
	}
	if len(calls) == 0 {
		return nil, nil
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, failure("claude_reference_provider_unavailable")
	}
	var mu sync.Mutex
	observations := []claudeFileObservation{}
	next := 0
	proofErr := ""
	completed := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			refuseInference(w)
			return
		}
		var req struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(data, &req) != nil {
			refuseInference(w)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if next > 0 && next <= len(calls) {
			call := calls[next-1]
			text, isError, found := claudeFileResult(req.Messages, call.ID)
			if !found {
				refuseInference(w)
				return
			}
			denied := isError && (strings.Contains(strings.ToLower(text), "permission") || strings.Contains(strings.ToLower(text), "denied") || strings.Contains(strings.ToLower(text), "not allowed") || strings.Contains(strings.ToLower(text), "outside"))
			if call.Denied != denied || (!call.Denied && (isError || !strings.Contains(text, call.Want))) {
				proofErr = "claude_reference_" + call.ID + "_failed"
			}
			detail := ""
			if proofErr != "" {
				detail = text
			}
			for _, secret := range []string{"outside-proof-secret", "home-proof-secret", "git-proof-secret", "credential-proof-secret"} {
				if strings.Contains(text, secret) {
					proofErr = "claude_reference_private_content_exposed"
				}
			}
			if call.ID == "reference-recursive-glob" && (strings.Contains(text, ".git/config") || strings.Contains(text, "credentials")) {
				proofErr = "claude_reference_private_names_exposed"
			}
			observations = append(observations, claudeFileObservation{ID: call.ID, Tool: call.Tool, Denied: denied, Detail: detail})
		}
		if proofErr != "" || next == len(calls) {
			select {
			case completed <- struct{}{}:
			default:
			}
			refuseInference(w)
			return
		}
		writeClaudeFileCall(w, calls[next])
		next++
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	settled := make(chan error, 1)
	go func() {
		env := nativecli.Override(disposableEnvironment(o, home), "TMPDIR="+filepath.Join(root, "temp"), "ANTHROPIC_API_KEY=agent-harness-local-probe", "ANTHROPIC_BASE_URL=http://"+listener.Addr().String(), "MAX_RETRIES=0", "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1")
		id := newID()
		args := commandArgs(o, id, false, l)
		prompt, _ := json.Marshal(claudeUserFrame(id, "Capability check only."))
		cmd, p, err := process.Command(runCtx, o.Provider.CLI.Binary, args...)
		if err != nil {
			settled <- err
			return
		}
		cmd.Dir = work
		cmd.Env = env
		cmd.Stdin = strings.NewReader(string(prompt) + "\n")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		cmd.WaitDelay = 2 * time.Second
		runErr := p.Run()
		p.Close() // Settle descendants before publishing completion or deleting fixtures.
		settled <- runErr
	}()
	select {
	case <-completed:
		cancel()
		<-settled
	case <-ctx.Done():
		cancel()
		<-settled
		return nil, failure("claude_reference_probe_incomplete")
	case <-settled:
		return nil, failure("claude_reference_probe_incomplete")
	}
	mu.Lock()
	defer mu.Unlock()
	if ctx.Err() != nil || proofErr != "" || len(observations) != len(calls) {
		if proofErr == "" {
			proofErr = "claude_reference_probe_incomplete"
		}
		return observations, failure(proofErr)
	}
	var paths []string
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		before := files[p]
		expected := before
		if p == filepath.Join(work, "edit.txt") && sandbox.Write && slices.Contains(o.Policy.ClaudeTools, "Edit") {
			expected = "workspace-after"
		}
		after, err := os.ReadFile(p)
		if err != nil || sha256.Sum256(after) != sha256.Sum256([]byte(expected)) {
			return observations, failure("claude_reference_sentinel_changed")
		}
		beforeHash, afterHash := sha256.Sum256([]byte(before)), sha256.Sum256(after)
		label, _ := filepath.Rel(root, p)
		observations = append(observations, claudeFileObservation{ID: filepath.ToSlash(label), Tool: "SHA256", Before: hex.EncodeToString(beforeHash[:]), After: hex.EncodeToString(afterHash[:])})
	}
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(d.Name(), "must-not-exist.txt") {
			return failure("claude_reference_created_file")
		}
		return nil
	}); err != nil {
		return observations, failure("claude_reference_created_file")
	}
	if _, err := os.Stat(filepath.Join(refs, "new.txt")); !os.IsNotExist(err) {
		return observations, failure("claude_reference_created_file")
	}
	if sandbox.Write && slices.Contains(o.Policy.ClaudeTools, "Write") {
		data, err := os.ReadFile(filepath.Join(work, "new.txt"))
		if err != nil || string(data) != "workspace-created" {
			return observations, failure("claude_reference_workspace_write_failed")
		}
	}
	if slices.Contains(o.Policy.ClaudeTools, "Bash") && sandbox.Write {
		data, err := os.ReadFile(filepath.Join(work, "shell.txt"))
		if err != nil || string(data) != "workspace-shell-ok" {
			return observations, failure("claude_reference_workspace_shell_write_failed")
		}
	}
	return observations, nil
}

func claudeFileResult(messages []struct {
	Content json.RawMessage `json:"content"`
}, id string) (string, bool, bool) {
	for _, m := range messages {
		// Wire field names differ from Go's identifier spelling.
		var wire []struct {
			Type    string          `json:"type"`
			ID      string          `json:"tool_use_id"`
			Content json.RawMessage `json:"content"`
			Error   bool            `json:"is_error"`
		}
		if json.Unmarshal(m.Content, &wire) != nil {
			continue
		}
		for _, b := range wire {
			if b.Type != "tool_result" || b.ID != id {
				continue
			}
			var text string
			if json.Unmarshal(b.Content, &text) == nil {
				return text, b.Error, true
			}
			var parts []struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(b.Content, &parts) != nil {
				return "", true, true
			}
			for _, p := range parts {
				text += p.Text + "\n"
			}
			return text, b.Error, true
		}
	}
	return "", false, false
}
func writeClaudeFileCall(w http.ResponseWriter, call claudeFileCall) {
	input, _ := json.Marshal(call.Input)
	partial, _ := json.Marshal(string(input))
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	events := []struct{ name, data string }{
		{"message_start", `{"type":"message_start","message":{"id":"msg_file_canary","type":"message","role":"assistant","model":"harness-canary","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`},
		{"content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, call.ID, call.Tool)},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + string(partial) + `}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":1}}`},
		{"message_stop", `{"type":"message_stop"}`}}
	for _, e := range events {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, e.data)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// Indirect Perl opens prove the OS boundary, rather than the shell parser's
// named-path rules. This is the same runtime used by macOS interface canaries.
func claudeReferenceShellProgram(home, work, refs string, write bool) string {
	program := `use strict; use warnings; use Errno qw(EACCES EPERM);
my $visible=shift @ARGV;
open(my $input, "<", $visible) or die "reference read failed";
local $/; my $data=<$input>; close($input);
$data eq "reference-proof-visible" or die "reference content changed";
for (1..4) {
 my $path=shift @ARGV;
 if (open(my $f,"<",$path)) { close($f); die "private read escaped"; }
 ($! == EACCES || $! == EPERM) or die "private read not permission refusal";
}
for (1..3) {
 my $path=shift @ARGV;
 if (open(my $f,">",$path)) { close($f); die "reference write escaped"; }
 ($! == EACCES || $! == EPERM) or die "reference write not permission refusal";
}
my ($workspace,$write)=@ARGV;
if ($write) { open(my $out,">",$workspace) or die "workspace write failed"; print $out "workspace-shell-ok"; close($out); }
else { if (open(my $out,">",$workspace)) {close($out);die "readonly workspace write escaped";} ($! == EACCES || $! == EPERM) or die "workspace write not permission refusal"; }
print "reference-shell-proof-ok";
`
	var quoted []string
	for _, p := range []string{filepath.Join(refs, "read.txt"), filepath.Join(refs, ".git", "config"), filepath.Join(refs, "nested", ".aws", "credentials"), filepath.Join(home, "secret.txt"), filepath.Join(home, "sibling", "secret.txt"), filepath.Join(refs, "shell-must-not-exist.txt"), filepath.Join(home, "sibling", "shell-must-not-exist.txt"), filepath.Join(work, "escape", "shell-must-not-exist.txt"), filepath.Join(work, "shell.txt")} {
		quoted = append(quoted, "'"+strings.ReplaceAll(strings.ReplaceAll(p, "\\", "\\\\"), "'", "\\'")+"'")
	}
	flag := "0"
	if write {
		flag = "1"
	}
	return "@ARGV = (" + strings.Join(quoted, ",") + ", '" + flag + "');\n" + program
}
