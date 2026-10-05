//go:build darwin || linux

package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

// Disposable scripts exercise PATH and interpreter selection without relocating
// Apple's platform binaries. Native execution is proved separately on Darwin.
// No installed Node or caller credentials are involved.
func workbenchExecutionCanary(ctx context.Context, l workbenchLayout, hidden string) (scriptOut string, errOut error) {
	step := ProofStepFixture
	defer func() {
		if errOut != nil {
			errOut = executionProofError(ctx, step, errOut)
		}
	}()
	if len(l.Read) == 0 {
		return "", stateError(StateUnusable)
	}
	bad := filepath.Join(hidden, "nvm", "bin")
	good := filepath.Join(l.Read[len(l.Read)-1], "toolchain")
	for _, dir := range []string{bad, good} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
	}
	for _, path := range []string{filepath.Join(bad, "node"), filepath.Join(good, "shell")} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexec /bin/sh \"$@\"\n"), 0700); err != nil {
			return "", err
		}
	}
	runtimeFile := filepath.Join(hidden, "runtime")
	if err := os.WriteFile(runtimeFile, []byte("disposable runtime"), 0600); err != nil {
		return "", err
	}
	// Prove that the script fixture and runtime work outside the boundary first.
	step = ProofStepOutside
	cmd, child, err := process.Command(ctx, filepath.Join(bad, "node"), "-c", "cat "+workbenchShellQuote(runtimeFile)+" >/dev/null && echo outside-control-ok")
	if err != nil {
		return "", err
	}
	cmd.Env = workbenchProbeEnvironment(l)
	cmd.WaitDelay = time.Second
	control := &workbenchOutput{limit: 1024}
	cmd.Stdout, cmd.Stderr = control, io.Discard
	err = child.Run()
	child.Close()
	if e := workbenchOutsideResult(ctx, control.text(), "outside-control-ok", err); e != nil {
		return "", e
	}
	step = ProofStepFixture
	if err := os.WriteFile(filepath.Join(good, "node"), []byte("#!/bin/sh\necho readable-node\n"), 0700); err != nil {
		return "", err
	}
	script := filepath.Join(l.Tmp, "env-node")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env node\necho outside-env-started\ncat "+workbenchShellQuote(runtimeFile)+"\n"), 0700); err != nil {
		return "", err
	}
	path, drops := filterWorkbenchPath(l, bad+":"+good+":/usr/bin:/bin")
	if len(drops) != 1 || drops[0].reason != "outside-read-set" {
		return "", stateError(StateUnusable)
	}
	q := workbenchShellQuote
	s := "export LC_ALL=C\nexport PATH=" + q(path) + "\n"
	s += q(filepath.Join(good, "shell")) + " -c 'echo readable-exec'\n"
	s += q(script) + "\n"
	// Print startup independently of exit status: a runtime-loading failure must
	// never masquerade as a successful execution refusal.
	s += q(filepath.Join(bad, "node")) + " -c " + q("echo outside-exec-started; cat "+q(filepath.Join(hidden, "runtime"))) + " 2>" + q(filepath.Join(l.Tmp, "exec-error")) + "\nstatus=$?\n"
	if runtime.GOOS == "darwin" {
		s += "[ \"$status\" = 126 ] && /usr/bin/grep -Eq 'Operation not permitted|Permission denied' " + q(filepath.Join(l.Tmp, "exec-error")) + " && echo exec-permission-refused\n"
	} else {
		s += "[ \"$status\" = 126 ] || [ \"$status\" = 127 ]; refused=$?\n[ \"$refused\" = 0 ] && /bin/grep -Eiq 'not found|No such file|Permission denied|Operation not permitted' " + q(filepath.Join(l.Tmp, "exec-error")) + " && echo exec-inaccessible-refused\n"
	}
	s += "echo execution-canary-ran\n"
	if runtime.GOOS == "linux" {
		binary, err := os.ReadFile("/bin/echo")
		if err != nil {
			return "", err
		}
		native := filepath.Join(bad, "native")
		if err := os.WriteFile(native, binary, 0700); err != nil {
			return "", err
		}
		cmd, child, err := process.Command(ctx, native, "outside-native-control")
		if err != nil {
			return "", err
		}
		out := &workbenchOutput{limit: 1024}
		cmd.Env = workbenchProbeEnvironment(l)
		cmd.Stdout, cmd.Stderr = out, io.Discard
		err = child.Run()
		child.Close()
		if e := workbenchOutsideResult(ctx, out.text(), "outside-native-control", err); e != nil {
			return "", e
		}
		s = q(native) + " outside-native-started 2>" + q(filepath.Join(l.Tmp, "native-error")) + "\nstatus=$?\n[ \"$status\" = 126 ] || [ \"$status\" = 127 ]; refused=$?\n[ \"$refused\" = 0 ] && /bin/grep -Eiq 'not found|No such file|Permission denied|Operation not permitted' " + q(filepath.Join(l.Tmp, "native-error")) + " && echo native-execution-refused\n" + s
	}
	return s, nil
}

func judgeWorkbenchExecution(output string) error {
	lines, last := workbenchCanaryLines(output)
	if lines["outside-exec-started"] || lines["outside-env-started"] || lines["outside-native-started"] {
		return workbenchCapability(CapabilitySandboxNotEnforced)
	}
	refusal := "exec-inaccessible-refused"
	if runtime.GOOS == "darwin" {
		refusal = "exec-permission-refused"
	}
	for _, required := range []string{"readable-exec", "readable-node", refusal} {
		if !lines[required] {
			return workbenchCapability(CapabilitySandboxUnavailable)
		}
	}
	if runtime.GOOS == "linux" && !lines["native-execution-refused"] {
		return workbenchCapability(CapabilitySandboxUnavailable)
	}
	if strings.TrimSpace(last) != "execution-canary-ran" {
		return workbenchCapability(CapabilitySandboxUnavailable)
	}
	return nil
}

func judgeWorkbenchNative(output string) error {
	lines, last := workbenchCanaryLines(output)
	if lines["outside-native-started"] {
		return workbenchCapability(CapabilitySandboxNotEnforced)
	}
	if !lines["native-positive"] || !lines["native-execution-refused"] || strings.TrimSpace(last) != "native-canary-ran" {
		return workbenchCapability(CapabilitySandboxUnavailable)
	}
	return nil
}

func executionProofError(ctx context.Context, step string, err error) *ProofError {
	p := &ProofError{Code: CapabilitySandboxUnavailable, Step: step}
	var old *ProofError
	if errors.As(err, &old) {
		p.Code = old.Code
		p.Tools = append([]string(nil), old.Tools...)
		if proofStep(old.Step) != "" {
			p.Step = old.Step
		}
	}
	// Preserve an observed enforcement breach through outer proof translation.
	if ctx.Err() != nil && p.Code != CapabilitySandboxNotEnforced {
		p.Code = CapabilityProbeTimeout
	}
	return p
}

func workbenchOutsideResult(ctx context.Context, got, want string, err error) error {
	if err != nil || strings.TrimSpace(got) != want {
		return executionProofError(ctx, ProofStepOutside, err)
	}
	return nil
}

func proveWorkbenchExecution(ctx context.Context, l workbenchLayout, hidden string, run func(context.Context, workbenchLayout, string) (string, error)) error {
	script, err := workbenchExecutionCanary(ctx, l, hidden)
	if err != nil {
		return err
	}
	out, err := run(ctx, l, script)
	if err != nil {
		return executionProofError(ctx, ProofStepLaunch, err)
	}
	if err := judgeWorkbenchExecution(out); err != nil {
		return executionProofError(ctx, ProofStepJudgment, err)
	}
	return nil
}
