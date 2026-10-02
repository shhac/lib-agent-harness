package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/internal/skills"
)

var errWorkbenchCommandUnknown = errors.New("command_outcome_unknown")

type workbenchRunner struct {
	run     func(context.Context, string, string, time.Duration) (ToolResult, error)
	close   func() error
	timeout time.Duration
}

func normalizeWorkbenchCommands(o Options) (Options, error) {
	if o.Workbench.Commands == nil {
		return o, nil
	}
	c := *o.Workbench.Commands
	c.Read = slices.Clone(c.Read)
	c.Env = slices.Clone(c.Env)
	if c.Timeout == 0 {
		c.Timeout = 2 * time.Minute
	}
	if c.Timeout < 0 || c.Timeout > skills.MaxTimeout {
		return o, refuse(o, "commands", RefusedLimit, "command timeout must be between zero and ten minutes")
	}
	home, _ := os.UserHomeDir()
	if resolved, e := filepath.EvalSymlinks(home); e == nil {
		home = resolved
	}
	if home == "" || nested(o.WorkDir, home) {
		return o, refuse(o, "work_dir", RefusedWorkDir, "a command workspace must not contain the owner's home")
	}
	read, problem := sandboxReadDirs(c.Read)
	if problem != "" {
		return o, refuse(o, "commands", RefusedSandboxRead, problem)
	}
	c.Read = nil
	runtimeHome, e := filepath.EvalSymlinks(o.RuntimeHome)
	if e != nil {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "runtime home could not be resolved")
	}
	o.RuntimeHome = runtimeHome
	var err error
	o, err = normalizeWorkbenchSystem(o)
	if err != nil {
		return o, err
	}
	for _, dir := range read {
		if runtime.GOOS == "linux" && linuxCommandSocketRead(dir) {
			return o, refuse(o, "commands", RefusedSandboxRead, "command read paths must not overlap /run or contain /tmp: read-only binds expose host Unix sockets")
		}
		if info, e := os.Stat(dir); e != nil || !info.IsDir() {
			return o, refuse(o, "commands", RefusedSandboxRead, "command read paths must be existing directories")
		}
		for _, alias := range workbenchDataAliases(home) {
			if nested(dir, alias) {
				return o, refuse(o, "commands", RefusedSandboxRead, "command read paths must not reopen the owner's home through a data-volume alias")
			}
		}
		for _, alias := range workbenchDataAliases(runtimeHome) {
			if nested(dir, alias) || nested(alias, dir) {
				return o, refuse(o, "runtime_home", RefusedRuntimeHome, "command read paths must not overlap the runtime home")
			}
		}
		covered := false
		for _, system := range o.Workbench.system {
			if workbenchSystemContains(system, dir) {
				covered = true
				break
			}
		}
		if !covered {
			c.Read = append(c.Read, dir)
		}
	}
	slices.Sort(c.Read)
	c.Read = slices.Compact(c.Read)
	for _, entry := range c.Env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || strings.ContainsRune(entry, 0) || !(key == "PATH" || key == "LANG" || strings.HasPrefix(key, "LC_")) {
			return o, refuse(o, "commands", RefusedConflict, "Commands.Env accepts only PATH, LANG and LC_*; HOME and TMPDIR are private scratch")
		}
	}
	o.Workbench.Commands = &c
	return o, nil
}

// Paths have already been resolved, including /var/run aliases.
func linuxCommandSocketRead(dir string) bool {
	return lexicallyWithin(dir, "/run") || lexicallyWithin("/run", dir) || lexicallyWithin(dir, "/tmp")
}

func workbenchDataAliases(p string) []string {
	aliases := []string{p}
	const data = "/System/Volumes/Data"
	other := filepath.Join(data, p)
	if lexicallyWithin(data, p) {
		other = strings.TrimPrefix(p, data)
	}
	a, err := os.Stat(p)
	if err != nil {
		return aliases
	}
	b, err := os.Stat(other)
	if err == nil && os.SameFile(a, b) && other != p {
		aliases = append(aliases, other)
	}
	return aliases
}

func workbenchCommandDefinition() ToolDefinition {
	return ToolDefinition{Name: workbenchRunCommand, Description: "Run a shell command inside the proved workspace sandbox. Output is bounded and a timeout stops the process tree.", Schema: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "dir": map[string]any{"type": "string"}, "timeout_seconds": map[string]any{"type": "integer"}}, "required": []any{"command"}, "additionalProperties": false}}
}

func (w *workspace) runCommand(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	var in struct {
		Command string
		Dir     string
		Seconds *int `json:"timeout_seconds"`
	}
	fail := func(code string) (ToolResult, error) { return workbenchError(workbenchRunCommand, code, in.Dir), nil }
	if json.Unmarshal(raw, &in) != nil || in.Command == "" || len(in.Command) > 64<<10 || strings.ContainsRune(in.Command, 0) {
		return fail(wbArgumentsInvalid)
	}
	timeout := w.commands.timeout
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
	c, _, _, code := w.writeParent(ctx, rel+"/placeholder", false)
	if code != "" {
		return fail(code)
	}
	c.close()
	return w.commands.run(ctx, in.Command, rel, timeout)
}
