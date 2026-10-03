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

type commandSandbox struct {
	execute func(context.Context, string, string, time.Duration, func()) (CommandResult, error)
	close   func() error
	timeout time.Duration
}

// commandConfig is shared by hosted and standalone execution. Preparation
// derives the proved layout, minimal environment, token and scratch from these
// normalized options; outputBudget keeps the hosted tool's historical bound.
type commandConfig struct {
	options      Options
	id           string
	outputBudget int
}

func setupWorkbenchCommands(w *workspace, o Options, id string) error {
	if o.Workbench.Commands == nil {
		return nil
	}
	commands, err := newCommandSandbox(commandConfig{options: o, id: id, outputBudget: w.budget})
	if err == nil {
		w.commands = commands
	}
	return err
}

// run preserves the hosted tool's existing JSON and error contract.
func (s *commandSandbox) run(ctx context.Context, command, rel string, timeout time.Duration) (ToolResult, error) {
	result, err := s.execute(ctx, command, rel, timeout, nil)
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
		if why := workbenchEnvRefusal(entry); why != "" {
			return o, refuse(o, "commands", RefusedConflict, "Commands.Env "+why)
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

// workbenchEnvRefusal says why a Commands.Env entry is refused, or "" when it
// is an ordinary setting the caller may pass to commands. What it refuses
// either belongs to the library, acts before the sandbox exists, runs ahead
// of the supervisor script, or would hand a credential to the model, which
// can print its environment.
func workbenchEnvRefusal(entry string) string {
	key, _, ok := strings.Cut(entry, "=")
	switch {
	case !ok || !workbenchEnvName(key) || strings.ContainsRune(entry, 0):
		return "entries must be NAME=value with a portable name"
	case key == "HOME" || key == "TMPDIR":
		return "cannot set HOME or TMPDIR: they name private scratch"
	case strings.HasPrefix(key, "AGENT_HARNESS_"):
		return "cannot set AGENT_HARNESS_* variables: they belong to the library"
	case strings.HasPrefix(key, "DYLD_") || strings.HasPrefix(key, "LD_"):
		return "cannot set dynamic loader variables: they act before the sandbox applies"
	case slices.Contains([]string{"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "IFS", "CDPATH"}, key):
		return "cannot set shell start-up variables: they run ahead of the command supervisor"
	case workbenchEnvCredential(key):
		return "cannot set credential-like variables: commands' output reaches the model"
	}
	return ""
}

// workbenchEnvName is a portable environment variable name, which also keeps
// out Bash's exported functions (BASH_FUNC_name%%).
func workbenchEnvName(key string) bool {
	for i, r := range key {
		letter := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return key != ""
}

// workbenchEnvCredential reports a name with a credential-like segment, such
// as OPENAI_API_KEY, GITHUB_TOKEN or SSH_AUTH_SOCK.
func workbenchEnvCredential(key string) bool {
	for _, segment := range strings.Split(strings.ToUpper(key), "_") {
		switch segment {
		case "KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "CREDENTIALS", "AUTH":
			return true
		}
	}
	return false
}
