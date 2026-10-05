package sandbox

import (
	"context"
	"errors"
	"fmt"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProcessInspectionCanaryReal(t *testing.T) {
	requireWorkbenchBwrap(t)
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%t", background), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			root := t.TempDir()
			l := workbenchLayout{Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), System: workbenchSystemDirs()}
			for _, path := range []string{l.Work, l.Home, l.Tmp} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			binary, _, err := checkBwrap(ctx, l)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			var transcript string
			err = proveProcessInspectionDiagnosed(ctx, Options{Background: background}, l, binary, runInspectionProbe, nil, func(output, marker string) { transcript = inspectionDiagnostic(output, marker) })
			cancel()
			if err != nil {
				t.Logf("control=%s transcript:\n%s", inspectionControl(transcript), transcript)
				t.Fatalf("background=%t: %v", background, err)
			}
		})
	}
}

func TestProcessInspectionCancelledLiveChildReal(t *testing.T) {
	requireWorkbenchBwrap(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	o := commandSandboxOptions(t, false)
	o.ProcessInspection = true
	l := workbenchLayout{Work: o.WorkDir, Home: t.TempDir(), Tmp: t.TempDir(), System: workbenchSystemDirs()}
	binary, _, err := checkBwrap(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	childReady := false
	fixturePID := 0
	run := func(ctx context.Context, binary string, l workbenchLayout, script string, background bool) (string, error) {
		return runInspectionProbeObserved(ctx, binary, l, script, background, func() { childReady = true; cancel() })
	}
	_, err = openWithProof(ctx, o, func(ctx context.Context, o Options) (Proof, error) {
		return Proof{}, proveProcessInspectionUsing(ctx, o, l, binary, run, func(stage string, pid int) { fixturePID = pid })
	})
	var proof *ProofError
	if !childReady || !errors.As(err, &proof) || proof.Code != CapabilityProbeTimeout || proof.Step != ProofStepProcessInspection {
		t.Fatalf("ready=%t %v", childReady, err)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", fixturePID)); !os.IsNotExist(err) {
		t.Fatalf("fixture not reaped: %v", err)
	}
	// runInspectionProbe settles both bwrap status and EOF on the child's pipe.
	// sleep inherits that writer: actual drain proves it no longer holds it.
	entries, err := os.ReadDir(o.RuntimeHome)
	if err != nil || len(entries) != 0 {
		t.Fatalf("command state prepared: %v %v", entries, err)
	}
}

func TestProcessInspectionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := proveProcessInspection(ctx, Options{}, workbenchLayout{}, "unused")
	var p *ProofError
	if !errors.As(err, &p) || p.Code != CapabilityProbeTimeout || p.Step != ProofStepProcessInspection {
		t.Fatalf("%v", err)
	}
}

func TestProcessInspectionCancelledStagesLeaveNoState(t *testing.T) {
	testenv.RequireProcessGroup(t)
	for _, stage := range []string{"fixture", "launch", "inside", "judgment", "judgment-leak"} {
		t.Run(stage, func(t *testing.T) {
			o := commandSandboxOptions(t, false)
			o.ProcessInspection = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fixturePID := 0
			run := func(context.Context, string, workbenchLayout, string, bool) (string, error) {
				if stage == "inside" {
					cancel()
				}
				if stage == "judgment-leak" {
					data, e := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", fixturePID))
					if e != nil {
						t.Fatal(e)
					}
					args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
					return args[len(args)-1], nil
				}
				return "host 3 2 5 123\nexpected-nice 5\nexpected-group 2\npositive 3 2 5 S\nstat 3 2 5 S 123\ncommand Mon Oct 5 12:00:00 2026 /bin/sleep 30\nprivate-namespace\nnegative-invisible\ninspection-ran\n", nil
			}
			observe := func(current string, pid int) {
				fixturePID = pid
				if current == stage || (stage == "judgment-leak" && current == "judgment") {
					cancel()
				}
			}
			_, err := openWithProof(ctx, o, func(ctx context.Context, o Options) (Proof, error) {
				return Proof{}, proveProcessInspectionUsing(ctx, o, workbenchLayout{}, "synthetic", run, observe)
			})
			var p *ProofError
			expectedCode := CapabilityProbeTimeout
			if stage == "judgment-leak" {
				expectedCode = CapabilitySandboxNotEnforced
			}
			if !errors.As(err, &p) || p.Code != expectedCode || p.Step != ProofStepProcessInspection {
				t.Fatalf("%v", err)
			}
			if fixturePID <= 0 {
				t.Fatal("fixture did not start")
			}
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", fixturePID)); !os.IsNotExist(err) {
				t.Fatalf("fixture not reaped: %v", err)
			}
			entries, err := os.ReadDir(o.RuntimeHome)
			if err != nil || len(entries) != 0 {
				t.Fatalf("command state prepared: %v %v", entries, err)
			}
		})
	}
}

func TestProcessInspectionProofIdentity(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "bwrap")
	if err := os.WriteFile(binary, []byte("synthetic binary"), 0700); err != nil {
		t.Fatal(err)
	}
	o := Options{WorkDir: filepath.Join(root, "work"), RuntimeHome: filepath.Join(root, "home")}
	base, err := workbenchLinuxProbeKey(o, binary, "0.9.0", workbenchLinuxWitness{})
	if err != nil {
		t.Fatal(err)
	}
	o.ProcessInspection = true
	added, err := workbenchLinuxProbeKey(o, binary, "0.9.0", workbenchLinuxWitness{})
	if err != nil {
		t.Fatal(err)
	}
	if added == base {
		t.Fatal("legacy evidence can certify inspection")
	}
	o.ProcessInspection = false
	again, err := workbenchLinuxProbeKey(o, binary, "0.9.0", workbenchLinuxWitness{})
	if err != nil || again != base {
		t.Fatalf("zero-value key changed: %v", err)
	}
}

func TestProcessInspectionSeparateCommandsReal(t *testing.T) {
	requireWorkbenchBwrap(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o := commandSandboxOptions(t, false)
	o.ProcessInspection = true
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.WorkDir, "inspection-helper"), data, 0700); err != nil {
		t.Fatal(err)
	}
	s := openInspectionDiagnosed(t, ctx, o)
	for _, otherHome := range []bool{false, true} {
		target := s
		if otherHome {
			other := o
			other.RuntimeHome = t.TempDir()
			if err := os.Chmod(other.RuntimeHome, 0700); err != nil {
				t.Fatal(err)
			}
			target = openInspectionDiagnosed(t, ctx, other)
		}
		marker := fmt.Sprintf("inspection-sibling-%d-%t", os.Getpid(), otherHome)
		h, err := target.Start(ctx, CommandRequest{Command: "export INSPECTION_SIBLING=" + marker + "; /bin/sleep 30 & wait"})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Stop()
		// Find only our controlled live shell. Its host PID must remain invisible
		// to a separate Run, even when both share the same Open and RuntimeHome.
		pid := 0
		entries, err := os.ReadDir("/proc")
		if err != nil {
			h.Stop()
			t.Fatal(err)
		}
		for _, entry := range entries {
			n, e := strconv.Atoi(entry.Name())
			if e != nil {
				continue
			}
			data, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
			if strings.Contains(string(data), marker) {
				pid = n
				break
			}
		}
		if pid == 0 {
			h.Stop()
			t.Fatal("live controlled sibling not found")
		}
		ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
		if err != nil {
			h.Stop()
			t.Fatal(err)
		}
		result, err := s.Run(ctx, CommandRequest{Command: inspectionScript(pid, ns)})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("%+v %v", result, err)
		}
		if strings.Contains(result.Stdout, marker) {
			t.Fatal("sibling arguments or environment leaked")
		}
		if err := judgeProcessInspection(result.Stdout, false); err != nil {
			t.Logf("control=%s transcript:\n%s", inspectionControl(result.Stdout), inspectionDiagnostic(result.Stdout, marker))
			t.Fatal(err)
		}
		result, err = s.Run(ctx, CommandRequest{Command: fmt.Sprintf("INSPECTION_TARGET=%d INSPECTION_HOST_NS=%s ./inspection-helper -test.run=^TestProcessInspectionProcHelper$", pid, workbenchShellQuote(ns))})
		if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "proc-interfaces-proved") {
			t.Fatalf("%+v %v", result, err)
		}
		h.Stop()
	}
}

func TestProcessInspectionProcHelper(t *testing.T) {
	raw := os.Getenv("INSPECTION_TARGET")
	if raw == "" {
		return
	}
	target, err := strconv.Atoi(raw)
	if err != nil || target <= 0 {
		t.Fatal("invalid fixture PID")
	}
	own, err := os.Readlink("/proc/self/ns/pid")
	if err != nil || own == os.Getenv("INSPECTION_HOST_NS") {
		t.Fatal("private namespace absent")
	}
	selfStatus, err := os.ReadFile("/proc/self/status")
	if err != nil || !inspectionStatusPID(string(selfStatus), os.Getpid(), true) {
		t.Fatal("proc view is not rooted in private namespace")
	}
	if err := unix.Kill(os.Getpid(), 0); err != nil {
		t.Fatal("own kill-0 positive control failed")
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	pidfd := err == nil
	if pidfd {
		unix.Close(fd)
	} else if err == unix.ENOSYS {
		t.Log("pidfd unavailable: kernel returns ENOSYS")
	} else {
		t.Fatalf("own pidfd positive failed: %v", err)
	}
	_, statErr := os.Stat(fmt.Sprintf("/proc/%d", target))
	if os.IsNotExist(statErr) {
		if err := unix.Kill(target, 0); err != unix.ESRCH {
			t.Fatalf("outside kill-0=%v", err)
		}
		if pidfd {
			fd, err = unix.PidfdOpen(target, 0)
			if err != unix.ESRCH {
				if err == nil {
					unix.Close(fd)
				}
				t.Fatalf("outside pidfd=%v", err)
			}
		}
	} else {
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", target))
		if err != nil || !inspectionStatusPID(string(status), target, false) {
			t.Fatal("visible outside process or permission refusal")
		}
		t.Log("numeric PID collision belongs to private namespace")
	}
	fmt.Println("proc-interfaces-proved")
}

func openInspectionDiagnosed(t *testing.T, ctx context.Context, o Options) *Sandbox {
	t.Helper()
	var transcript string
	s, err := openWithProof(ctx, o, func(ctx context.Context, o Options) (Proof, error) {
		return proveWorkbenchDiagnosed(ctx, o, func(output, marker string) { transcript = inspectionDiagnostic(output, marker) })
	})
	if err != nil {
		t.Logf("control=%s transcript:\n%s", inspectionControl(transcript), transcript)
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func inspectionStatusPID(status string, pid int, rooted bool) bool {
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == "NSpid:" {
			return fields[1] == strconv.Itoa(pid) && (!rooted || len(fields) == 2)
		}
	}
	return false
}
