package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

// Included only for inspection requests; legacy keys and launch mounts stay unchanged.
const processInspectionProofVersion = "bwrap-workbench-v5-process-inspection-v3"

func inspectionFailure(ctx context.Context, code string) error {
	if ctx.Err() != nil {
		code = CapabilityProbeTimeout
	}
	return &ProofError{Code: code, Step: ProofStepProcessInspection}
}

func proveProcessInspection(ctx context.Context, o Options, l workbenchLayout, binary string) error {
	return proveProcessInspectionUsing(ctx, o, l, binary, runInspectionProbe, nil)
}

// Instance-local seam exercises interruption without a sandbox or global hooks.
func proveProcessInspectionUsing(ctx context.Context, o Options, l workbenchLayout, binary string, run func(context.Context, string, workbenchLayout, string, bool) (string, error), observe func(string, int)) error {
	return proveProcessInspectionDiagnosed(ctx, o, l, binary, run, observe, nil)
}

func proveProcessInspectionDiagnosed(ctx context.Context, o Options, l workbenchLayout, binary string, run func(context.Context, string, workbenchLayout, string, bool) (string, error), observe func(string, int), diagnose func(string, string)) error {
	if ctx.Err() != nil {
		return inspectionFailure(ctx, CapabilityProbeTimeout)
	}
	marker := process.NewToken()
	fixture, err := os.CreateTemp("", "inspection-"+marker+"-")
	if err != nil {
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	defer os.Remove(fixture.Name())
	defer fixture.Close()
	if _, err := fixture.WriteString(marker); err != nil {
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	// Bounded fixture, no owner environment or credentials. The shell retains
	// argv until settlement and its children inherit the synthetic marker.
	pingR, pingW, err := os.Pipe()
	if err != nil {
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	defer pingR.Close()
	defer pingW.Close()
	pongR, pongW, err := os.Pipe()
	if err != nil {
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	defer pongR.Close()
	defer pongW.Close()
	cmd, p, err := process.Command(ctx, "/bin/sh", "-c", "/bin/sleep 60 & timer=$!; while read line; do [ \"$line\" = ping ] && echo pong; done; wait \"$timer\"", marker)
	if err != nil {
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "INSPECTION_MARKER=" + marker}
	cmd.ExtraFiles = []*os.File{fixture}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pingR, pongW, io.Discard
	cmd.WaitDelay = time.Second
	ready := make(chan int, 1)
	p.Notify(func(pid int) { ready <- pid })
	done := make(chan error, 1)
	go func() { done <- p.Run() }()
	defer func() { p.Close(); <-done }()
	var pid int
	select {
	case pid = <-ready:
	case <-ctx.Done():
		return inspectionFailure(ctx, CapabilityProbeTimeout)
	case e := <-done:
		done <- e
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	if observe != nil {
		observe("fixture", pid)
	}
	if ctx.Err() != nil {
		return inspectionFailure(ctx, CapabilityProbeTimeout)
	}
	if _, err := io.WriteString(pingW, "ping\n"); err != nil {
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	pong := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(pongR).ReadString('\n'); pong <- line }()
	select {
	case line := <-pong:
		if line != "pong\n" {
			return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
		}
	case <-ctx.Done():
		return inspectionFailure(ctx, CapabilityProbeTimeout)
	case e := <-done:
		done <- e
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	outsideNS, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
	if err != nil {
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	for _, name := range []string{"cmdline", "environ"} {
		data, e := os.ReadFile(fmt.Sprintf("/proc/%d/%s", pid, name))
		if e != nil || !strings.Contains(string(data), marker) {
			return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
		}
	}
	fd, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/3", pid))
	if err != nil || fd != fixture.Name() {
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	script := strings.Replace(inspectionScript(pid, outsideNS), "# Compare installed ps", "echo inspection-ready \"$child\"\nread ack\n[ \"$ack\" = go ] || exit 1\n# Compare installed ps", 1)
	if observe != nil {
		observe("launch", pid)
	}
	if ctx.Err() != nil {
		return inspectionFailure(ctx, CapabilityProbeTimeout)
	}
	output, err := run(ctx, binary, l, script, o.Background)
	if diagnose != nil {
		diagnose(output, marker)
	}
	if observe != nil {
		observe("judgment", pid)
	}
	// A recorded breach wins over cancellation of the trial.
	if failure := judgeInspectionTranscript(ctx, output, marker); failure != nil {
		return failure
	}
	if err != nil {
		return inspectionFailure(ctx, CapabilityProcessInspectionUnavailable)
	}
	if err := judgeProcessInspection(output, o.Background); err != nil {
		return err
	}
	return judgeInspectionHost(output)
}

func inspectionScript(outside int, outsideNS string) string {
	return fmt.Sprintf(`export LC_ALL=C
base=$(/usr/bin/ps -o ni= -p $$ | /usr/bin/tr -d ' ')
expected=$((base + 5)); [ "$expected" -le 19 ] || expected=19
echo expected-nice "$expected"
/usr/bin/nice -n 5 /bin/sleep 30 & child=$!
trap 'kill "$child" 2>/dev/null; wait "$child" 2>/dev/null' EXIT
# Wait for nice to exec sleep; the intermediate nice process proves nothing.
for attempt in 1 2 3 4 5 6 7 8 9 10; do
 [ "$(/bin/cat /proc/$child/comm)" = sleep ] && break
 /bin/sleep 0.02
done
/usr/bin/ps -o pgid= -p $$ | /usr/bin/sed 's/^/expected-group /'
# Compare installed ps with kernel stat for the live child, not an empty scan.
set -- $(/usr/bin/ps -o pid=,pgid=,ni=,stat= -p "$child")
printf 'positive %%s %%s %%s %%s\n' "$1" "$2" "$3" "$4"
/usr/bin/awk '{print "stat",$1,$5,$19,$3,$22}' /proc/$child/stat
/usr/bin/ps -o lstart=,args= -p "$child" | /usr/bin/sed 's/^/command /'
own=$(/usr/bin/readlink /proc/self/ns/pid)
if [ -z "$own" ]; then echo namespace-unavailable
elif [ "$own" = %s ]; then echo shared-namespace
else echo private-namespace; fi
# NSpid is relative to the namespace that mounted proc. One ID for our
# shell proves this proc view is rooted here, not in an ancestor namespace.
%s || echo foreign-process
# The rooted proc view above implies every visible PID is a namespace member.
# A numeric collision is classified from that view, not independently by status.
outside=%d
if [ ! -e /proc/$outside ]; then
 echo negative-invisible
elif %s; then
 echo negative-collision
else echo negative-visible-refused; fi
for entry in /proc/[0-9]*; do
 # PID 1 may be non-dumpable in bwrap's outer user namespace. Its ns
 # symlink is ptrace-gated; readable status still identifies the proc view.
 if [ -e "$entry/status" ]; then
  %s || printf '\nmembership-unavailable\n'
 fi
 /bin/cat "$entry/cmdline" "$entry/environ" 2>/dev/null
done
/bin/cat /proc/$outside/cmdline /proc/$outside/environ 2>/dev/null
/bin/ls /proc/$outside/fd 2>/dev/null
/usr/bin/readlink /proc/$outside/fd/* 2>/dev/null
/usr/bin/ps -ww -o pid=,args= -p "$outside" 2>/dev/null
/usr/bin/ps eww -p "$outside" 2>/dev/null
printf '\ninspection-ran\n'
`, workbenchShellQuote(outsideNS), inspectionStatusCheck("/proc/$$/status", "\"$$\"", true), outside, inspectionStatusCheck("/proc/$outside/status", "\"$outside\"", false), inspectionStatusCheck("\"$entry/status\"", "\"${entry##*/}\"", false))
}
