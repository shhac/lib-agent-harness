package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Darwin fingerprints sandbox-exec in the proof cache, not in runner creation.
func workbenchBinaryFingerprintForTest(string) (string, error) { return "", nil }

// Fixture startup is independently testable even when nested Seatbelt is
// unavailable. A signing/startup failure is a failure, never a sandbox skip.
func TestWorkbenchExecutionFixtureRunsAtDisposablePaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := workbenchLayout{Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), Read: []string{filepath.Join(root, "read")}, System: workbenchSystemDirs()}
	for _, dir := range []string{l.Work, l.Home, l.Tmp} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Construction runs the hidden script and reads its runtime.
	if _, err := workbenchExecutionCanary(ctx, l, filepath.Join(root, "hidden")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workbenchNativeCanary(ctx, l); err != nil {
		t.Fatal(err)
	}
	narrow, _, err := workbenchNativeCanary(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	for _, interpreter := range []string{"/bin/sh", "/bin/bash", "/bin/dash", "/bin/zsh"} {
		if !slices.Contains(narrow.Read, interpreter) {
			t.Fatalf("missing dispatcher grant: %s", interpreter)
		}
		profile := seatbeltProfile(narrow)
		for _, operation := range []string{"file-read*", "process-exec"} {
			if !strings.Contains(profile, "(allow "+operation+" (subpath \""+interpreter+"\"))") {
				t.Fatalf("missing narrow %s grant: %s", operation, interpreter)
			}
		}
	}
	if slices.Contains(narrow.System, "/bin") || slices.Contains(narrow.Read, "/bin/echo") {
		t.Fatal("native witness became readable")
	}
	t.Log("outside script/native controls succeeded")
	good := filepath.Join(l.Read[0], "toolchain")
	cmd := exec.CommandContext(ctx, filepath.Join(good, "shell"), "-c", "echo readable-exec; "+workbenchShellQuote(filepath.Join(l.Tmp, "env-node")))
	cmd.Env = append(workbenchProbeEnvironment(l), "PATH="+good+":/usr/bin:/bin")
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "readable-exec\nreadable-node\n" {
		t.Fatalf("disposable executable/interpreter controls: %v %s", err, output)
	}
	t.Log("readable interpreter/env-shebang controls succeeded")
}

func TestWorkbenchExecutionCanaryReal(t *testing.T) {
	requireWorkbenchSeatbeltExecution(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := workbenchLayout{Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), Read: []string{filepath.Join(root, "read")}, System: workbenchSystemDirs()}
	for _, dir := range []string{l.Work, l.Home, l.Tmp, l.Read[0]} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	script, err := workbenchExecutionCanary(ctx, l, filepath.Join(root, "hidden"))
	if err != nil {
		t.Fatal(err)
	}
	output, err := runWorkbenchProbe(ctx, l, script, false, workbenchInboundProbe{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(output)
	if err := judgeWorkbenchExecution(output); err != nil {
		t.Fatal(err)
	}
	directRefusal, err := os.ReadFile(filepath.Join(l.Tmp, "exec-error"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("direct platform refusal: %s", directRefusal)
	// Replace a directory after filtering. Canonicalization does not replace
	// the OS boundary when even a retained canonical pathname changes.
	alias := filepath.Join(l.Tmp, "switched")
	if err := os.Mkdir(alias, 0700); err != nil {
		t.Fatal(err)
	}
	retained, dropped := filterWorkbenchPath(l, alias)
	if retained != alias || len(dropped) != 0 {
		t.Fatal(retained, dropped)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "hidden", "nvm", "bin"), alias); err != nil {
		t.Fatal(err)
	}
	switched := strings.ReplaceAll(script, workbenchShellQuote(filepath.Join(root, "hidden", "nvm", "bin", "node")), workbenchShellQuote(filepath.Join(retained, "node")))
	output, err = runWorkbenchProbe(ctx, l, switched, false, workbenchInboundProbe{})
	if err != nil {
		t.Fatal(err)
	}
	if err := judgeWorkbenchExecution(output); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := os.ReadFile(filepath.Join(l.Tmp, "exec-error"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("replaced-directory refusal: %s", diagnostic)
	nativeLayout, nativeScript, err := workbenchNativeCanary(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("outside native control succeeded")
	output, err = runWorkbenchProbe(ctx, nativeLayout, nativeScript, false, workbenchInboundProbe{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native controls: %s", output)
	if err := judgeWorkbenchNative(output); err != nil {
		t.Fatal(err)
	}
	nativeRefusal, err := os.ReadFile(filepath.Join(l.Tmp, "native-error"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native direct permission refusal: %s", nativeRefusal)
	nativeAlias := filepath.Join(l.Tmp, "native-switched")
	if err := os.Mkdir(nativeAlias, 0700); err != nil {
		t.Fatal(err)
	}
	if retained, dropped := filterWorkbenchPath(nativeLayout, nativeAlias); retained != nativeAlias || len(dropped) != 0 {
		t.Fatal(retained, dropped)
	}
	if err := os.Remove(nativeAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin", nativeAlias); err != nil {
		t.Fatal(err)
	}
	switchedNative := strings.Replace(nativeScript, "/bin/echo outside-native-started", workbenchShellQuote(filepath.Join(nativeAlias, "echo"))+" outside-native-started", 1)
	output, err = runWorkbenchProbe(ctx, nativeLayout, switchedNative, false, workbenchInboundProbe{})
	if err != nil {
		t.Fatal(err)
	}
	if err := judgeWorkbenchNative(output); err != nil {
		t.Fatal(err)
	}
	nativeRefusal, err = os.ReadFile(filepath.Join(l.Tmp, "native-error"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native replaced-directory permission refusal: %s", nativeRefusal)
	// Reconstruct the former unrestricted-exec defect without relaxing reads.
	profile := seatbeltProfile(nativeLayout)
	var old strings.Builder
	for _, line := range strings.SplitAfter(profile, "\n") {
		if !strings.HasPrefix(line, "(allow process-exec ") {
			old.WriteString(line)
		}
	}
	old.WriteString("(allow process-exec)\n")
	baseline := nativeScript
	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", old.String(), "/bin/sh", "-c", baseline)
	cmd.Dir = l.Work
	cmd.Env = workbenchProbeEnvironment(l)
	control, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, control)
	}
	t.Logf("old execution policy: %s", control)
	if !strings.Contains(string(control), "outside-native-started") {
		t.Fatal("old policy did not reproduce startup")
	}
	if err := judgeWorkbenchNative(string(control)); err == nil || err.(*ProofError).Code != CapabilitySandboxNotEnforced {
		t.Fatal(err)
	}
}

// Exercise discovery, evidence publication and admission implementations
// used by Prove/Open, replacing only the disposable OS trial with a synthetic
// successful observation whose deferred cleanup cancels the request.
func TestProofCleanupCancellationPreventsAdmission(t *testing.T) {
	for _, admission := range []bool{false, true} {
		t.Run(map[bool]string{false: "prove", true: "open"}[admission], func(t *testing.T) {
			o := Options{WorkDir: t.TempDir(), RuntimeHome: t.TempDir()}
			if err := os.Chmod(o.RuntimeHome, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var key string
			calls := 0
			trial := func(ctx context.Context, n Options, system []string) error {
				calls++
				var err error
				key, _, err = workbenchProbeEvidence(n, system)
				if err != nil {
					return err
				}
				defer cancel() // successful probe cleanup, before evidence publication
				return nil
			}
			proof := func(ctx context.Context, n Options) (Proof, error) { return proveWorkbenchUsing(ctx, n, trial) }
			var err error
			if admission {
				var s *Sandbox
				s, err = openWithProof(ctx, o, proof)
				if s != nil {
					s.Close()
					t.Fatal("interrupted admission returned sandbox")
				}
			} else {
				p, e := proveOptions(ctx, o, proof)
				err = e
				if p.Binary() != "" {
					t.Fatal("interrupted proof returned evidence")
				}
			}
			var failure *ProofError
			if !errors.As(err, &failure) || failure.Code != CapabilityProbeTimeout || calls != 1 || key == "" {
				t.Fatalf("interrupted proof: calls=%d error=%v", calls, err)
			}
			if verified.holds(key) {
				t.Fatal("cleanup interruption cached evidence")
			}
			entries, e := os.ReadDir(o.RuntimeHome)
			if e != nil || len(entries) != 0 {
				t.Fatalf("command state created before proof: %v %v", entries, e)
			}
			if _, e := os.Stat(filepath.Join(o.WorkDir, ".agent-harness")); !os.IsNotExist(e) {
				t.Fatalf("workspace state prepared: %v", e)
			}
		})
	}
}

func TestCancelledCachedProofIsRefused(t *testing.T) {
	o := Options{WorkDir: t.TempDir(), RuntimeHome: t.TempDir()}
	if err := os.Chmod(o.RuntimeHome, 0700); err != nil {
		t.Fatal(err)
	}
	n, err := normalize(o, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := proveWorkbenchUsing(ctx, n, func(context.Context, Options, []string) error { return nil })
	if err != nil || p.Binary() == "" {
		t.Fatal(err)
	}
	cancel()
	p, err = proveWorkbenchUsing(ctx, n, func(context.Context, Options, []string) error { t.Fatal("cached trial called"); return nil })
	if err == nil || p.Binary() != "" {
		t.Fatal("cancelled cache hit returned proof")
	}
}

func TestFailedProofPreventsAdmission(t *testing.T) {
	for _, step := range []string{ProofStepFixture, ProofStepOutside, ProofStepLaunch, ProofStepJudgment} {
		t.Run(step, func(t *testing.T) {
			o := Options{WorkDir: t.TempDir(), RuntimeHome: t.TempDir()}
			if err := os.Chmod(o.RuntimeHome, 0700); err != nil {
				t.Fatal(err)
			}
			var key string
			trial := func(ctx context.Context, n Options, system []string) error {
				var err error
				key, _, err = workbenchProbeEvidence(n, system)
				if err != nil {
					return err
				}
				return executionProofError(ctx, step, errors.New("private raw fixture diagnostic"))
			}
			s, err := openWithProof(context.Background(), o, func(ctx context.Context, n Options) (Proof, error) { return proveWorkbenchUsing(ctx, n, trial) })
			var failure *ProofError
			if s != nil || !errors.As(err, &failure) || failure.Step != step {
				t.Fatalf("admission: %v %v", s, err)
			}
			if strings.Contains(err.Error(), "private raw") {
				t.Fatal("raw diagnostic leaked")
			}
			if key == "" || verified.holds(key) {
				t.Fatal("failed proof recorded")
			}
			entries, e := os.ReadDir(o.RuntimeHome)
			if e != nil || len(entries) != 0 {
				t.Fatalf("command state prepared: %v %v", entries, e)
			}
		})
	}
}
