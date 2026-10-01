package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
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

func requireWorkbenchSeatbelt(t *testing.T) {
	t.Helper()
	testenv.RequireProcessGroup(t)
	// A trivial profile isolates nesting refusal from generated-profile errors.
	trivial := exec.Command("/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)", "/bin/sh", "-c", "true")
	out, err := trivial.CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		aborted := errors.As(err, &exit) && exit.ProcessState.Sys().(syscall.WaitStatus).Signal() == syscall.SIGABRT
		if aborted || strings.Contains(string(out), "sandbox_apply: Operation not permitted") {
			err = errors.Join(fs.ErrPermission, err)
		}
		testenv.SkipIfRefused(t, "nested Seatbelt launch", err)
	}
	profile := seatbeltProfile(workbenchLayout{Work: t.TempDir(), Home: t.TempDir(), Tmp: t.TempDir(), System: workbenchSystemDirs(), Write: true})
	cmd := exec.Command("/usr/bin/sandbox-exec", "-p", profile, "/bin/sh", "-c", "true")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated Seatbelt profile failed after trivial launch succeeded: %v: %s", err, out)
	}
}

// The supervisor protocol is exercised without Seatbelt using a fixed
// synthetic command, so nested-sandbox refusal does not hide pipe settlement.
func TestWorkbenchSupervisorProtocol(t *testing.T) {
	testenv.RequireProcessGroup(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	keepRead, keepWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer keepRead.Close()
	defer keepWrite.Close()
	cmd, p, err := process.Command(context.Background(), "/bin/sh", "-c", workbenchSupervisor, process.TokenArgument(process.NewToken()), "echo output; (echo forged >&3) 2>/dev/null; exit 7")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cmd.ExtraFiles = []*os.File{w, keepRead, keepWrite}
	out := &workbenchOutput{limit: 100}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = time.Second
	p.Notify(func(int) { w.Close() })
	done := make(chan error, 1)
	go func() { done <- p.Run() }()
	data := make([]byte, 20)
	n, err := r.Read(data)
	if err != nil || string(data[:n]) != "7\n" {
		t.Fatalf("completion %q: %v", data[:n], err)
	}
	select {
	case err := <-done:
		t.Fatalf("supervisor exited early: %v", err)
	default:
	}
	p.Stop()
	<-done
	if !strings.Contains(out.text(), "output") || strings.Contains(out.text(), "forged") {
		t.Fatal(out.text())
	}
}

func TestWorkbenchSupervisorsReapedWithoutJobs(t *testing.T) {
	testenv.RequireProcessGroup(t)
	testenv.RequireProcessStatus(t)
	for range 25 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		keepRead, keepWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		token, since := process.NewToken(), time.Now()
		cmd, p, err := process.Command(context.Background(), "/bin/sh", "-c", workbenchSupervisor, process.TokenArgument(token), "true")
		if err != nil {
			t.Fatal(err)
		}
		cmd.ExtraFiles = []*os.File{w, keepRead, keepWrite}
		pidReady := make(chan int, 1)
		p.Notify(func(pid int) { w.Close(); keepRead.Close(); keepWrite.Close(); pidReady <- pid })
		done := make(chan struct{})
		go func() { _ = p.Run(); close(done) }()
		pid := <-pidReady
		line := make([]byte, 2)
		if n, err := r.Read(line); err != nil || string(line[:n]) != "0\n" {
			p.Close()
			t.Fatalf("%q %v", line[:n], err)
		}
		r.Close()
		if children, err := p.GroupHasChildren(); err != nil || children {
			p.Close()
			t.Fatalf("empty group: %t %v", children, err)
		}
		go reapWorkbenchSupervisor(p, done)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			p.Close()
			t.Fatal("empty supervisor was retained")
		}
		if syscall.Kill(pid, 0) == nil || process.TokenPresent(token, since) {
			t.Fatal("supervisor outlived empty group")
		}
		p.Close()
	}
}

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
		r, err := s.api.workspace.commands.run(context.Background(), "true", ".", time.Second)
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
	r, err := s.api.workspace.commands.run(context.Background(), "sleep 60 >/dev/null 2>&1 & echo $! > pid", ".", time.Second)
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
	r, err = s.api.workspace.commands.run(context.Background(), "cat pid", ".", time.Second)
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
	r, err = resumed.api.workspace.commands.run(context.Background(), "sleep 60 >/dev/null 2>&1 & echo $! > pid", ".", time.Second)
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

func TestWorkbenchSeatbeltProfile(t *testing.T) {
	for _, write := range []bool{false, true} {
		for _, loop := range []bool{false, true} {
			l := workbenchLayout{Work: "/workspace", Home: "/runtime/home", Tmp: "/runtime/tmp", Read: []string{"/readset"}, System: []string{"/System", "/usr"}, Write: write, Loopback: loop}
			profile := seatbeltProfile(l)
			for _, required := range []string{"(deny default)", "(require-not (subpath \"/System/Volumes/Data\"))", "(allow process-exec)", "(allow process-fork)", "(target same-sandbox)", "(deny file-link", "(deny file-write*", "(subpath \"/readset\")", "(literal \"/private/etc/passwd\")", "(allow file-read-data (literal \"/\"))"} {
				// Public etc entries use subpath so an optional directory (ssl/certs)
				// and regular files share one pinned list.
				if required == "(literal \"/private/etc/passwd\")" {
					required = "(subpath \"/private/etc/passwd\")"
				}
				if !strings.Contains(profile, required) {
					t.Fatalf("missing %q in %s", required, profile)
				}
			}
			if strings.Contains(profile, "SecurityServer") || strings.Contains(profile, "keychain") && strings.Contains(profile, "global-name \"com.apple.security") {
				t.Fatal("keychain service emitted")
			}
			if strings.Contains(profile, "(allow network-outbound") != loop {
				t.Fatal("loopback drift")
			}
			if strings.Contains(profile, "(allow file-write* (subpath \"/workspace\"))") != write {
				t.Fatal("write drift")
			}
		}
	}
}

func TestWorkbenchCanaryJudge(t *testing.T) {
	good := "inside\nnested\ntmp\ntmpdir\nsystem\nreadset\ncanary-ran\n"
	if err := judgeWorkbench(good, true, false, false); err != nil {
		t.Fatal(err)
	}
	for _, escape := range []string{"sibling", "gitdir", "gitmove", "gitlink", "githardlink", "home", "real-home", "outside-data", "runtime-data", "real-home-data", "outside", "runtime", "readset-write", "link", "socket", "network", "localhost", "keychain", "mount"} {
		err := judgeWorkbench(good+escape+"\n", true, false, false)
		var cap *CapabilityError
		if !errors.As(err, &cap) || cap.Code != CapabilitySandboxNotEnforced {
			t.Fatalf("%s: %v", escape, err)
		}
	}
	if judgeWorkbench(strings.ReplaceAll(good, "canary-ran\n", ""), true, false, false) == nil {
		t.Fatal("missing final witness accepted")
	}
	if judgeWorkbench(good+"unfinished\n", true, false, false) == nil {
		t.Fatal("non-final completion witness accepted")
	}
	if judgeWorkbench(good, true, false, true) == nil {
		t.Fatal("network observation ignored")
	}
}

func TestWorkbenchCanaryWithoutSandboxReportsEscapes(t *testing.T) {
	// All filesystem and socket escapes use disposable fixtures. The two
	// privileged witnesses use synthetic success here; only the real canary
	// establishes keychain and disk-image denial against the installed OS.
	listener, wg, reached, err := countingListener("127.0.0.1:0")
	testenv.SkipIfRefused(t, "the canary's local network witness", err)
	defer func() { listener.Close(); wg.Wait() }()
	dir, err := os.MkdirTemp(".", ".wbc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	root, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	read := filepath.Join(root, "readset")
	l := workbenchLayout{Work: work, Home: filepath.Join(root, "runtime", "home"), Tmp: filepath.Join(root, "runtime", "tmp"), Write: true}
	for _, p := range []string{filepath.Join(work, ".git", "config"), filepath.Join(work, ".GIT", "config"), filepath.Join(root, "owner", "marker"), filepath.Join(root, "private", "marker"), filepath.Join(root, "runtime", "transcript"), filepath.Join(read, "marker"), filepath.Join(work, ".harness-workbench-00000000000000000000000000000000-0000000000000000.tmp")} {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		// Only an actual fixture permission refusal may skip; CI forbids it.
		testenv.SkipIfRefused(t, "canary fixture creation", os.WriteFile(p, []byte("marker\n"), 0600))
	}
	for _, p := range []string{l.Home, l.Tmp} {
		if os.MkdirAll(p, 0700) != nil {
			t.Fatal("scratch setup")
		}
	}
	// Relative Unix paths avoid sockaddr_un's limit in deeply nested checkouts.
	socket, err := net.Listen("unix", filepath.Join(dir, "outside.sock"))
	testenv.SkipIfRefused(t, "the canary's Unix socket", err)
	defer socket.Close()
	socketReached := make(chan struct{}, 1)
	go func() {
		for {
			c, e := socket.Accept()
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
	port := listener.Addr().(*net.TCPAddr).Port
	script := workbenchCanary(l, root, read, "../outside.sock", filepath.Join(l.Tmp, "image.dmg"), filepath.Join(work, "mount"), "127.0.0.1", port)
	script = strings.ReplaceAll(script, "nc -z -w 2 127.0.0.1 443", "nc -z -w 2 127.0.0.1 "+strconv.Itoa(port))
	script += workbenchReadWitnesses(root, filepath.Join(root, "owner"))
	script += "try keychain /bin/sh -c true\necho canary-ran\n"
	lines := strings.Split(script, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "try mount ") {
			lines[i] = "try mount /bin/sh -c true"
		}
	}
	script = strings.Join(lines, "\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, p, err := process.Command(ctx, "/bin/sh", "-c", script)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cmd.Dir = work
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + l.Home, "TMPDIR=" + l.Tmp}
	cmd.WaitDelay = time.Second
	out := &workbenchOutput{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(out.text(), "\n") {
		seen[line] = true
	}
	names := []string{"inside", "nested", "tmp", "tmpdir", "sibling", "gitdir", "gitcase", "gitmove", "gitlink", "githardlink", "reserved-temp", "home", "real-home", "outside", "runtime", "readset", "readset-write", "link", "socket", "network", "localhost", "keychain", "mount", canaryRan}
	for _, label := range []string{"outside-data", "runtime-data", "real-home-data"} {
		if strings.Contains(script, "try "+label+" ") {
			names = append(names, label)
		}
	}
	for _, name := range names {
		if !seen[name] {
			t.Fatalf("escape %s not observed: %s", name, out.text())
		}
	}
	select {
	case <-socketReached:
	case <-time.After(time.Second):
		t.Fatal("Unix socket client reported success without a connection")
	}
	if !reached() || judgeWorkbench(out.text(), true, false, true) == nil {
		t.Fatal("unsandboxed execution accepted")
	}
	git, _ := os.ReadFile(filepath.Join(work, ".git", "config"))
	if string(git) == "marker\n" {
		t.Fatal("negative git witness did not change config")
	}
}

func TestWorkbenchUnixSocketClientPositive(t *testing.T) {
	dir, err := os.MkdirTemp(".", ".wbu-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "socket")
	listener, err := net.Listen("unix", path)
	testenv.SkipIfRefused(t, "Unix socket positive control", err)
	defer listener.Close()
	reached := make(chan bool, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			conn.Close()
		}
		reached <- err == nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, p, err := process.Command(ctx, "/bin/sh", "-c", workbenchUnixConnect(path))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cmd.WaitDelay = time.Second
	if err := p.Run(); err != nil {
		t.Fatalf("installed Unix socket client failed: %v", err)
	}
	select {
	case ok := <-reached:
		if !ok {
			t.Fatal("connection not observed")
		}
	case <-ctx.Done():
		t.Fatal("connection not observed")
	}
}

func TestWorkbenchRuntimeHomeInSystemReadSetRefused(t *testing.T) {
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Commands = &Commands{}
	// Direct normalization tests the system-set rule without an earlier
	// private-directory mode check masking it, and performs no writes.
	o.RuntimeHome = "/usr"
	_, err := normalizeWorkbenchCommands(o)
	var refused *UnsupportedError
	if !errors.As(err, &refused) || refused.Code != RefusedRuntimeHome {
		t.Fatalf("%v", err)
	}
	if workbenchSystemContains("/System", "/System/Volumes/Data/Users/owner") {
		t.Fatal("data volume counted as system read set")
	}
	for _, system := range workbenchSystemDirs() {
		if system == "/System" {
			continue
		}
		data := filepath.Join("/System/Volumes/Data", system)
		original, err := os.Stat(system)
		if err != nil {
			continue
		}
		aliased, err := os.Stat(data)
		if err == nil && os.SameFile(original, aliased) && !workbenchSystemContains(system, data) {
			t.Fatalf("system alias %s missed", data)
		}
	}
}

func TestWorkbenchProbeKeySeparatesLaunches(t *testing.T) {
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Commands = &Commands{}
	base, err := workbenchProbeKey(o, []string{"/System", "/usr"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		edit   func(*Options)
		system []string
	}{
		{"workspace", func(o *Options) { o.WorkDir += "/other" }, nil},
		{"runtime", func(o *Options) { o.RuntimeHome += "/other" }, nil},
		{"write", func(o *Options) { o.Workbench.Write = true }, nil},
		{"loopback", func(o *Options) { o.Workbench.Commands.Loopback = true }, nil},
		{"read", func(o *Options) { o.Workbench.Commands.Read = []string{"/toolchain"} }, nil},
		{"environment", func(o *Options) { o.Workbench.Commands.Env = []string{"LANG=C"} }, nil},
		{"background", func(o *Options) { o.Background = true }, nil},
		{"system", func(o *Options) {}, []string{"/System", "/usr", "/new-runtime"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := o
			wb := *o.Workbench
			commands := *wb.Commands
			wb.Commands = &commands
			changed.Workbench = &wb
			tc.edit(&changed)
			system := tc.system
			if system == nil {
				system = []string{"/System", "/usr"}
			}
			key, err := workbenchProbeKey(changed, system)
			if err != nil || key == base {
				t.Fatalf("cache collision: %s %v", key, err)
			}
		})
	}
}

func TestWorkbenchCommandsNormalize(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Options)
	}{
		{"timeout", func(o *Options) { o.Workbench.Commands.Timeout = 11 * time.Minute }},
		{"environment", func(o *Options) { o.Workbench.Commands.Env = []string{"API_KEY=secret"} }},
		{"scratch environment", func(o *Options) { o.Workbench.Commands.Env = []string{"HOME=/elsewhere"} }},
		{"runtime read", func(o *Options) { o.Workbench.Commands.Read = []string{o.RuntimeHome} }},
		{"home read", func(o *Options) { home, _ := os.UserHomeDir(); o.Workbench.Commands.Read = []string{home} }},
		{"data-volume home read", func(o *Options) { o.Workbench.Commands.Read = []string{"/System/Volumes"} }},
		{"home workspace", func(o *Options) { o.WorkDir, _ = os.UserHomeDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := workbenchOptions(t, nopHandler())
			o.Workbench.Commands = &Commands{}
			tc.edit(&o)
			if _, err := normalize(o); err == nil {
				t.Fatal("not refused")
			}
			entries, _ := os.ReadDir(o.RuntimeHome)
			if len(entries) != 0 {
				t.Fatal("refusal touched state")
			}
		})
	}
}

func TestWorkbenchRealCanary(t *testing.T) {
	requireWorkbenchSeatbelt(t)
	for _, loop := range []bool{false, true} {
		for _, write := range []bool{false, true} {
			o := workbenchOptions(t, nopHandler())
			o.Workbench = &Workbench{Write: write, Commands: &Commands{Loopback: loop}}
			o, err := normalize(o)
			if err != nil {
				t.Fatal(err)
			}
			if err := proveWorkbench(context.Background(), o); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestWorkbenchWidenedProfileFailsProof(t *testing.T) {
	requireWorkbenchSeatbelt(t)
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if os.Mkdir(work, 0700) != nil {
		t.Fatal("workspace setup")
	}
	outside := filepath.Join(root, "outside")
	writeFile(t, outside, "marker")
	l := workbenchLayout{Work: work, Home: t.TempDir(), Tmp: t.TempDir(), System: workbenchSystemDirs(), Write: true}
	for _, tc := range []struct{ name, rule, script string }{
		{"read root", "(allow file-read* (subpath \"/\"))", "cat " + workbenchShellQuote(outside) + " >/dev/null 2>&1 && echo outside; echo canary-ran"},
		{"read owners", "(allow file-read* (subpath \"/Users\"))", "try() { name=$1; shift; if ( \"$@\" ) >/dev/null 2>&1; then echo \"$name\"; fi; }; " + workbenchReadWitnesses(root, mustWorkbenchHome(t)) + "echo canary-ran"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, p, err := process.Command(context.Background(), "/usr/bin/sandbox-exec", "-p", seatbeltProfile(l)+tc.rule, "/bin/sh", "-c", tc.script)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			out := &workbenchOutput{limit: 4096}
			cmd.Stdout = out
			cmd.Stderr = out
			cmd.WaitDelay = time.Second
			if err := p.Run(); err != nil {
				t.Fatal(err)
			}
			var cap *CapabilityError
			if err := judgeWorkbench(out.text(), true, false, false); !errors.As(err, &cap) || cap.Code != CapabilitySandboxNotEnforced {
				t.Fatalf("widened rule accepted: %s %v", out.text(), err)
			}
		})
	}
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
	cmd, p, err := process.Command(context.Background(), "/usr/bin/sandbox-exec", "-p", seatbeltProfile(l)+"(allow network*)", "/bin/sh", "-c", "nc -z -w 2 127.0.0.1 "+strconv.Itoa(port)+" && echo network; echo canary-ran")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	out := &workbenchOutput{limit: 4096}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = time.Second
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	var cap *CapabilityError
	if err := judgeWorkbench(out.text(), true, false, connections.Load() > 0); !errors.As(err, &cap) || cap.Code != CapabilitySandboxNotEnforced {
		t.Fatal("widened network accepted")
	}
}

func mustWorkbenchHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func TestWorkbenchMacOSEditAndRunSession(t *testing.T) {
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

func TestWorkbenchCommandBoundsAndCancellation(t *testing.T) {
	requireWorkbenchSeatbelt(t)
	o := workbenchOptions(t, nopHandler())
	o.Workbench = &Workbench{Write: true, Commands: &Commands{}}
	o, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := proveWorkbench(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	s := startAPI(t, o)
	defer closeAPI(t, s)
	w := s.api.workspace
	r, err := w.commands.run(context.Background(), "sleep 30", ".", 50*time.Millisecond)
	if err != nil || !strings.Contains(r.Content, `"timed_out":true`) {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = w.commands.run(context.Background(), "yes x | head -c 100000", ".", time.Second)
	if err != nil || !strings.Contains(r.Content, "output truncated") || len(r.Content) > w.budget {
		t.Fatalf("len %d, %v", len(r.Content), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := w.commands.run(ctx, "sleep 30 & wait", ".", time.Minute); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not settle")
	}
}

// Late writes must be consumed even after the result capture window closes.
func TestWorkbenchBackgroundLateOutputSurvives(t *testing.T) {
	requireWorkbenchSeatbelt(t)
	o := workbenchOptions(t, nopHandler())
	o.Workbench = &Workbench{Write: true, Commands: &Commands{}}
	s := startAPI(t, o)
	defer closeAPI(t, s)
	r, err := s.api.workspace.commands.run(context.Background(), "(sleep 3; echo late && echo late >&2 && touch alive && sleep 30) &", ".", 10*time.Second)
	if err != nil || r.IsError {
		t.Fatalf("%+v %v", r, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(o.WorkDir, "alive")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background stdout/stderr failed after tool return")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWorkbenchKilledSupervisorCleansGroup(t *testing.T) {
	requireWorkbenchSeatbelt(t)
	o := workbenchOptions(t, nopHandler())
	o.Workbench = &Workbench{Write: true, Commands: &Commands{}}
	s := startAPI(t, o)
	defer closeAPI(t, s)
	_, _ = s.api.workspace.commands.run(context.Background(), "sleep 60 >/dev/null 2>&1 & echo $! > victim; kill $PPID", ".", 10*time.Second)
	data, err := os.ReadFile(filepath.Join(o.WorkDir, "victim"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatal("invalid victim PID")
	}
	closeAPI(t, s)
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("background member survived supervisor death and Close")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
