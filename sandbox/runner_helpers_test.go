//go:build darwin || linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

type Commands struct {
	Loopback  bool
	Read, Env []string
	Timeout   time.Duration
}
type Workbench struct {
	Write                          bool
	Commands                       *Commands
	system                         []string
	commandBinary, commandIdentity string
	proof                          Proof
}
type ToolHost struct{}
type Restriction struct{ Tools ToolHost }
type testOptions struct {
	Provider             harness.Provider
	WorkDir, RuntimeHome string
	Workbench            *Workbench
	Restriction          *Restriction
	Background           bool
}
type ToolResult = Result
type workbenchHost struct {
	files    *Workspace
	commands *Runner
	budget   int
}

func nopHandler() any { return nil }
func privateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	return home
}
func workbenchOptions(t *testing.T, _ any) testOptions {
	return testOptions{WorkDir: t.TempDir(), RuntimeHome: privateHome(t), Workbench: &Workbench{}, Restriction: &Restriction{}}
}
func testCommandOptions(o testOptions) Options {
	c := o.Workbench.Commands
	if c == nil {
		c = &Commands{}
	}
	return Options{WorkDir: o.WorkDir, RuntimeHome: o.RuntimeHome, Write: o.Workbench.Write, Read: c.Read, Env: c.Env, Loopback: c.Loopback, Timeout: c.Timeout, Background: o.Background}
}
func normalizeTestCommands(o testOptions) (testOptions, error) {
	n, err := normalize(testCommandOptions(o), true)
	if err == nil {
		o.WorkDir, o.RuntimeHome = n.WorkDir, n.RuntimeHome
		o.Workbench.Commands = &Commands{Read: n.Read, Env: n.Env, Loopback: n.Loopback, Timeout: n.Timeout}
		o.Workbench.system = n.system
	}
	return o, err
}
func proveTestWorkbench(ctx context.Context, o testOptions) error {
	p, err := Prove(ctx, testCommandOptions(o))
	if err == nil {
		o.Workbench.proof = p
	}
	return err
}
func openWorkspace(o testOptions, id string) (*workbenchHost, error) {
	w, err := OpenWorkspace(Config{Root: o.WorkDir, SessionID: id})
	return &workbenchHost{files: w, budget: MaxResult}, err
}
func (w *workbenchHost) close()         { w.files.Close() }
func sessionDir(home, id string) string { return filepath.Join(home, "sessions", id) }
func setupWorkbenchCommands(w *workbenchHost, o testOptions, id string) error {
	n := testCommandOptions(o)
	p := o.Workbench.proof
	if len(p.system) == 0 {
		p = Proof{system: o.Workbench.system, binary: o.Workbench.commandBinary, identity: o.Workbench.commandIdentity, options: n}
	}
	r, err := newCommandSandbox(commandConfig{options: n, proof: p, stateDir: sessionDir(o.RuntimeHome, id), outputBudget: w.budget})
	if err == nil {
		w.commands = r
	}
	return err
}

var errWorkbenchCommandUnknown = errors.New("command_outcome_unknown")

func (r *Runner) run(ctx context.Context, cmd, rel string, timeout time.Duration) (Result, error) {
	value, err := r.Execute(ctx, cmd, rel, timeout, nil)
	var failure *CommandError
	if errors.As(err, &failure) {
		if failure.Code == CommandSandboxClosed {
			return Result{}, context.Canceled
		}
		out := ToolError("run_command", failure.Code, rel)
		if failure.Code == CommandOutcomeUnknown {
			return out, errWorkbenchCommandUnknown
		}
		return out, nil
	}
	if err != nil {
		return Result{}, err
	}
	payload, _ := json.Marshal(value)
	return Result{Content: string(payload)}, nil
}

func testCommandRunner(t *testing.T, o testOptions) *workbenchHost {
	t.Helper()
	var err error
	o, err = normalizeTestCommands(o)
	if err != nil {
		t.Fatal(err)
	}
	if err = proveTestWorkbench(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	id := newID()
	if err = os.MkdirAll(sessionDir(o.RuntimeHome, id), 0700); err != nil {
		t.Fatal(err)
	}
	w, err := openWorkspace(o, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = setupWorkbenchCommands(w, o, id); err != nil {
		w.close()
		t.Fatal(err)
	}
	return w
}
func closeTestCommandRunner(t *testing.T, w *workbenchHost) {
	t.Helper()
	if err := w.commands.Close(); err != nil {
		t.Error(err)
	}
	w.close()
}
