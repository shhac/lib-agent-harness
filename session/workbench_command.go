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

func commandOptions(o Options) sandbox.Options {
	c := o.Workbench.Commands
	return sandbox.Options{WorkDir: o.WorkDir, RuntimeHome: o.RuntimeHome, Write: o.Workbench.Write, Read: c.Read, Env: c.Env, Loopback: c.Loopback, Timeout: c.Timeout, Background: o.Background}
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
	o.Workbench.Commands = &Commands{Read: n.Read, Env: n.Env, Loopback: n.Loopback, Timeout: n.Timeout}
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
	r, err := sandbox.NewRunner(commandOptions(o), o.Workbench.proof, filepath.Join(o.RuntimeHome, "sessions", id), w.budget)
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
			return ToolResult{}, context.Canceled
		}
		r := workbenchError(workbenchRunCommand, failure.Code, rel)
		if failure.Code == CommandOutcomeUnknown {
			return r, errWorkbenchCommandUnknown
		}
		return r, nil
	}
	if err != nil {
		return ToolResult{}, err
	}
	payload, _ := json.Marshal(result)
	return ToolResult{Content: string(payload)}, nil
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
