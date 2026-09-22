package process

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestHelper(t *testing.T) {
	if os.Getenv("HARNESS_PROCESS_HELPER") != "1" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "answer":
		fmt.Println("ready")
	case "wait":
		time.Sleep(30 * time.Second)
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestHelper$", "--", "wait")
		child.Env = os.Environ()
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(30 * time.Second)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func helper(t *testing.T, ctx context.Context, role string) (*Process, *bytes.Buffer) {
	t.Helper()
	cmd, p, err := Command(ctx, os.Args[0], "-test.run=^TestHelper$", "--", role)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	cmd.Env = append(os.Environ(), "HARNESS_PROCESS_HELPER=1")
	buf := &bytes.Buffer{}
	cmd.Stdout = buf
	cmd.WaitDelay = 2 * time.Second
	return p, buf
}

// Command's cancellation is the contained Stop, not exec's default of killing
// only the direct child. Calling it before Run proves which one it is: Stop
// refuses a later start, where the default would have had no process to kill.
func TestCommandWiresCancellationToContainment(t *testing.T) {
	cmd, p, err := Command(context.Background(), os.Args[0], "-test.run=^TestHelper$", "--", "answer")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if cmd.SysProcAttr == nil {
		t.Fatal("the command was returned without containment")
	}
	cmd.Env = append(os.Environ(), "HARNESS_PROCESS_HELPER=1")
	buf := &bytes.Buffer{}
	cmd.Stdout = buf
	if cmd.Cancel == nil || cmd.Cancel() != nil {
		t.Fatal("cancellation was not wired")
	}
	if p.Run() == nil || buf.Len() != 0 {
		t.Fatal("a cancelled contained command still started")
	}
}

func TestContainedOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, buf := helper(t, ctx, "answer")
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "ready" {
		t.Fatalf("output: %q", buf.String())
	}
}

func TestStopBeforeStart(t *testing.T) {
	p, buf := helper(t, context.Background(), "answer")
	p.Stop()
	if p.Run() == nil {
		t.Fatal("cancelled process started")
	}
	if buf.Len() != 0 {
		t.Fatal("cancelled process emitted output")
	}
}

func TestCancellationDrainsDescendantPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p, buf := helper(t, ctx, "parent")
	started := time.Now()
	if p.Run() == nil {
		t.Fatal("cancelled process succeeded")
	}
	if buf.Len() == 0 {
		t.Fatal("parent failed to start child")
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("descendant pipe prevented shutdown")
	}
}
