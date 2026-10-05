package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

// Observe only descendants of the controlled bwrap launch. The child pauses
// while its host-side identity is read, so exit/PID reuse cannot certify proof.
func runInspectionProbe(ctx context.Context, binary string, l workbenchLayout, script string, background bool) (string, error) {
	return runInspectionProbeObserved(ctx, binary, l, script, background, nil)
}

func runInspectionProbeObserved(ctx context.Context, binary string, l workbenchLayout, script string, background bool, readyChild func()) (string, error) {
	args, err := bwrapArgs(l)
	if err != nil {
		return "", err
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer statusR.Close()
	defer statusW.Close()
	outputR, outputW, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer outputR.Close()
	defer outputW.Close()
	inputR, inputW, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer inputR.Close()
	defer inputW.Close()
	args = append(args, "--json-status-fd", "3", "--chdir", l.Work, "--", "/bin/sh", "-c", script)
	if background {
		binary, args = linuxBackgroundLaunch(binary, args)
	}
	cmd, p, err := process.Command(ctx, binary, args...)
	if err != nil {
		return "", err
	}
	defer p.Close()
	launched := make(chan int, 1)
	p.Notify(func(pid int) { statusW.Close(); launched <- pid })
	cmd.ExtraFiles = []*os.File{statusW}
	cmd.Stdin = inputR
	cmd.Stdout = outputW
	cmd.Stderr = io.Discard
	cmd.Env = workbenchProbeEnvironment(l)
	cmd.WaitDelay = time.Second
	out := &workbenchOutput{limit: 64 << 10}
	ready := make(chan int, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		scanner := bufio.NewScanner(io.TeeReader(outputR, out))
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "inspection-ready ") {
				pid, _ := strconv.Atoi(strings.TrimPrefix(scanner.Text(), "inspection-ready "))
				select {
				case ready <- pid:
				default:
				}
			}
		}
	}()
	done := make(chan error, 1)
	go func() { e := p.Run(); statusW.Close(); outputW.Close(); done <- e }()
	status := make(chan bool, 1)
	go func() {
		started, code, settled := readBwrapStatus(statusR, nil)
		status <- started && settled && code == 0
	}()
	// Always settle process, stdout copying and status consumption before returning.
	defer func() {
		p.Stop()
		<-done
		statusR.Close()
		<-status
		outputW.Close()
		<-drained
	}()
	var leader, child int
	select {
	case leader = <-launched:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-drained:
		return out.text(), fmt.Errorf("inspection launch unavailable")
	}
	select {
	case child = <-ready:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-drained:
		return out.text(), fmt.Errorf("inspection child unavailable")
	}
	if readyChild != nil {
		readyChild()
	}
	if ctx.Err() != nil {
		return out.text(), ctx.Err()
	}
	facts, err := inspectionHostChild(leader, child)
	if err != nil {
		return out.text(), err
	}
	// Feed host truth back into the fixed protocol, outside the child's output.
	out.Write([]byte("host " + strings.Join(facts, " ") + "\n"))
	if _, err = io.WriteString(inputW, "go\n"); err != nil {
		return out.text(), err
	}
	inputW.Close()
	select {
	case <-drained:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	// The deferred settlement consumes done/status; validate both here then put
	// them back for the one common cleanup path.
	e := <-done
	done <- e
	ok := <-status
	status <- ok
	if e != nil || !ok {
		return out.text(), fmt.Errorf("inspection status unavailable")
	}
	return out.text(), nil
}

func inspectionHostChild(leader, namespacePID int) ([]string, error) {
	queue := []int{leader}
	seen := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		root := filepath.Join("/proc", strconv.Itoa(pid))
		children, e := os.ReadFile(filepath.Join(root, "task", strconv.Itoa(pid), "children"))
		if e == nil {
			for _, field := range strings.Fields(string(children)) {
				n, _ := strconv.Atoi(field)
				if n > 0 {
					queue = append(queue, n)
				}
			}
		}
		status, e := os.ReadFile(filepath.Join(root, "status"))
		if e != nil {
			continue
		}
		var nsPID, nsGroup string
		for _, line := range strings.Split(string(status), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			switch fields[0] {
			case "NSpid:":
				nsPID = fields[len(fields)-1]
			case "NSpgid:":
				nsGroup = fields[len(fields)-1]
			}
		}
		if nsPID != strconv.Itoa(namespacePID) {
			continue
		}
		raw, e := os.ReadFile(filepath.Join(root, "stat"))
		if e != nil {
			return nil, e
		}
		end := strings.LastIndex(string(raw), ")")
		if end < 0 {
			return nil, fmt.Errorf("host stat malformed")
		}
		if !strings.Contains(string(raw[:end+1]), "(sleep)") {
			continue
		}
		fields := strings.Fields(string(raw)[end+1:])
		if len(fields) < 20 {
			return nil, fmt.Errorf("host stat incomplete")
		}
		// stat tail begins at field 3; nice=19, birth=22.
		if fields[0] == "Z" || nsGroup == "" {
			return nil, fmt.Errorf("host child not live")
		}
		return []string{nsPID, nsGroup, fields[16], fields[19]}, nil
	}
	return nil, fmt.Errorf("controlled host child unavailable")
}
