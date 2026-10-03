package session

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

func proveWorkbench(ctx context.Context, o Options) error {
	if o.Workbench == nil || o.Workbench.Commands == nil {
		return nil
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		e := workbenchCapability(CapabilitySandboxToolMissing)
		e.Tools = []string{"sandbox-exec"}
		return e
	}
	ctx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	system, err := workbenchSystem(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return workbenchCapability(CapabilityProbeTimeout)
		}
		return workbenchCapability(CapabilitySandboxUnavailable)
	}
	for _, dir := range system {
		if workbenchSystemContains(dir, o.RuntimeHome) {
			return refuse(o, "runtime_home", RefusedRuntimeHome, "runtime home must be outside the command system read set")
		}
	}
	o.Workbench.system = system
	key, err := workbenchProbeKey(o, system)
	if err != nil {
		return workbenchCapability(CapabilitySandboxUnavailable)
	}
	if verified.holds(key) {
		return nil
	}
	if err = probeWorkbench(ctx, o, system); err != nil {
		if ctx.Err() != nil {
			return workbenchCapability(CapabilityProbeTimeout)
		}
		return err
	}
	verified.record(key)
	return nil
}

func probeWorkbench(ctx context.Context, o Options, system []string) error {
	workbenchCanaryRuns.Add(1)
	unavailable := workbenchCapability(CapabilitySandboxUnavailable)
	root, err := os.MkdirTemp("", "agent-harness-workbench-")
	if err != nil {
		return unavailable
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return unavailable
	}
	work := filepath.Join(root, "workspace")
	scratch := filepath.Join(root, "runtime", "sessions", "probe", "workbench")
	read := filepath.Join(root, "readset")
	for _, dir := range []string{filepath.Join(work, ".git"), filepath.Join(scratch, "home"), filepath.Join(scratch, "tmp"), read, filepath.Join(root, "owner"), filepath.Join(root, "private")} {
		if os.MkdirAll(dir, 0700) != nil {
			return unavailable
		}
	}
	for _, p := range []string{filepath.Join(work, ".git", "config"), filepath.Join(read, "marker"), filepath.Join(root, "owner", "marker"), filepath.Join(root, "private", "marker"), filepath.Join(root, "runtime", "transcript")} {
		if os.WriteFile(p, []byte("marker\n"), 0600) != nil {
			return unavailable
		}
	}
	if os.WriteFile(filepath.Join(work, ".harness-workbench-00000000000000000000000000000000-0000000000000000.tmp"), []byte("private temporary"), 0600) != nil {
		return unavailable
	}
	l := workbenchLayout{Work: work, Home: filepath.Join(scratch, "home"), Tmp: filepath.Join(scratch, "tmp"), Read: append(append([]string{}, o.Workbench.Commands.Read...), read), System: system, Write: o.Workbench.Write, Loopback: o.Workbench.Commands.Loopback}
	// Test launch first. Nested Seatbelt refusal must not trigger any network
	// witness or touch the real session's runtime state.
	if _, err = runWorkbenchProbe(ctx, l, "echo canary-ran", false, workbenchInboundProbe{}); err != nil {
		if ctx.Err() != nil {
			return workbenchCapability(CapabilityProbeTimeout)
		}
		return unavailable
	}
	// Prove group inspection positively as well: an empty scan must not let
	// the reaper kill a real background job or retain every empty supervisor.
	if _, err = runWorkbenchProbe(ctx, l, "/bin/sleep 30 >/dev/null 2>&1 & echo canary-ran", true, workbenchInboundProbe{}); err != nil {
		return unavailable
	}
	// A sibling fixture cannot witness the owner's home. Prove that directory
	// enumeration works outside the sandbox, without reading credential files.
	ownerHome, err := os.UserHomeDir()
	if err != nil {
		return unavailable
	}
	ownerHome, err = filepath.EvalSymlinks(ownerHome)
	if err != nil {
		return unavailable
	}
	homeDir, err := os.Open(ownerHome)
	if err != nil {
		return unavailable
	}
	_, err = homeDir.ReadDir(1)
	homeDir.Close()
	if err != nil {
		return unavailable
	}
	witness, ok := offMachineWitness(ctx)
	if !ok {
		return unavailable
	}
	// Discover metadata only, then positively query one existing keychain.
	// Merely listing a scratch HOME's default search path would prove nothing.
	keychainCmd, keychainProcess, e := process.Command(ctx, "/usr/bin/security", "list-keychains", "-d", "user")
	if e != nil {
		return unavailable
	}
	keychainOut := &workbenchOutput{limit: 4096}
	keychainCmd.Stdout = keychainOut
	keychainCmd.WaitDelay = 2 * time.Second
	e = keychainProcess.Run()
	keychainProcess.Close()
	if e != nil || strings.TrimSpace(keychainOut.text()) == "" {
		return unavailable
	}
	keychain := ""
	for _, line := range strings.Split(keychainOut.text(), "\n") {
		name := strings.Trim(strings.TrimSpace(line), "\"")
		if info, e := os.Stat(name); e == nil && info.Mode().IsRegular() {
			keychain = name
			break
		}
	}
	if keychain == "" {
		return unavailable
	}
	keychainCheck := "/usr/bin/security show-keychain-info " + workbenchShellQuote(keychain)
	cmdCheck, procCheck, e := process.Command(ctx, "/bin/sh", "-c", keychainCheck)
	if e != nil {
		return unavailable
	}
	cmdCheck.WaitDelay = 2 * time.Second
	e = procCheck.Run()
	procCheck.Close()
	if e != nil {
		return unavailable
	}
	listener, wg, reached, err := countingListener("127.0.0.1:0")
	if err != nil {
		return unavailable
	}
	defer func() { listener.Close(); wg.Wait() }()
	port := listener.Addr().(*net.TCPAddr).Port
	// The socket lives outside every allowed subtree. Use a short path to fit
	// sockaddr_un; the probe root may be deep on CI.
	socketDir, err := os.MkdirTemp("/tmp", "wb-s-")
	if err != nil {
		return unavailable
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "socket")
	unixListener, err := net.Listen("unix", socket)
	if err != nil {
		return unavailable
	}
	defer unixListener.Close()
	socketReached := make(chan struct{}, 1)
	go func() {
		for {
			c, e := unixListener.Accept()
			if e != nil {
				return
			}
			c.Close()
			select {
			case socketReached <- struct{}{}:
			default:
			}
		}
	}()
	// Positive control uses the identical installed nc invocation. A broken
	// client must refuse proof rather than masquerade as socket isolation.
	connect, connectProcess, e := process.Command(ctx, "/bin/sh", "-c", workbenchUnixConnect(socket))
	if e != nil {
		return unavailable
	}
	connect.WaitDelay = 2 * time.Second
	e = connectProcess.Run()
	connectProcess.Close()
	if e != nil {
		return unavailable
	}
	select {
	case <-socketReached:
	case <-ctx.Done():
		return unavailable
	}
	// A disposable image is readable to the command; mounting it is not. No
	// owner image is used. Setup positively attaches and detaches this image.
	image := filepath.Join(l.Tmp, "canary.dmg")
	cmd, p, e := process.Command(ctx, "/usr/bin/hdiutil", "create", "-size", "8m", "-fs", "HFS+", "-volname", "HarnessCanary", image)
	if e != nil {
		return unavailable
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + l.Home, "TMPDIR=" + l.Tmp}
	e = p.Run()
	p.Close()
	if e != nil {
		return unavailable
	}
	mount := filepath.Join(work, "mount")
	if os.Mkdir(mount, 0700) != nil {
		return unavailable
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, p, e := process.Command(cleanupCtx, "/usr/bin/hdiutil", "detach", mount)
		if e == nil {
			c.WaitDelay = time.Second
			_ = p.Run()
			p.Close()
		}
	}()
	// Prove attachment works outside Seatbelt with the identical environment
	// and mount point. An unavailable disk-image service proves nothing.
	before, statErr := os.Stat(mount)
	if statErr != nil {
		return unavailable
	}
	attach, ap, ae := process.Command(ctx, "/usr/bin/hdiutil", "attach", "-nobrowse", "-mountpoint", mount, image)
	if ae != nil {
		return unavailable
	}
	// hdiutil leaves a disk-image helper in its group that serves the mount,
	// and closing the handle reaps the group, so the handle stays open until
	// the probe is done; the detach below ends the helper first.
	defer ap.Close()
	attach.Env = cmd.Env
	attach.WaitDelay = time.Second
	ae = ap.Run()
	after, statErr := os.Stat(mount)
	if ae != nil || statErr != nil || before.Sys().(*syscall.Stat_t).Dev == after.Sys().(*syscall.Stat_t).Dev {
		return unavailable
	}
	detach, dp, de := process.Command(ctx, "/usr/bin/hdiutil", "detach", mount)
	if de != nil {
		return unavailable
	}
	detach.Env = cmd.Env
	detach.WaitDelay = time.Second
	de = dp.Run()
	dp.Close()
	restored, statErr := os.Stat(mount)
	if de != nil || statErr != nil || !os.SameFile(before, restored) {
		return unavailable
	}
	script := workbenchCanary(l, root, read, socket, image, mount, witness, port)
	script += workbenchReadWitnesses(root, ownerHome)
	script += "try keychain /bin/sh -c " + workbenchShellQuote(keychainCheck) + "\n"
	inbound := workbenchInboundProbe{}
	if l.Loopback {
		bind, e := freeLoopbackPort()
		if e != nil {
			return unavailable
		}
		script += "\n" + loopbackCanary(port, witness, 443, bind)
		script += "\nkill \"$!\" >/dev/null 2>&1; wait \"$!\" 2>/dev/null\n"
		inbound.Port, err = freeLoopbackPort()
		if err != nil {
			return unavailable
		}
		inbound.Nonce = process.NewToken()
		script += fmt.Sprintf("printf '%s\\n' | nc -l 127.0.0.1 %d >/dev/null 2>&1 &\nsleep 3\nkill \"$!\" >/dev/null 2>&1; wait \"$!\" 2>/dev/null\n", inbound.Nonce, inbound.Port)
	}
	script += "echo canary-ran\n"
	output, err := runWorkbenchProbe(ctx, l, script, false, inbound)
	if ctx.Err() != nil {
		return workbenchCapability(CapabilityProbeTimeout)
	}
	if err != nil {
		return unavailable
	}
	// Observations outside the sandbox take precedence over scripted reports.
	postMount, statErr := os.Stat(mount)
	if statErr != nil || !os.SameFile(before, postMount) {
		return workbenchCapability(CapabilitySandboxNotEnforced)
	}
	git, err := os.ReadFile(filepath.Join(work, ".git", "config"))
	if err != nil || !bytes.Equal(git, []byte("marker\n")) {
		return workbenchCapability(CapabilitySandboxNotEnforced)
	}
	if _, err = os.Lstat(filepath.Join(root, "sibling-write")); err == nil {
		return workbenchCapability(CapabilitySandboxNotEnforced)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-socketReached:
		return workbenchCapability(CapabilitySandboxNotEnforced)
	default:
	}
	return judgeWorkbench(output, l.Write, l.Loopback, reached())
}

type workbenchInboundProbe struct {
	Port  int
	Nonce string
}

func runWorkbenchProbe(ctx context.Context, l workbenchLayout, script string, expectBackground bool, inbound workbenchInboundProbe) (string, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer r.Close()
	defer w.Close()
	keepRead, keepWrite, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer keepRead.Close()
	defer keepWrite.Close()
	token := process.NewToken()
	since := time.Now()
	args := append(workbenchSandboxArgs(l, workbenchSupervisor), process.TokenArgument(token), script)
	cmd, p, err := process.Command(ctx, "/usr/bin/sandbox-exec", args...)
	if err != nil {
		return "", err
	}
	defer p.Close()
	cmd.ExtraFiles = []*os.File{w, keepRead, keepWrite}
	p.Notify(func(int) { w.Close() })
	cmd.Dir = l.Work
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + l.Home, "TMPDIR=" + l.Tmp}
	cmd.WaitDelay = 2 * time.Second
	out := &workbenchOutput{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = out
	done := make(chan error, 1)
	go func() { err := p.Run(); w.Close(); done <- err }()
	status := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(r).ReadString('\n'); status <- line }()
	inboundDone := make(chan bool, 1)
	inboundCtx, cancelInbound := context.WithCancel(ctx)
	defer func() {
		cancelInbound()
		for range inboundDone {
		}
	}()
	if inbound.Port != 0 {
		go func() {
			defer close(inboundDone)
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				conn, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(inbound.Port)), 100*time.Millisecond)
				if e == nil {
					_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
					line, readErr := bufio.NewReader(io.LimitReader(conn, 64)).ReadString('\n')
					conn.Close()
					if readErr == nil && line == inbound.Nonce+"\n" {
						inboundDone <- true
						return
					}
				}
				select {
				case <-ticker.C:
				case <-inboundCtx.Done():
					inboundDone <- false
					return
				}
			}
		}()
	} else {
		inboundDone <- false
		close(inboundDone)
	}
	var line string
	select {
	case line = <-status:
	case <-ctx.Done():
		p.Stop()
		<-done
		return out.text(), ctx.Err()
	}
	owned := process.TokenPresent(token, since)
	children, inspectErr := p.GroupHasChildren()
	p.Stop()
	err = <-done
	if line != "0\n" || !owned || inspectErr != nil || children != expectBackground {
		return out.text(), fmt.Errorf("sandbox supervisor protocol unavailable")
	}
	output := out.text()
	// Settle the witness even if it read the nonce just before cancellation
	// but has not yet published its result.
	cancelInbound()
	if connected := <-inboundDone; connected {
		output = "inbound\n" + output
	}
	return output, nil
}

func workbenchCanary(l workbenchLayout, root, read, socket, image, mount, witness string, port int) string {
	q := workbenchShellQuote
	try := func(label, command string) string { return "try " + label + " /bin/sh -c " + q(command) + "\n" }
	s := "try() { name=$1; shift; if ( \"$@\" ) >/dev/null 2>&1; then echo \"$name\"; fi; }\n"
	s += try("inside", "echo x > inside")
	s += try("nested", "mkdir -p deep/nested && echo x > deep/nested/file")
	s += try("sibling", "echo x > "+q(filepath.Join(root, "sibling-write")))
	s += try("tmp", "echo x > "+q(filepath.Join(l.Tmp, "tmp-write")))
	s += try("tmpdir", "echo x > \"$TMPDIR/tmpdir-write\"")
	s += try("gitdir", "echo x >> .git/config")
	s += try("gitcase", "echo x >> .GIT/config")
	s += try("reserved-temp", "cat .harness-workbench-00000000000000000000000000000000-0000000000000000.tmp")
	s += try("gitmove", "mv .git git-moved && echo x >> git-moved/config; result=$?; if [ -d git-moved ]; then mv git-moved .git; fi; exit $result")
	s += try("gitlink", "ln -s .git git-link && echo x >> git-link/config")
	s += try("githardlink", "ln .git/config git-hard && echo x >> git-hard")
	s += try("home", "cat "+q(filepath.Join(root, "owner", "marker")))
	s += try("outside", "cat "+q(filepath.Join(root, "private", "marker")))
	s += try("runtime", "cat "+q(filepath.Join(root, "runtime", "transcript")))
	s += try("system", "/bin/sh -c true && ls /usr/bin && cat /etc/passwd")
	s += try("readset", "cat "+q(filepath.Join(read, "marker")))
	s += try("readset-write", "echo x > "+q(filepath.Join(read, "write")))
	s += try("link", "ln "+q(filepath.Join(root, "owner", "marker"))+" secret-link")
	s += try("socket", workbenchUnixConnect(socket))
	s += "command -v nc >/dev/null 2>&1 || echo no-network-client\n"
	s += try("network", fmt.Sprintf("nc -z -w 2 %s 443", witness))
	if !l.Loopback {
		s += try("localhost", "nc -z -w 2 127.0.0.1 "+strconv.Itoa(port))
	}
	// The keychain witness is appended only after its outside positive control.
	s += try("mount", "hdiutil attach -nobrowse -mountpoint "+q(mount)+" "+q(image))
	return s
}

func judgeWorkbench(output string, write, loopback, reached bool) error {
	lines, last := workbenchCanaryLines(output)
	if workbenchCanaryEscaped(lines, "gitcase", "reserved-temp", "real-home", "outside-data", "runtime-data", "real-home-data", "keychain", "mount") {
		return workbenchCapability(CapabilitySandboxNotEnforced)
	}
	if !write && (lines["inside"] || lines["nested"]) {
		return workbenchCapability(CapabilitySandboxNotEnforced)
	}
	if !loopback && reached {
		return workbenchCapability(CapabilitySandboxNotEnforced)
	}
	if last != canaryRan || !lines[canaryRan] || lines[canaryNoClient] || !lines["tmp"] || !lines["tmpdir"] || !lines["system"] || !lines["readset"] || (write && (!lines["inside"] || !lines["nested"])) {
		return workbenchCapability(CapabilitySandboxUnavailable)
	}
	if loopback {
		if !lines[loopbackReached] || !lines[loopbackBound] || !lines["inbound"] || lines[loopbackOutside] || !reached {
			return workbenchCapability(CapabilitySandboxNotEnforced)
		}
	}
	return nil
}

// macOS nc's zero-I/O scan (-z) does not support Unix sockets. A real connect
// with bounded lifetime and EOF input exercises the installed socket client.
func workbenchUnixConnect(socket string) string {
	return "/usr/bin/nc -U -w 2 " + workbenchShellQuote(socket) + " </dev/null"
}

func workbenchReadWitnesses(root, home string) string {
	q := workbenchShellQuote
	s := "try real-home /bin/sh -c " + q("/bin/ls -A "+q(home)) + "\n"
	for _, witness := range []struct{ label, path, command string }{
		{"outside-data", filepath.Join(root, "private", "marker"), "cat"},
		{"runtime-data", filepath.Join(root, "runtime", "transcript"), "cat"},
		{"real-home-data", home, "/bin/ls -A"},
	} {
		data := filepath.Join("/System/Volumes/Data", witness.path)
		if info, err := os.Stat(data); err == nil {
			original, err := os.Stat(witness.path)
			if err == nil && os.SameFile(info, original) {
				s += "try " + witness.label + " /bin/sh -c " + q(witness.command+" "+q(data)) + "\n"
			}
		}
	}
	return s
}
