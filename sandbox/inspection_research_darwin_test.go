package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/process"
	"golang.org/x/sys/unix"
)

// Research observations are not production proof. Run explicitly with -v.
func TestProcessInspectionResearch(t *testing.T) {
	if os.Getenv("AGENT_HARNESS_INSPECTION_RESEARCH") != "1" {
		t.Skip("opt-in Seatbelt process-inspection research")
	}
	testenv.RequireNestedSandbox(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	l := workbenchLayout{Work: root, Home: root, Tmp: root, System: workbenchSystemDirs(), Read: []string{binary}}
	marker := "inspection-research-" + strconv.Itoa(os.Getpid())
	fixture, err := os.CreateTemp(t.TempDir(), marker+"-")
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	if _, err := fixture.WriteString(marker); err != nil {
		t.Fatal(err)
	}
	outside, p, err := process.Command(ctx, "/bin/sh", "-c", "/bin/sleep 55 & wait", marker)
	if err != nil {
		t.Fatal(err)
	}
	outside.Env = []string{"PATH=/usr/bin:/bin", "INSPECTION_MARKER=" + marker}
	outside.ExtraFiles = []*os.File{fixture}
	ready := make(chan int, 1)
	p.Notify(func(pid int) { ready <- pid })
	done := make(chan error, 1)
	go func() { done <- p.Run() }()
	defer func() { p.Close(); <-done }()
	var outsidePID int
	select {
	case outsidePID = <-ready:
	case e := <-done:
		done <- e
		t.Fatal(e)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	build, _ := unix.Sysctl("kern.osversion")
	t.Logf("OS build=%s", build)
	for _, row := range []struct{ name, rule string }{
		{"baseline", ""}, {"explicit-deny", "(deny process-info*)\n"},
		{"same-sandbox", "(allow process-info* (target same-sandbox))\n(allow sysctl-read (sysctl-name \"kern.proc.pid\") (sysctl-name \"kern.procargs2\") (target same-sandbox))\n"},
		{"self", "(allow process-info* (target self))\n(allow sysctl-read (sysctl-name \"kern.proc.pid\") (sysctl-name \"kern.procargs2\") (target self))\n"},
	} {
		profile := inspectionResearchProfile(l, binary) + row.rule
		var peers []int
		for _, suffix := range []string{"", "; different instance profile\n"} {
			peer, handle, e := process.Command(ctx, "/usr/bin/sandbox-exec", "-p", profile+suffix, "/bin/sh", "-c", "/bin/sleep 50 & wait", marker)
			if e != nil {
				t.Fatal(e)
			}
			peer.Dir = root
			peer.Env = outside.Env
			peer.ExtraFiles = []*os.File{fixture}
			started := make(chan int, 1)
			settled := make(chan error, 1)
			handle.Notify(func(pid int) { started <- pid })
			go func() { settled <- handle.Run() }()
			defer func() { handle.Close(); <-settled }()
			select {
			case pid := <-started:
				peers = append(peers, pid)
			case e := <-settled:
				settled <- e
				t.Logf("peer unavailable: %v", e)
				peers = append(peers, 0)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		script := fmt.Sprintf(`sleep 40 & child=$!; trap 'kill "$child"; wait "$child"' EXIT
for target in "$child" %d %d %d; do
case "$target" in "$child") label=own-child;; %d) label=unsandboxed;; %d) label=identical-profile;; *) label=different-profile;; esac
echo target=$target label=$label
/bin/ps -ww -o pid,pgid,nice,stat,lstart,command -p "$target"; status=$?; echo ps-status=$status; [ "$status" != 126 ] || echo research-exec-denied
/bin/ps -Eww -p "$target"; status=$?; echo env-status=$status; [ "$status" != 126 ] || echo research-exec-denied
/usr/sbin/lsof -p "$target"; echo lsof-status=$?
%s -test.run '^TestProcessInspectionSysctlHelper$' "$target"
status=$?; echo helper-status=$status; [ "$status" != 126 ] || echo research-exec-denied
done
/bin/ps -A -o pid,command; status=$?; echo listing-status=$status; [ "$status" != 126 ] || echo research-exec-denied
`, outsidePID, peers[0], peers[1], outsidePID, peers[0], workbenchShellQuote(binary))
		cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", "-p", profile, "/bin/sh", "-c", script)
		cmd.Dir = root
		cmd.Env = workbenchProbeEnvironment(l)
		cmd.ExtraFiles = []*os.File{fixture}
		output := &workbenchOutput{limit: 256 << 10}
		cmd.Stdout, cmd.Stderr = output, output
		e := cmd.Run()
		out := []byte(output.text())
		if e == nil && strings.Count(string(out), "research-helper-started\n") != 4 {
			t.Errorf("profile=%s inconclusive: helper startup not observed for every target", row.name)
		}
		if inspectionResearchExecDenied(string(out)) {
			t.Errorf("profile=%s inconclusive: research tool execution denied, not process-info refusal", row.name)
		}
		t.Logf("profile=%s exit=%v bytes=%d\n%s", row.name, e, len(out), out)
	}
}

func TestProcessInspectionSysctlHelper(t *testing.T) {
	if len(os.Args) < 2 {
		return
	}
	pid, err := strconv.Atoi(os.Args[len(os.Args)-1])
	if err != nil {
		return
	}
	// The inherited fixture descriptor supplies expected data without putting
	// it in observer/helper argv or environment that process listings inspect.
	fd := os.NewFile(3, "fixture-marker")
	marker, readErr := inspectionResearchMarker(fd)
	if readErr != nil {
		t.Fatalf("fixture descriptor unavailable: %v", readErr)
	}
	fmt.Println("research-helper-started")
	for _, name := range []string{"kern.proc.pid", "kern.procargs2", "kern.proc.all", "kern.proc.pgrp"} {
		args := []int{pid}
		if name == "kern.proc.pgrp" {
			group, e := unix.Getpgid(pid)
			if e != nil {
				fmt.Printf("getpgid errno=%v\n", e)
				continue
			}
			args = []int{group}
		}
		if name == "kern.proc.all" {
			args = nil
		}
		data, e := unix.SysctlRaw(name, args...)
		fmt.Printf("sysctl=%s errno=%v bytes=%d marker=%t\n", name, e, len(data), strings.Contains(string(data), marker))
	}
	// Darwin proc_info(call, pid, flavor, arg, buffer, size), CGO-free.
	for _, query := range []struct {
		name         string
		call, flavor uintptr
	}{
		{"pidinfo", 2, 3}, {"pidpath", 2, 11}, {"listpids", 1, 1}, {"pidfdinfo", 3, 2},
	} {
		buf := make([]byte, 64<<10)
		target, flavor, arg := uintptr(pid), query.flavor, uintptr(0)
		if query.name == "listpids" {
			target = 1
			flavor = 0
		}
		if query.name == "pidfdinfo" {
			arg = 3
		}
		if query.name == "pidpath" {
			buf = buf[:4096]
		}
		n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, query.call, target, flavor, arg, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		fmt.Printf("proc_info=%s errno=%d bytes=%d\n", query.name, errno, n)
	}
}

// Disposable research grants only: never used for production launches or keys.
func inspectionResearchProfile(l workbenchLayout, binary string) string {
	profile := seatbeltProfile(l)
	for _, path := range []string{"/bin/ps", binary} {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			resolved = path
		}
		for _, spelling := range []string{path, resolved} {
			profile += fmt.Sprintf("(allow process-exec (literal %q))\n(allow file-read* (literal %q))\n", spelling, spelling)
			for parent := filepath.Dir(spelling); ; parent = filepath.Dir(parent) {
				profile += fmt.Sprintf("(allow file-read-metadata (literal %q))\n", parent)
				if parent == filepath.Dir(parent) {
					break
				}
			}
		}
	}
	return profile
}

func TestProcessInspectionResearchGrants(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	l := workbenchLayout{Work: t.TempDir(), Home: t.TempDir(), Tmp: t.TempDir(), System: workbenchSystemDirs()}
	profile := inspectionResearchProfile(l, binary)
	for _, path := range []string{"/bin/ps", binary} {
		grant := fmt.Sprintf("(allow process-exec (literal %q))", path)
		if !strings.Contains(profile, grant) {
			t.Fatal("research executable absent", path)
		}
		if strings.Contains(seatbeltProfile(l), grant) {
			t.Fatal("research grant entered production")
		}
	}
}

func inspectionResearchExecDenied(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if line == "research-exec-denied" {
			return true
		}
	}
	return false
}

func TestProcessInspectionResearchClassification(t *testing.T) {
	if !inspectionResearchExecDenied("ps-status=126\nresearch-exec-denied\n") {
		t.Fatal("exec denial lost")
	}
	if inspectionResearchExecDenied("echo research-exec-denied\nps-status=1\n") {
		t.Fatal("process-info refusal classified as exec denial")
	}
}

func inspectionResearchMarker(fd *os.File) (string, error) {
	buf := make([]byte, 256)
	n, err := fd.ReadAt(buf, 0)
	if n == 0 {
		return "", err
	}
	return string(buf[:n]), nil
}

func TestProcessInspectionResearchMarkerDescriptor(t *testing.T) {
	fixture, err := os.CreateTemp(t.TempDir(), "marker-")
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	marker := "controlled-research-marker"
	if _, err := fixture.WriteString(marker); err != nil {
		t.Fatal(err)
	}
	// File offset is at EOF and shared by inherited descriptors. ReadAt must
	// recover expected data repeatedly without putting it in process argv.
	for attempt := 0; attempt < 2; attempt++ {
		got, err := inspectionResearchMarker(fixture)
		if err != nil || got != marker {
			t.Fatalf("marker=%q error=%v", got, err)
		}
	}
}
