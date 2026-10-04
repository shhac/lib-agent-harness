//go:build darwin || linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/skills"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/process"
)

func TestStartupBeforeNotifyRetainsDiagnostics(t *testing.T) {
	for _, fast := range []bool{false, true} {
		t.Run(fmt.Sprint(fast), func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range []string{"work", "state", "hidden"} {
				if err := os.Mkdir(filepath.Join(root, n), 0700); err != nil {
					t.Fatal(err)
				}
			}
			o := Options{WorkDir: filepath.Join(root, "work"), RuntimeHome: filepath.Join(root, "state"), Timeout: time.Second, Env: []string{"PATH=" + filepath.Join(root, "hidden") + ":/usr/bin:/bin"}}
			childDone, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			identity, err := workbenchBinaryFingerprintForTest("/bin/sh")
			if err != nil {
				t.Fatal(err)
			}
			config := commandConfig{options: o, proof: Proof{system: workbenchSystemDirs(), binary: "/bin/sh", identity: identity}, stateDir: o.RuntimeHome, outputBudget: MinResult}
			statusRead := make(chan bool, 1)
			config.statusRead = func(code int, known bool) { statusRead <- known && code == 0 }
			var fixtureCmd *exec.Cmd
			config.command = func(ctx context.Context, _ string, _ ...string) (*exec.Cmd, *process.Process, error) {
				status := ""
				if fast {
					status = "printf '0\\n' >&3; "
				}
				if fast && runtime.GOOS == "linux" {
					status = "printf '%s\\n' '{\"child-pid\":123}' '{\"exit-code\":0}' >&3; "
				}
				cmd, p, err := process.Command(ctx, "/bin/sh", "-c", status+"echo actual-output; i=0; while [ $i -lt 1000 ]; do echo actual-stderr >&2; i=$((i+1)); done")
				fixtureCmd = cmd
				return cmd, p, err
			}
			config.run = func(p *process.Process) error {
				p.Notify(nil) // priority refusal occurs before the normal notification
				err := p.Run()
				if runtime.GOOS == "linux" {
					// Without Notify, the parent still owns the status writer.
					// Linux parses through EOF, so close it before holding Run.
					_ = fixtureCmd.ExtraFiles[0].Close()
				}
				close(childDone)
				<-release
				if err != nil {
					return err
				}
				return syscall.EPERM
			}
			r, err := newCommandSandbox(config)
			if err != nil {
				t.Fatal(err)
			}
			ws, err := OpenWorkspace(Config{Root: o.WorkDir, SessionID: newID()})
			if err != nil {
				t.Fatal(err)
			}
			s := &Sandbox{ws: ws, commands: r, active: make(map[*StartedCommand]struct{})}
			defer func() {
				unblock()
				if err := s.Close(); err != nil {
					requireCommandCode(t, err, CommandCleanupUnknown)
				}
			}()
			type outcome struct {
				h   *StartedCommand
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				h, e := s.Start(context.Background(), CommandRequest{Command: "fixture"})
				done <- outcome{h, e}
			}()
			select {
			case <-childDone:
			case <-time.After(5 * time.Second):
				t.Fatal("child never settled")
			}
			if fast {
				select {
				case valid := <-statusRead:
					if !valid {
						t.Fatal("fast status was not parsed as success")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("fast status was not observed")
				}
			}
			unblock()
			var got outcome
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Start did not settle")
			}
			if got.h == nil {
				t.Fatalf("lost launched handle: %v", got.err)
			}
			if got.err != nil {
				requireCommandCode(t, got.err, CommandOutcomeUnknown)
			}
			for range 2 {
				result, e := got.h.Result()
				requireCommandCode(t, e, CommandOutcomeUnknown)
				if result.ExitCode != -1 || !result.Truncated || result.Stdout != "actual-output\n" || strings.Count(result.Stderr, "[harness PATH:") != 1 || len(result.Stderr) > MinResult {
					t.Fatalf("lost bounded diagnostics: %+v", result)
				}
			}
			got.h.Stop()
		})
	}
}

func TestWorkbenchPathPolicy(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]string{}
	for _, name := range []string{"work", "home", "tmp", "system", "read", "nvm/bin"} {
		dirs[name] = filepath.Join(root, name)
		if err := os.MkdirAll(dirs[name], 0700); err != nil {
			t.Fatal(err)
		}
	}
	l := workbenchLayout{Work: dirs["work"], Home: dirs["home"], Tmp: dirs["tmp"], System: []string{dirs["system"]}, Read: []string{dirs["read"]}}
	file := filepath.Join(root, "only-file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	l.System = append(l.System, file)
	for _, link := range []struct{ name, target string }{{"alias", dirs["read"]}, {"escape", dirs["nvm/bin"]}, {"dangling", filepath.Join(root, "missing")}, {"loop", filepath.Join(root, "loop")}} {
		if err := os.Symlink(link.target, filepath.Join(root, link.name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, source, want string
		reasons            []string
	}{
		{"order", dirs["nvm/bin"] + ":" + dirs["read"] + ":" + dirs["work"] + ":" + dirs["read"], dirs["read"] + ":" + dirs["work"] + ":" + dirs["read"], []string{"outside-read-set"}},
		{"coverage", dirs["system"] + ":" + dirs["home"] + ":" + dirs["tmp"], dirs["system"] + ":" + dirs["home"] + ":" + dirs["tmp"], nil},
		{"empty-relative", ":relative::", "", []string{"empty", "relative", "empty", "empty"}},
		{"file-and-ancestor", file + ":" + root, "", []string{"not-directory", "outside-read-set"}},
		{"alias", filepath.Join(root, "alias"), dirs["read"], nil},
		{"outside-alias", filepath.Join(root, "escape"), "", []string{"outside-read-set"}},
		{"broken", filepath.Join(root, "dangling") + ":" + filepath.Join(root, "loop"), "", []string{"unresolvable", "unresolvable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, drops := filterWorkbenchPath(l, tc.source)
			var reasons []string
			for _, d := range drops {
				reasons = append(reasons, d.reason)
			}
			if got != tc.want || !reflect.DeepEqual(reasons, tc.reasons) {
				t.Fatalf("%q %v", got, drops)
			}
		})
	}
	if runtime.GOOS == "darwin" && (workbenchLayout{System: []string{"/System"}}).readsDirectory("/System/Volumes/Data/Users") {
		t.Fatal("data volume admitted")
	}
	// A replaced alias is rechecked for each invocation.
	if err := os.Remove(filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dirs["nvm/bin"], filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if path, drops := filterWorkbenchPath(l, filepath.Join(root, "alias")); path != "" || len(drops) != 1 {
		t.Fatal(path, drops)
	}
	t.Run("reserved-temporary", func(t *testing.T) {
		testenv.RequireAtomicWrite(t)
		reserved := filepath.Join(dirs["work"], ".harness-workbench-"+strings.Repeat("0", 32)+"-"+strings.Repeat("0", 16)+".tmp")
		if err := os.Mkdir(reserved, 0700); err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS == "darwin" {
			if path, drops := filterWorkbenchPath(l, reserved); path != "" || len(drops) != 1 {
				t.Fatal("reserved data denial ignored", path, drops)
			}
		}
	})
	colon := filepath.Join(dirs["work"], "colon:target")
	if err := os.Mkdir(colon, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "colon-alias")
	if err := os.Symlink(colon, link); err != nil {
		t.Fatal(err)
	}
	if path, drops := filterWorkbenchPath(l, link); path != "" || len(drops) != 1 || drops[0].reason != "path-separator" {
		t.Fatal(path, drops)
	}
}

func TestWorkbenchPathEnvironmentAndFallback(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := workbenchLayout{Work: root, Tmp: root}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := l.anchorScratch(); err != nil {
		t.Fatal(err)
	}
	defer l.scratch.Close()
	for _, override := range [][]string{nil, {"PATH=" + root}, {"PATH=relative"}, {"PATH="}} {
		inherited := []string{"PATH=" + root, "OTHER=x"}
		merged, err := skills.Environment(inherited, override)
		if err != nil {
			t.Fatal(err)
		}
		original := append([]string{}, merged...)
		env, drops, err := workbenchCommandEnvironment(l, merged)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(merged, original) {
			t.Fatal("mutated source")
		}
		path := strings.TrimPrefix(env[len(env)-1], "PATH=")
		if len(override) == 0 || override[0] == "PATH="+root {
			if path != root || len(drops) != 0 {
				t.Fatal(env, drops)
			}
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 || !lexicallyWithin(root, path) {
			t.Fatal(path, err)
		}
		if err := os.WriteFile(filepath.Join(root, "cwd-tool"), []byte("#!/bin/sh\necho unsafe\n"), 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-c", "cwd-tool")
		cmd.Dir = root
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err == nil || strings.Contains(string(out), "unsafe") {
			t.Fatal(string(out), err)
		}
	}
	env, drops, err := workbenchCommandEnvironment(l, []string{"OTHER=x"})
	if err != nil || len(drops) != 1 || !strings.HasPrefix(env[len(env)-1], "PATH="+root+"/empty-path-") {
		t.Fatal(env, drops, err)
	}
	if _, _, err := workbenchCommandEnvironment(workbenchLayout{Tmp: filepath.Join(root, "missing")}, nil); err == nil {
		t.Fatal("fallback failure accepted")
	}
	for _, tmp := range []string{"", "relative", filepath.Join(root, "colon:tmp")} {
		if _, _, err := workbenchCommandEnvironment(workbenchLayout{Tmp: tmp}, nil); err == nil {
			t.Fatal("unrepresentable fallback accepted", tmp)
		}
	}
}

func TestWorkbenchPathReportBound(t *testing.T) {
	drops := []workbenchPathDrop{{"evil\n\"name", "relative"}}
	for i := 0; i < 1000; i++ {
		drops = append(drops, workbenchPathDrop{strings.Repeat("x", 100), "relative"})
	}
	for _, limit := range []int{256, 4096, 65536} {
		note := workbenchPathReport(drops, limit)
		if len(note) > limit || strings.Count(note, "[harness PATH:") != 1 || strings.Count(note, "\n") != 1 || !strings.Contains(note, "entries omitted") {
			t.Fatalf("invalid note %q", note)
		}
		b := workbenchOutput{limit: limit}
		_, _ = b.Write([]byte(note))
		_, _ = b.Write([]byte(strings.Repeat("o", limit)))
		_, truncated := b.finish()
		if !truncated {
			t.Fatal("note bypassed output budget")
		}
	}
}

func TestWorkbenchFallbackScratchReplacement(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "replaced", true: "concurrent"}[concurrent], func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			tmp, saved, outside := filepath.Join(base, "tmp"), filepath.Join(base, "saved"), filepath.Join(base, "outside")
			for _, p := range []string{tmp, outside} {
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			l := workbenchLayout{Tmp: tmp}
			if err := l.anchorScratch(); err != nil {
				t.Fatal(err)
			}
			defer l.scratch.Close()
			if err := os.Rename(tmp, saved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, tmp); err != nil {
				t.Fatal(err)
			}
			if _, _, err := workbenchCommandEnvironment(l, nil); err == nil {
				t.Fatal("changed scratch accepted")
			}
			if concurrent {
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 100 {
						if e := os.Remove(tmp); e != nil {
							t.Error(e)
							return
						}
						if e := os.Rename(saved, tmp); e != nil {
							t.Error(e)
							return
						}
						if e := os.Rename(tmp, saved); e != nil {
							t.Error(e)
							return
						}
						if e := os.Symlink(outside, tmp); e != nil {
							t.Error(e)
							return
						}
					}
				}()
				for range 100 {
					_, _, _ = workbenchCommandEnvironment(l, nil)
				}
				wg.Wait()
			}
			entries, e := os.ReadDir(outside)
			if e != nil || len(entries) != 0 {
				t.Fatalf("outside host write: %v %v", entries, e)
			}
		})
	}
}

func TestWorkbenchExecutionJudge(t *testing.T) {
	refusal := "exec-inaccessible-refused"
	if runtime.GOOS == "darwin" {
		refusal = "exec-permission-refused"
	}
	good := "readable-exec\nreadable-node\n" + refusal + "\nexecution-canary-ran\n"
	if runtime.GOOS == "linux" {
		good = "native-execution-refused\n" + good
	}
	if err := judgeWorkbenchExecution(good); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"readable-exec", "readable-node", refusal, "execution-canary-ran"} {
		if judgeWorkbenchExecution(strings.ReplaceAll(good, label+"\n", "")) == nil {
			t.Fatal("missing evidence accepted", label)
		}
	}
	if err := judgeWorkbenchExecution("outside-env-started\n" + good); err == nil || err.(*ProofError).Code != CapabilitySandboxNotEnforced {
		t.Fatal(err)
	}
	if err := judgeWorkbenchExecution("outside-exec-started\n" + good); err == nil || err.(*ProofError).Code != CapabilitySandboxNotEnforced {
		t.Fatal(err)
	}
	if err := judgeWorkbenchExecution("outside-native-started\n" + good); err == nil || err.(*ProofError).Code != CapabilitySandboxNotEnforced {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" && judgeWorkbenchExecution(strings.Replace(good, "native-execution-refused\n", "", 1)) == nil {
		t.Fatal("missing native refusal accepted")
	}
	native := "native-positive\nnative-execution-refused\nnative-canary-ran\n"
	if err := judgeWorkbenchNative(native); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"native-positive", "native-execution-refused", "native-canary-ran"} {
		if judgeWorkbenchNative(strings.Replace(native, marker+"\n", "", 1)) == nil {
			t.Fatal("missing native evidence accepted", marker)
		}
	}
	if err := judgeWorkbenchNative("outside-native-started\n" + native); err == nil || err.(*ProofError).Code != CapabilitySandboxNotEnforced {
		t.Fatal(err)
	}
}

func TestExecutionProofStepFailures(t *testing.T) {
	for _, which := range []string{"fixture", "outside", "launch", "judgment", "escape", "cancel", "timeout"} {
		t.Run(which, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			l := workbenchLayout{Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), Read: []string{filepath.Join(root, "read")}, System: workbenchSystemDirs()}
			for _, p := range []string{l.Work, l.Home, l.Tmp} {
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			state := filepath.Join(root, "command-state")
			if err := os.Mkdir(state, 0700); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			hidden := filepath.Join(root, "hidden")
			wantStep, wantCode := ProofStepLaunch, CapabilitySandboxUnavailable
			if which == "fixture" {
				if err := os.WriteFile(hidden, []byte("block"), 0600); err != nil {
					t.Fatal(err)
				}
				wantStep = ProofStepFixture
			}
			if which == "cancel" {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
				wantStep = ProofStepOutside
				wantCode = CapabilityProbeTimeout
			}
			if which == "outside" {
				wantStep = ProofStepOutside
			}
			if which == "timeout" {
				c, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				ctx = c
				wantStep = ProofStepOutside
				wantCode = CapabilityProbeTimeout
			}
			if which == "judgment" || which == "escape" {
				wantStep = ProofStepJudgment
			}
			if which == "escape" {
				wantCode = CapabilitySandboxNotEnforced
			}
			verified.mu.Lock()
			before := len(verified.seen)
			verified.mu.Unlock()
			err = proveWorkbenchExecution(ctx, l, hidden, func(context.Context, workbenchLayout, string) (string, error) {
				if which == "judgment" {
					return "readable-exec\n", nil
				}
				if which == "escape" {
					return "outside-native-started\n", nil
				}
				return "", errors.New("secret /owner/private raw-provider-text")
			})
			if which == "outside" {
				err = workbenchOutsideResult(ctx, "unexpected output", "outside-control-ok", errors.New("secret /owner/private raw-provider-text"))
			}
			var p *ProofError
			if !errors.As(err, &p) || p.Step != wantStep || p.Code != wantCode {
				t.Fatalf("%+v", err)
			}
			facts, ok := harness.ErrorFacts(err)
			if !ok || facts.Phase != "before_launch" || facts.ProofStep != wantStep || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), root) {
				t.Fatalf("unsafe facts: %+v %v", facts, err)
			}
			verified.mu.Lock()
			after := len(verified.seen)
			verified.mu.Unlock()
			if before != after {
				t.Fatal("failure recorded proof")
			}
			entries, e := os.ReadDir(state)
			if e != nil || len(entries) != 0 {
				t.Fatal("failure created command state", entries, e)
			}
		})
	}
	p := &ProofError{Code: CapabilitySandboxUnavailable, Step: "secret /owner/private"}
	if strings.Contains(p.Error(), "secret") || p.HarnessFacts().ProofStep != "" {
		t.Fatal("untrusted step exposed")
	}
}

func TestCommandSandboxReadablePathRunAndStart(t *testing.T) {
	opts := commandSandboxOptions(t, false)
	read, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts.Read = []string{read}
	opts.Env = []string{"PATH=" + hidden + ":" + read + ":/usr/bin:/bin"}
	if err := os.WriteFile(filepath.Join(read, "node"), []byte("#!/bin/sh\necho readable\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "node"), []byte("#!/bin/sh\necho unsafe\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(opts.WorkDir, "env-node")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env node\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s := openTestCommandSandbox(t, opts)
	t.Log("public Open succeeded after platform proof")
	for _, start := range []bool{false, true} {
		var result CommandResult
		request := CommandRequest{Command: workbenchShellQuote(script) + "; exit 7"}
		if start {
			started, err := s.Start(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started.Done():
			case <-time.After(30 * time.Second):
				t.Fatal("start did not settle")
			}
			result, err = started.Result()
			if err != nil {
				t.Fatal(err)
			}
			again, err := started.Result()
			if err != nil || !reflect.DeepEqual(result, again) {
				t.Fatal("unstable result", err)
			}
		} else {
			result, err = s.Run(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
		}
		if result.ExitCode != 7 || result.Stdout != "readable\n" || strings.Count(result.Stderr, "[harness PATH:") != 1 || !strings.Contains(result.Stderr, "outside-read-set") {
			t.Fatalf("%+v", result)
		}
		t.Logf("shared command start=%v result=%+v", start, result)
	}
	result, err := s.Run(context.Background(), CommandRequest{Command: workbenchShellQuote(filepath.Join(hidden, "node"))})
	if err != nil || result.ExitCode == 0 || strings.Contains(result.Stdout, "unsafe") {
		t.Fatal(result, err)
	}
	// Inherited PATH takes the identical route after environment merging. The
	// host PATH stays after the fixtures, so CI's pinned bubblewrap is still
	// found when the sandbox reopens.
	t.Setenv("PATH", hidden+":"+read+":/usr/bin:/bin:"+os.Getenv("PATH"))
	opts.Env = nil
	inherited := openTestCommandSandbox(t, opts)
	result, err = inherited.Run(context.Background(), CommandRequest{Command: workbenchShellQuote(script)})
	if err != nil || result.Stdout != "readable\n" || strings.Count(result.Stderr, "[harness PATH:") != 1 {
		t.Fatal(result, err)
	}
}
