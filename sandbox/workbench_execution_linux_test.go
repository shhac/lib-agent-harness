package sandbox

import (
	"context"
	"os"
	"os/exec"

	"path/filepath"
	"strings"
	"testing"
	"time"
)

func workbenchBinaryFingerprintForTest(binary string) (string, error) {
	return workbenchBinaryFingerprint(binary)
}

// Fixture viability is independent of bubblewrap/user-namespace availability.
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
	if _, err := workbenchExecutionCanary(ctx, l, filepath.Join(root, "hidden")); err != nil {
		t.Fatal(err)
	}
	t.Log("outside script/native controls succeeded")
	good := filepath.Join(l.Read[0], "toolchain")
	cmd := exec.CommandContext(ctx, filepath.Join(good, "shell"), "-c", "echo readable-exec; "+workbenchShellQuote(filepath.Join(l.Tmp, "env-node")))
	cmd.Env = append(workbenchProbeEnvironment(l), "PATH="+good+":/usr/bin:/bin")
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "readable-exec\nreadable-node\n" {
		t.Fatalf("fixture controls: %v %s", err, output)
	}
	t.Log("readable interpreter/env-shebang controls succeeded")
}

func TestWorkbenchExecutionCanaryReal(t *testing.T) {
	binary, _ := requireWorkbenchBwrapExecution(t)
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
	t.Log("outside native control succeeded")
	output, err := runLinuxProbe(ctx, binary, l, script, false)
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
	nativeRefusal, err := os.ReadFile(filepath.Join(l.Tmp, "native-error"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native direct platform refusal: %s", nativeRefusal)
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
	output, err = runLinuxProbe(ctx, binary, l, switched, false)
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
	// The mount builder still omits the hidden executable; no new binds.
	args, err := bwrapArgs(l)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "hidden") {
		t.Fatal("hidden executable mounted")
	}
}
