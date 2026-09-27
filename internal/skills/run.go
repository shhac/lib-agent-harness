package skills

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

// Codes for preparing or running a script.
const (
	CodeScriptsNotPermitted = "scripts_not_permitted"
	CodeScriptNotExecutable = "script_not_executable"
	CodeArgsInvalid         = "script_args_invalid"
	CodeEnvInvalid          = "script_env_invalid"
	CodeTimeoutInvalid      = "script_timeout_invalid"
	CodeWorkDirRequired     = "work_dir_required"
	CodeWorkDirInvalid      = "work_dir_invalid"
	CodePlatformUnsupported = "script_platform_unsupported"
	CodeStartFailed         = "script_start_failed"
)

const (
	DefaultTimeout = time.Minute
	MaxTimeout     = 10 * time.Minute
	// MaxOutputBytes bounds each of stdout and stderr; the rest is discarded
	// and the output marked truncated.
	MaxOutputBytes = 64 << 10
	MaxArgs        = 64
	MaxArgBytes    = 4096
	maxEnvEntries  = 256
	waitDelay      = 2 * time.Second
)

// Command is one script invocation, checked and resolved.
type Command struct {
	// Script is the script's resolved absolute path, inside the skill.
	Script string
	// Args are the script's arguments, passed as an argument vector; no shell
	// ever reads them.
	Args    []string
	WorkDir string
	Env     []string
	Timeout time.Duration
}

// Output is what a finished script produced.
type Output struct {
	ExitCode        int
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	// TimedOut: the timeout ended the script and its process tree; ExitCode
	// is then -1.
	TimedOut bool
}

// Prepare checks a script invocation without running it: the skill permits
// scripts, the script is a regular file inside the skill, the arguments,
// environment and timeout are bounded, and a working directory was named.
// The environment is a fixed allowlist of the parent's (PATH, HOME, TMPDIR,
// LANG and LC_*) with env added on top.
func Prepare(s Skill, rel string, args []string, workDir string, env []string, timeout time.Duration) (Command, error) {
	if !s.Scripts {
		return Command{}, fail(CodeScriptsNotPermitted)
	}
	if workDir == "" {
		return Command{}, fail(CodeWorkDirRequired)
	}
	if strings.ContainsRune(workDir, 0) {
		return Command{}, fail(CodeWorkDirInvalid)
	}
	script, _, err := s.resolve(rel)
	if err != nil {
		return Command{}, err
	}
	if len(args) > MaxArgs {
		return Command{}, fail(CodeArgsInvalid)
	}
	for _, arg := range args {
		if len(arg) > MaxArgBytes || strings.ContainsRune(arg, 0) {
			return Command{}, fail(CodeArgsInvalid)
		}
	}
	switch {
	case timeout == 0:
		timeout = DefaultTimeout
	case timeout < 0 || timeout > MaxTimeout:
		return Command{}, fail(CodeTimeoutInvalid)
	}
	environment, err := Environment(os.Environ(), env)
	if err != nil {
		return Command{}, err
	}
	return Command{Script: script, Args: append([]string{}, args...), WorkDir: workDir, Env: environment, Timeout: timeout}, nil
}

// Environment keeps only the allowlisted entries of parent, then applies
// extra, whose entries replace a parent entry of the same name.
func Environment(parent, extra []string) ([]string, error) {
	if len(extra) > maxEnvEntries {
		return nil, fail(CodeEnvInvalid)
	}
	var order []string
	values := map[string]string{}
	set := func(key, entry string) {
		if _, ok := values[key]; !ok {
			order = append(order, key)
		}
		values[key] = entry
	}
	for _, entry := range parent {
		key, _, ok := strings.Cut(entry, "=")
		if ok && allowed(key) {
			set(key, entry)
		}
	}
	for _, entry := range extra {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(entry, 0) {
			return nil, fail(CodeEnvInvalid)
		}
		set(key, entry)
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, values[key])
	}
	return out, nil
}

func allowed(key string) bool {
	switch key {
	case "PATH", "HOME", "TMPDIR", "LANG":
		return true
	}
	return strings.HasPrefix(key, "LC_")
}

// Run executes a prepared command directly, never through a shell: the
// script must carry an execute permission, and the operating system reads
// any #! line itself. It runs in its own process tree, which the timeout or
// ctx ends as a whole. Scripts are not run on Windows, which has no execute
// permission to opt into; a caller there runs them in its own environment.
//
// A non-zero exit or a timeout is an Output, not an error; the error is
// ctx's own when ctx ended first, or a fixed code when nothing could start.
func Run(ctx context.Context, c Command) (Output, error) {
	if runtime.GOOS == "windows" {
		return Output{}, fail(CodePlatformUnsupported)
	}
	if info, err := os.Stat(c.WorkDir); !filepath.IsAbs(c.WorkDir) || err != nil || !info.IsDir() {
		return Output{}, fail(CodeWorkDirInvalid)
	}
	info, err := os.Stat(c.Script)
	if err != nil || !info.Mode().IsRegular() {
		return Output{}, fail(CodeFileNotRegular)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return Output{}, fail(CodeScriptNotExecutable)
	}
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	timed, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd, child, err := process.Command(timed, c.Script, c.Args...)
	if err != nil {
		return Output{}, fail(CodeStartFailed)
	}
	defer child.Close()
	stdout, stderr := &bounded{}, &bounded{}
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr, cmd.WaitDelay = c.WorkDir, c.Env, stdout, stderr, waitDelay
	runErr := child.Run()
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	out := Output{Stdout: string(stdout.data), Stderr: string(stderr.data), StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated}
	if timed.Err() != nil {
		out.ExitCode, out.TimedOut = -1, true
		return out, nil
	}
	var exit *exec.ExitError
	switch {
	case runErr == nil:
		return out, nil
	case errors.As(runErr, &exit):
		out.ExitCode = exit.ExitCode()
		return out, nil
	case errors.Is(runErr, exec.ErrWaitDelay) && cmd.ProcessState != nil:
		// The script exited; something it started still held its output.
		out.ExitCode = cmd.ProcessState.ExitCode()
		return out, nil
	}
	return Output{}, fail(CodeStartFailed)
}

// bounded keeps the first MaxOutputBytes written and discards the rest, so a
// chatty script is never stopped for its volume.
type bounded struct {
	data      []byte
	truncated bool
}

func (b *bounded) Write(p []byte) (int, error) {
	room := MaxOutputBytes - len(b.data)
	if len(p) > room {
		b.data = append(b.data, p[:max(room, 0)]...)
		b.truncated = true
		return len(p), nil
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
