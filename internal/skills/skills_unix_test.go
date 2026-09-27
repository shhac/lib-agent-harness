//go:build !windows

package skills

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
)

func TestSymbolicLinksCannotLeaveTheSkill(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := writeSkill(t, "---\nname: demo\ndescription: x\n---\n")
	for link, target := range map[string]string{
		"escape.txt": filepath.Join(outside, "secret.txt"),
		"escape-dir": outside,
		"relative":   "../" + filepath.Base(outside) + "/secret.txt",
		"inside.md":  Manifest,
	} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load([]harness.Skill{{Name: "demo", Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	for rel, code := range map[string]string{
		"escape.txt":            CodeOutsideSkill,
		"escape-dir/secret.txt": CodeOutsideSkill,
		"relative":              CodeOutsideSkill,
		"pipe":                  CodeFileNotRegular,
	} {
		_, err := loaded[0].Read(rel)
		requireCode(t, err, code)
	}
	if text, err := loaded[0].Read("inside.md"); err != nil || !strings.HasPrefix(text, "---") {
		t.Fatalf("a link inside the skill: %v", err)
	}

	// A linked skill directory is its target; a linked SKILL.md must not leave it.
	linkedRoot := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(dir, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if loaded, err := Load([]harness.Skill{{Name: "demo", Dir: linkedRoot}}); err != nil || loaded[0].Root() == linkedRoot {
		t.Fatalf("linked directory: %v", err)
	}
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte("---\nname: demo\ndescription: secret\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "SKILL.md"), filepath.Join(foreign, Manifest)); err != nil {
		t.Fatal(err)
	}
	_, err = Load([]harness.Skill{{Name: "demo", Dir: foreign}})
	requireCode(t, err, CodeManifestInvalid)
}

// scriptSkill is a skill holding executable scripts, permitted to run them.
func scriptSkill(t *testing.T, scripts map[string]string) Skill {
	t.Helper()
	dir := writeSkill(t, "---\nname: tools\ndescription: Scripts.\n---\n")
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := Load([]harness.Skill{{Name: "tools", Dir: dir, Scripts: true}})
	if err != nil {
		t.Fatal(err)
	}
	return loaded[0]
}

func run(t *testing.T, skill Skill, script string, args []string, env []string, timeout time.Duration) (Output, string) {
	t.Helper()
	work := t.TempDir()
	command, err := Prepare(skill, script, args, work, env, timeout)
	if err != nil {
		t.Fatal(err)
	}
	output, err := Run(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	return output, work
}

func TestRunPassesAnArgumentVectorNeverAShellString(t *testing.T) {
	skill := scriptSkill(t, map[string]string{"echo.sh": "#!/bin/sh\nprintf '%s\\n' \"$@\"\npwd\nexit 3\n"})
	injection := "$(touch pwned); `touch pwned2`; echo hi"
	output, work := run(t, skill, "echo.sh", []string{injection, "two words"}, nil, 0)
	resolvedWork, _ := filepath.EvalSymlinks(work)
	if output.ExitCode != 3 || output.TimedOut || output.Stdout != injection+"\ntwo words\n"+resolvedWork+"\n" {
		t.Fatalf("%+v", output)
	}
	for _, name := range []string{"pwned", "pwned2"} {
		if _, err := os.Stat(filepath.Join(work, name)); err == nil {
			t.Fatal("an argument was interpreted by a shell")
		}
	}
}

func TestRunEnvironmentIsAnAllowlist(t *testing.T) {
	t.Setenv("HARNESS_SECRET_TOKEN", "secret")
	t.Setenv("LC_TEST_VALUE", "kept")
	skill := scriptSkill(t, map[string]string{"env.sh": "#!/bin/sh\nenv\n"})
	output, _ := run(t, skill, "env.sh", nil, []string{"CALLER_VALUE=given", "LC_TEST_VALUE=replaced"}, 0)
	lines := strings.Split(strings.TrimSpace(output.Stdout), "\n")
	got := map[string]string{}
	for _, line := range lines {
		key, value, _ := strings.Cut(line, "=")
		got[key] = value
	}
	if strings.Contains(output.Stdout, "secret") || got["CALLER_VALUE"] != "given" || got["LC_TEST_VALUE"] != "replaced" || got["PATH"] == "" {
		t.Fatalf("environment %v", got)
	}
	for key := range got {
		if !allowed(key) && key != "CALLER_VALUE" && key != "PWD" && key != "SHLVL" && key != "_" {
			t.Errorf("unexpected variable %s", key)
		}
	}
}

func TestRunTimeoutEndsTheProcessTree(t *testing.T) {
	skill := scriptSkill(t, map[string]string{"hang.sh": "#!/bin/sh\nsleep 30 &\necho $! > child.pid\necho started\nwait\n"})
	started := time.Now()
	output, work := run(t, skill, "hang.sh", nil, nil, 2*time.Second)
	if !output.TimedOut || output.ExitCode != -1 || time.Since(started) > 10*time.Second {
		t.Fatalf("%+v after %s", output, time.Since(started))
	}
	raw, err := os.ReadFile(filepath.Join(work, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("a descendant outlived the timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunOutputIsBounded(t *testing.T) {
	skill := scriptSkill(t, map[string]string{"loud.sh": "#!/bin/sh\nhead -c 200000 /dev/zero | tr '\\0' o\nhead -c 100 /dev/zero | tr '\\0' e >&2\n"})
	output, _ := run(t, skill, "loud.sh", nil, nil, 0)
	if output.ExitCode != 0 || len(output.Stdout) != MaxOutputBytes || !output.StdoutTruncated || len(output.Stderr) != 100 || output.StderrTruncated {
		t.Fatalf("stdout %d %v stderr %d %v", len(output.Stdout), output.StdoutTruncated, len(output.Stderr), output.StderrTruncated)
	}
}

func TestPrepareAndRunRefusals(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "run.sh"), []byte("#!/bin/sh\necho secret\n"), 0700); err != nil {
		t.Fatal(err)
	}
	skill := scriptSkill(t, map[string]string{"ok.sh": "#!/bin/sh\necho ok\n"})
	if err := os.WriteFile(filepath.Join(skill.Root(), "plain.txt"), []byte("echo secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "run.sh"), filepath.Join(skill.Root(), "escape.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill.Root(), "noshebang"), []byte("\x7fELF-not-really"), 0700); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	unpermitted := skill
	unpermitted.Scripts = false
	for name, tc := range map[string]struct {
		skill   Skill
		script  string
		args    []string
		work    string
		env     []string
		timeout time.Duration
		code    string
	}{
		"not permitted":     {unpermitted, "ok.sh", nil, work, nil, 0, CodeScriptsNotPermitted},
		"no work dir":       {skill, "ok.sh", nil, "", nil, 0, CodeWorkDirRequired},
		"parent path":       {skill, "../run.sh", nil, work, nil, 0, CodeOutsideSkill},
		"symlink escape":    {skill, "escape.sh", nil, work, nil, 0, CodeOutsideSkill},
		"directory":         {skill, ".", nil, work, nil, 0, CodePathInvalid},
		"missing":           {skill, "none.sh", nil, work, nil, 0, CodeFileNotFound},
		"too many args":     {skill, "ok.sh", make([]string, MaxArgs+1), work, nil, 0, CodeArgsInvalid},
		"nul arg":           {skill, "ok.sh", []string{"a\x00b"}, work, nil, 0, CodeArgsInvalid},
		"long arg":          {skill, "ok.sh", []string{strings.Repeat("a", MaxArgBytes+1)}, work, nil, 0, CodeArgsInvalid},
		"malformed env":     {skill, "ok.sh", nil, work, []string{"NOEQUALS"}, 0, CodeEnvInvalid},
		"empty env key":     {skill, "ok.sh", nil, work, []string{"=x"}, 0, CodeEnvInvalid},
		"negative timeout":  {skill, "ok.sh", nil, work, nil, -time.Second, CodeTimeoutInvalid},
		"excessive timeout": {skill, "ok.sh", nil, work, nil, MaxTimeout + time.Second, CodeTimeoutInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Prepare(tc.skill, tc.script, tc.args, tc.work, tc.env, tc.timeout)
			requireCode(t, err, tc.code)
		})
	}
	for name, tc := range map[string]struct {
		script, work, code string
	}{
		"not executable":   {"plain.txt", work, CodeScriptNotExecutable},
		"relative workdir": {"ok.sh", "relative", CodeWorkDirInvalid},
		"missing workdir":  {"ok.sh", filepath.Join(work, "missing"), CodeWorkDirInvalid},
		"not a program":    {"noshebang", work, CodeStartFailed},
	} {
		t.Run(name, func(t *testing.T) {
			command, err := Prepare(skill, tc.script, nil, tc.work, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Run(context.Background(), command)
			requireCode(t, err, tc.code)
		})
	}
	command, err := Prepare(skill, "ok.sh", nil, work, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, command); err != context.Canceled {
		t.Fatalf("a cancelled context ran the script: %v", err)
	}
}
