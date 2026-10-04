package session

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/sandbox"
)

var errWorkbenchCommandUnknown = errors.New("command_outcome_unknown")

// Tests replace runner construction with a synthetic transport. Production
// always uses NewRunner's proof/configuration and state checks.
var newWorkbenchRunner = sandbox.NewRunner

func commandOptions(o Options) sandbox.Options {
	c := o.Workbench.Commands
	return sandbox.Options{WorkDir: o.WorkDir, RuntimeHome: o.RuntimeHome, Write: o.Workbench.Write, Read: c.Read, Env: c.Env, Loopback: c.Loopback, LoopbackPorts: c.LoopbackPorts, LoopbackControl: c.LoopbackControl, Timeout: c.Timeout, Background: o.Background}
}
func normalizeWorkbenchCommands(o Options) (Options, error) {
	if o.Workbench.Commands == nil {
		return o, nil
	}
	v, err := sandboxbridge.NormalizeWorkbench(commandOptions(o))
	if err != nil {
		return o, fromSandbox(err, o)
	}
	n := v.(sandbox.Options)
	o.WorkDir, o.RuntimeHome = n.WorkDir, n.RuntimeHome
	o.Workbench.Commands = &Commands{Read: n.Read, Env: n.Env, Loopback: n.Loopback, LoopbackPorts: n.LoopbackPorts, LoopbackControl: n.LoopbackControl, Timeout: n.Timeout}
	return o, nil
}
func proveWorkbench(ctx context.Context, o Options) error {
	if o.Workbench == nil || o.Workbench.Commands == nil {
		return nil
	}
	p, err := sandboxbridge.ProveWorkbench(ctx, commandOptions(o))
	if err == nil {
		o.Workbench.proof = p.(sandbox.Proof)
	}
	return fromSandbox(err, o)
}
func setupWorkbenchCommands(w *workbenchHost, o Options, id string) error {
	if o.Workbench.Commands == nil {
		return nil
	}
	r, err := newWorkbenchRunner(commandOptions(o), o.Workbench.proof, filepath.Join(o.RuntimeHome, "sessions", id), w.budget)
	if err == nil {
		w.commands = r
	}
	return fromSandbox(err, o)
}
func (w *workbenchHost) run(ctx context.Context, command, rel string, timeout time.Duration) (ToolResult, error) {
	result, err := w.commands.Execute(ctx, command, rel, timeout, nil)
	err = w.fromSandbox(err)
	var failure *TurnError
	if errors.As(err, &failure) {
		if failure.Code == CommandSandboxClosed {
			return w.commandInterruptedResult(result), context.Canceled
		}
		r := w.commandFailureResult(result, workbenchError(workbenchRunCommand, failure.Code, rel))
		if failure.Code == CommandOutcomeUnknown {
			return r, errWorkbenchCommandUnknown
		}
		return r, nil
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return w.commandInterruptedResult(result), err
		}
		if result.Stderr != "" {
			return w.commandFailureResult(result, ToolResult{Content: bound(err.Error(), 128), IsError: true}), err
		}
		return ToolResult{}, err
	}
	payload, _ := json.Marshal(result)
	return ToolResult{Content: string(payload)}, nil
}

// Failures without capture retain the existing plain-text contract. With
// capture, serialize a real payload instead of attempting to parse that text.
func (w *workbenchHost) commandFailureResult(result sandbox.CommandResult, failure ToolResult) ToolResult {
	if result.Stderr == "" {
		return failure
	}
	budget := w.budget
	if budget == 0 {
		budget = maxWorkbenchResult
	}
	// Reserve envelope/error space and worst-case JSON escaping. The actual
	// runner uses a smaller per-stream bound; this also bounds synthetic runners.
	stderr := bound(result.Stderr, (budget-1024)/6)
	payload, _ := json.Marshal(struct {
		Stderr    string `json:"stderr"`
		Truncated bool   `json:"truncated"`
		Error     string `json:"error"`
		ExitCode  int    `json:"exit_code"`
		TimedOut  bool   `json:"timed_out"`
	}{stderr, result.Truncated || stderr != result.Stderr, bound(failure.Content, 128), result.ExitCode, result.TimedOut})
	return ToolResult{Content: string(payload), IsError: true}
}
func (w *workbenchHost) commandInterruptedResult(result sandbox.CommandResult) ToolResult {
	if result.Stderr == "" {
		return ToolResult{}
	}
	return w.commandFailureResult(result, ToolResult{Content: cancelledToolText, IsError: true})
}

func workbenchCommandDefinition() ToolDefinition {
	return ToolDefinition{Name: workbenchRunCommand, Description: "Run a shell command inside the proved workspace sandbox. Output is bounded and a timeout stops the process tree.", Schema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "dir": map[string]any{"type": "string"}, "timeout_seconds": map[string]any{"type": "integer"}}, "required": []any{"command"}, "additionalProperties": false}}
}

func (w *workbenchHost) runCommand(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Command string
		Dir     string
		Seconds *int `json:"timeout_seconds"`
	}
	fail := func(code string) (ToolResult, error) { return workbenchError(workbenchRunCommand, code, in.Dir), nil }
	if json.Unmarshal(raw, &in) != nil || in.Command == "" || len(in.Command) > 64<<10 || strings.ContainsRune(in.Command, 0) {
		return fail(wbArgumentsInvalid)
	}
	timeout := w.commands.Timeout()
	if in.Seconds != nil {
		if *in.Seconds < 1 || *in.Seconds > int(timeout/time.Second) {
			return fail(wbArgumentsInvalid)
		}
		timeout = time.Duration(*in.Seconds) * time.Second
	}
	rel, code := resolveName(in.Dir, true)
	if code != "" {
		return fail(code)
	}
	// Reuse the symlink-free walk to validate a directory. The OS sandbox remains
	// the boundary if the pathname changes between validation and command start.
	code = w.checkDir(ctx, rel)
	if code != "" {
		return fail(code)
	}
	return w.run(ctx, in.Command, rel, timeout)
}

// NetworkControl reports the workbench command proof's DNS destination and source.
// It returns empty values when no selected-port proof exists; it contains no DNS data.
func (s *Session) NetworkControl() (address, source string) {
	if s.options.Workbench == nil {
		return "", ""
	}
	return s.options.Workbench.proof.NetworkControl()
}
