//go:build windows

package completion

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The helper is this test binary, not Codex; no credentials or inference are used.
func TestWindowsCodexProcessHelper(t *testing.T) {
	if os.Getenv("AGENT_ASSISTANT_PROCESS_TEST") != "1" {
		return
	}
	role := os.Args[len(os.Args)-1]
	switch role {
	case "answer":
		fmt.Println("ready")
		os.Exit(0)
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestWindowsCodexProcessHelper$", "--", "child")
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		fmt.Println(child.Process.Pid)
	case "child":
	default:
		os.Exit(2)
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func TestWindowsCodexJobResumesContainedProcess(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "AGENT_ASSISTANT_PROCESS_TEST=1")
	output, err := runCodex(context.Background(), Config{Timeout: 5 * time.Second}, binary, []string{"-test.run=^TestWindowsCodexProcessHelper$", "--", "answer"}, t.TempDir(), env, "")
	if err != nil || strings.TrimSpace(string(output)) != "ready" {
		t.Fatalf("contained process did not resume: %s, %v", output, err)
	}
}

func TestWindowsCodexCancellationKillsDescendants(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "AGENT_ASSISTANT_PROCESS_TEST=1")
	output, err := runCodex(context.Background(), Config{Timeout: 2 * time.Second}, binary, []string{"-test.run=^TestWindowsCodexProcessHelper$", "--", "parent"}, t.TempDir(), env, "")
	if err == nil {
		t.Fatal("cancelled process unexpectedly succeeded")
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 32)
	if err != nil {
		t.Fatalf("helper did not start its descendant: %q", output)
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err == windows.ERROR_INVALID_PARAMETER {
		return
	} // Child already fully reaped.
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(child)
	state, err := windows.WaitForSingleObject(child, 1000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant survived job cancellation: state=%d err=%v", state, err)
	}
}
