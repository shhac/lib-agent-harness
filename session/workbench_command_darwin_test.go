package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/process"
)

func TestWorkbenchCommandsWithoutJobsLeaveNoSupervisors(t *testing.T) {
	requireWorkbenchSeatbelt(t)
	o := workbenchOptions(t, nopHandler())
	o.Workbench = &Workbench{Commands: &Commands{}}
	s := startAPI(t, o)
	defer closeAPI(t, s)
	data, err := os.ReadFile(filepath.Join(sessionDir(o.RuntimeHome, s.Ref().ID), "workbench-token.json"))
	if err != nil {
		t.Fatal(err)
	}
	var token workbenchToken
	if err := json.Unmarshal(data, &token); err != nil {
		t.Fatal(err)
	}
	for range 25 {
		r, err := s.api.workspace.run(context.Background(), "true", ".", time.Second)
		if err != nil || r.IsError {
			t.Fatalf("%+v %v", r, err)
		}
		if process.TokenPresent(token.Token, token.Since) {
			t.Fatal("no-job command retained a supervisor")
		}
	}
}

func TestWorkbenchCommandBackgroundCloseAndRecovery(t *testing.T) {
	requireWorkbenchSeatbelt(t)
	o := workbenchOptions(t, nopHandler())
	o.Workbench = &Workbench{Write: true, Commands: &Commands{}}
	s := startAPI(t, o)
	defer closeAPI(t, s)
	r, err := s.api.workspace.run(context.Background(), "sleep 60 >/dev/null 2>&1 & echo $! > pid", ".", time.Second)
	if err != nil || r.IsError {
		t.Fatalf("%+v %v", r, err)
	}
	data, err := os.ReadFile(filepath.Join(o.WorkDir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid <= 1 || syscall.Kill(pid, 0) != nil {
		t.Fatal("background process did not survive")
	}
	tokenData, err := os.ReadFile(filepath.Join(sessionDir(o.RuntimeHome, s.Ref().ID), "workbench-token.json"))
	if err != nil {
		t.Fatal(err)
	}
	var originalToken workbenchToken
	if err := json.Unmarshal(tokenData, &originalToken); err != nil {
		t.Fatal(err)
	}
	if !process.TokenPresent(originalToken.Token, originalToken.Since) {
		t.Fatal("live background marker not observable")
	}
	r, err = s.api.workspace.run(context.Background(), "cat pid", ".", time.Second)
	if err != nil || r.IsError || syscall.Kill(pid, 0) != nil {
		t.Fatal("background process did not survive next command")
	}
	// A second private runtime holds the durable crash snapshot. The original
	// launch remains alive until the fresh session sweeps the copied marker.
	ref := s.Ref()
	// The reference names the canonical home, as the library resolves it.
	crashed, err := filepath.EvalSymlinks(privateHome(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := sessionDir(crashed, ref.ID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"transcript.jsonl", "workbench-token.json"} {
		data, err := os.ReadFile(filepath.Join(sessionDir(o.RuntimeHome, ref.ID), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	o.RuntimeHome = crashed
	ref.Home = crashed
	resumed, err := Resume(context.Background(), o, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAPI(t, resumed)
	waitCommandGone(t, pid)
	if process.TokenPresent(originalToken.Token, originalToken.Since) {
		t.Fatal("crashed session's marker survived recovery")
	}
	r, err = resumed.api.workspace.run(context.Background(), "sleep 60 >/dev/null 2>&1 & echo $! > pid", ".", time.Second)
	if err != nil || r.IsError {
		t.Fatalf("%+v %v", r, err)
	}
	data, _ = os.ReadFile(filepath.Join(o.WorkDir, "pid"))
	pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	tokenData, err = os.ReadFile(filepath.Join(sessionDir(o.RuntimeHome, resumed.Ref().ID), "workbench-token.json"))
	if err != nil {
		t.Fatal(err)
	}
	var resumedToken workbenchToken
	if err := json.Unmarshal(tokenData, &resumedToken); err != nil {
		t.Fatal(err)
	}
	closeAPI(t, resumed)
	waitCommandGone(t, pid)
	if process.TokenPresent(resumedToken.Token, resumedToken.Since) {
		t.Fatal("closed session's marker survived")
	}
}

func waitCommandGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("process %d outlived session cleanup", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorkbenchMacOSEditAndRunSession(t *testing.T) {
	testenv.RequireLoopback(t) // Provider fixtures and canaries need a real loopback bind.
	requireWorkbenchSeatbelt(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int32
	listenerDone := make(chan struct{})
	go func() {
		defer close(listenerDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			conn.Close()
		}
	}()
	defer func() { listener.Close(); <-listenerDone }()
	port := listener.Addr().(*net.TCPAddr).Port
	// A reachable outside control prevents a disconnected host from passing.
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	deadline := time.Now().Add(time.Second)
	for connections.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if connections.Load() != 1 {
		t.Fatal("network positive control missing")
	}
	o := workbenchOptions(t, nopHandler())
	o.Workbench = &Workbench{Write: true, Commands: &Commands{}}
	writeFile(t, filepath.Join(o.WorkDir, "file"), "old")
	e := newEndpoint(t,
		answer("", scriptedCall{"edit", "edit_file", `{"path":"file","old":"old","new":"new"}`}),
		answer("", scriptedCall{"run", "run_command", fmt.Sprintf(`{"command":"cat file; echo result > result; if echo bad > ../escaped; then echo ESCAPED; fi; if nc -z -w 1 127.0.0.1 %d; then echo NETWORK; fi"}`, port)}),
		answer("", scriptedCall{"finish", "finish", `{}`}),
	)
	o.Provider.API.BaseURL = e.url
	s := startAPI(t, o)
	defer closeAPI(t, s)
	done := runAPITurnToEnd(t, s, "Implement and QA")
	if done.err != nil || done.result.Status != "completed" {
		t.Fatalf("%+v %v", done.result, done.err)
	}
	data, err := os.ReadFile(filepath.Join(o.WorkDir, "result"))
	if err != nil || string(data) != "result\n" {
		t.Fatalf("%s %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(o.WorkDir, "file"))
	if err != nil || string(data) != "new" {
		t.Fatalf("edited content %q: %v", data, err)
	}
	if _, err = os.Stat(filepath.Join(o.WorkDir, "..", "escaped")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside write: %v", err)
	}
	commandSeen := false
	for _, ev := range done.events {
		if ev.Tool == workbenchRunCommand && ev.Kind == "tool_completed" {
			var result struct {
				Stdout string `json:"stdout"`
			}
			if json.Unmarshal([]byte(ev.Output), &result) != nil || result.Stdout != "new" {
				t.Fatalf("command output %q", ev.Output)
			}
			commandSeen = true
		}
		if ev.Tool == workbenchRunCommand && ev.Kind == "tool_completed" && (ev.Status != "completed" || strings.Contains(ev.Output, "ESCAPED") || strings.Contains(ev.Output, "NETWORK")) {
			t.Fatal(ev)
		}
	}
	if !commandSeen || connections.Load() != 1 {
		t.Fatal("command observation absent or network escaped")
	}
	if harness.Support(harness.OpenAICompatible, harness.Session, harness.Sandbox).Availability != harness.Unknown {
		t.Fatal("unproved native claim")
	}
}

// The caller's ordinary settings reach every command, beside the private
// scratch HOME and TMPDIR the library always sets.
