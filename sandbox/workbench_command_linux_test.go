package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

func TestWorkbenchLinuxRealCanary(t *testing.T) {
	binary, version := requireWorkbenchBwrap(t)
	home, e := os.UserHomeDir()
	if e != nil {
		t.Fatal(e)
	}
	for _, runtimeBase := range []string{home, "/var/tmp"} {
		for _, workBase := range []string{home, "/tmp"} {
			t.Run(runtimeBase+workBase, func(t *testing.T) {
				work, e := os.MkdirTemp(workBase, "wb-work-")
				if e != nil {
					t.Fatal(e)
				}
				defer os.RemoveAll(work)
				runtimeHome, e := os.MkdirTemp(runtimeBase, "wb-runtime-")
				if e != nil {
					t.Fatal(e)
				}
				defer os.RemoveAll(runtimeHome)
				o := workbenchOptions(t, nopHandler())
				o.WorkDir, o.RuntimeHome = work, runtimeHome
				o.Workbench = &Workbench{Write: true, Commands: &Commands{}}
				ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout)
				defer cancel()
				result, e := probeWorkbenchLinux(ctx, testCommandOptions(o), binary, version)
				if e != nil {
					t.Fatalf("%v: %s", e, result.Output)
				}
				if result.Witness.Network == "" || result.Witness.Socket == "" || result.Witness.Localhost == "" {
					t.Fatal("witnesses not recorded")
				}
				entries, _ := os.ReadDir(runtimeHome)
				if len(entries) != 0 {
					t.Fatal("probe wrote session state")
				}
			})
		}
	}
}

func TestWorkbenchLinuxStructuralProof(t *testing.T) {
	binary, _ := requireWorkbenchBwrap(t)
	l := linuxTestLayout(t)
	namespace, e := os.Readlink("/proc/self/ns/net")
	if e != nil {
		t.Fatal(e)
	}
	socket := filepath.Join(t.TempDir(), "socket")
	listener, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	script := linuxStructuralWitness(namespace, socket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, e := runLinuxProbe(ctx, binary, l, "( "+script+" ) && echo proved", false)
	if e != nil || !strings.Contains(output, "proved") {
		t.Fatalf("%s %v", output, e)
	}
	// Each failed or unreadable observation must fail the conjunction. The
	// socket's own parent is absent inside, which already proves it out of
	// reach, so its check is broken by naming a path the sandbox can see.
	for _, replacement := range [][2]string{{"/proc/self/ns/net", "/proc/absent"}, {"/proc/net/dev", "/proc/absent"}, {linuxSocketAbsent(socket), linuxSocketAbsent("/etc/passwd")}, {workbenchShellQuote(namespace), "\"$n\""}} {
		broken := strings.Replace(script, replacement[0], replacement[1], 1)
		output, e := runLinuxProbe(ctx, binary, l, "( "+broken+" ) && echo proved; exit 0", false)
		if e != nil || strings.Contains(output, "proved") {
			t.Fatalf("failed structural component accepted: %s %v", output, e)
		}
	}
}

func TestWorkbenchLinuxWidenedSandbox(t *testing.T) {
	binary, _ := requireWorkbenchBwrap(t)
	l := linuxTestLayout(t)
	read := t.TempDir()
	l.Read = []string{read}
	hidden := t.TempDir()
	if e := os.Mkdir(filepath.Join(l.Work, ".git"), 0700); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{filepath.Join(l.Work, ".git", "config"), filepath.Join(read, "marker"), filepath.Join(hidden, "home"), filepath.Join(hidden, "outside"), filepath.Join(hidden, "runtime")} {
		if e := os.WriteFile(p, []byte("marker"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	namespace, e := os.Readlink("/proc/self/ns/net")
	if e != nil {
		t.Fatal(e)
	}
	script := linuxWorkbenchCanary(l, hidden, read, filepath.Join(hidden, "socket"), namespace, workbenchLinuxWitness{"structural", "structural", "structural"}, "", "") + "echo canary-ran\n"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, p, e := process.Command(ctx, binary, "--bind", "/", "/", "--dev", "/dev", "--chdir", l.Work, "--", "/bin/sh", "-c", script)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	cmd.Env = append(os.Environ(), "HOME="+l.Home, "TMPDIR="+l.Tmp)
	out := &workbenchOutput{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = time.Second
	if e = p.Run(); e != nil {
		t.Fatalf("%v %s", e, out.text())
	}
	var cap *ProofError
	if e = judgeLinuxWorkbench(out.text(), true, false, false, false); !errors.As(e, &cap) || cap.Code != CapabilitySandboxNotEnforced {
		t.Fatalf("accepted widened sandbox: %v %s", e, out.text())
	}
	for _, label := range []string{"gitdir", "home", "outside", "runtime", "readset-write", "socket", "network", "localhost"} {
		if !strings.Contains("\n"+out.text(), "\n"+label+"\n") {
			t.Errorf("missing escape %s: %s", label, out.text())
		}
	}
}

func linuxCommandRunner(t *testing.T, background bool, edits ...func(*testOptions)) (*workbenchHost, testOptions) {
	t.Helper()
	binary, _ := requireWorkbenchBwrap(t)
	work := filepath.Join(t.TempDir(), "work")
	writeFile(t, filepath.Join(work, "a.txt"), "inside\n")
	w, err := openWorkspace(testOptions{WorkDir: work, Workbench: &Workbench{}, Restriction: &Restriction{Tools: ToolHost{}}}, newID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.close)
	writeFile(t, filepath.Join(work, ".git", "config"), "metadata\n")
	o := workbenchOptions(t, nopHandler())
	o.WorkDir = work
	o.Background = background
	o.Workbench = &Workbench{Write: true, Commands: &Commands{Timeout: time.Minute}, system: workbenchSystemDirs(), commandBinary: binary}
	identity, e := workbenchBinaryFingerprint(binary)
	if e != nil {
		t.Fatal(e)
	}
	o.Workbench.commandIdentity = identity
	for _, edit := range edits {
		edit(&o)
	}
	id := newID()
	if e := os.MkdirAll(sessionDir(o.RuntimeHome, id), 0700); e != nil {
		t.Fatal(e)
	}
	if e := setupWorkbenchCommands(w, o, id); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := w.commands.close(); e != nil {
			t.Error(e)
		}
	})
	return w, o
}

func TestWorkbenchLinuxCleanup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		cancel  bool
	}{
		{"return", 5 * time.Second, false},
		{"timeout", 2 * time.Second, false},
		{"cancel", 5 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, o := linuxCommandRunner(t, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Both the shell and descendants inherit the durable session token.
			marker := "workbench-settle-" + process.NewToken()
			command := "/bin/sh -c 'sleep 300 & wait' " + workbenchShellQuote(marker) + " & echo ready > ready; while [ ! -e release ]; do sleep .02; done"
			type outcome struct {
				r ToolResult
				e error
			}
			finished := make(chan outcome, 1)
			go func() { r, e := w.commands.run(ctx, command, ".", tc.timeout); finished <- outcome{r, e} }()
			observed := waitLinuxCommandTree(t, marker)
			if tc.cancel {
				cancel()
			} else if tc.name == "return" {
				if e := os.WriteFile(filepath.Join(o.WorkDir, "release"), []byte("release"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			done := <-finished
			result, e := done.r, done.e
			if tc.cancel {
				if !errors.Is(e, context.Canceled) {
					t.Fatalf("%v", e)
				}
			} else if e != nil || result.IsError {
				t.Fatalf("%+v %v", result, e)
			}
			if tc.name == "timeout" && !strings.Contains(result.Content, `"timed_out":true`) {
				t.Fatal(result)
			}
			entries, e := os.ReadDir(filepath.Join(o.RuntimeHome, "sessions"))
			if e != nil || len(entries) != 1 {
				t.Fatal(e)
			}
			data, e := os.ReadFile(filepath.Join(o.RuntimeHome, "sessions", entries[0].Name(), "workbench-token.json"))
			if e != nil {
				t.Fatal(e)
			}
			var token workbenchToken
			if json.Unmarshal(data, &token) != nil {
				t.Fatal("token")
			}
			if process.TokenPresent(token.Token, token.Since) {
				t.Fatal("marked process survived command")
			}
			waitLinuxTreeGone(t, observed)
			if e = w.commands.close(); e != nil {
				t.Fatal(e)
			}
		})
	}
}

type linuxProcessWitness struct {
	pid, parent   int
	birth, state  string
	marker, sleep bool
}

func linuxProcessWitnesses(marker string) map[int]linuxProcessWitness {
	entries, _ := os.ReadDir("/proc")
	all := map[int]linuxProcessWitness{}
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil {
			continue
		}
		data, e := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if e != nil {
			continue
		}
		i := strings.LastIndexByte(string(data), ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(data[i+1:]))
		if len(fields) < 20 || fields[0] == "Z" {
			continue
		}
		parent, e := strconv.Atoi(fields[1])
		if e != nil {
			continue
		}
		args, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		all[pid] = linuxProcessWitness{pid, parent, fields[19], fields[0], strings.Contains(string(args), marker), strings.HasPrefix(string(args), "sleep\x00")}
	}
	return all
}

// Positive live birth identities, including the sleep grandchild, keep an
// unreadable token environment from masquerading as successful cleanup.
func waitLinuxCommandTree(t *testing.T, marker string) map[int]linuxProcessWitness {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		all := linuxProcessWitnesses(marker)
		tree := map[int]linuxProcessWitness{}
		for pid, p := range all {
			if p.marker {
				tree[pid] = p
			}
		}
		for changed := true; changed; {
			changed = false
			for pid, p := range all {
				if _, ok := tree[p.parent]; ok {
					if _, ok = tree[pid]; !ok {
						tree[pid] = p
						changed = true
					}
				}
			}
		}
		for _, p := range tree {
			if p.sleep {
				return tree
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no positive live sleep descendant witness")
	return nil
}

func waitLinuxTreeGone(t *testing.T, observed map[int]linuxProcessWitness) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		live := linuxProcessWitnesses("no-such-marker")
		remaining := false
		for pid, p := range observed {
			if now, ok := live[pid]; ok && now.birth == p.birth {
				remaining = true
			}
		}
		if !remaining {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a positively identified command descendant survived settlement")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The caller's ordinary settings reach every command, beside the private
// scratch HOME and TMPDIR the library always sets.
func TestWorkbenchLinuxCommandEnvironment(t *testing.T) {
	w, o := linuxCommandRunner(t, false, func(o *testOptions) {
		o.Workbench.Commands.Env = []string{"GOFLAGS=-mod=mod", "PORT=4321", "npm_config_cache=/nowhere/npm"}
	})
	r, e := w.commands.run(context.Background(), `printf '%s|%s|%s|%s' "$GOFLAGS" "$PORT" "$npm_config_cache" "$HOME"`, ".", 5*time.Second)
	var out struct{ Stdout string }
	if e != nil || r.IsError || json.Unmarshal([]byte(r.Content), &out) != nil {
		t.Fatalf("%+v %v", r, e)
	}
	fields := strings.Split(out.Stdout, "|")
	if len(fields) != 4 || fields[0] != "-mod=mod" || fields[1] != "4321" || fields[2] != "/nowhere/npm" || fields[3] == "" || fields[3] == os.Getenv("HOME") {
		t.Fatalf("environment %q (runtime %s)", out.Stdout, o.RuntimeHome)
	}
}

func TestWorkbenchLinuxBackgroundPriority(t *testing.T) {
	w, _ := linuxCommandRunner(t, true)
	r, e := w.commands.run(context.Background(), "ps -o ni= -p $$", ".", 5*time.Second)
	if e != nil || r.IsError {
		t.Fatalf("%+v %v", r, e)
	}
	var out struct{ Stdout string }
	if json.Unmarshal([]byte(r.Content), &out) != nil || strings.TrimSpace(out.Stdout) != "10" {
		t.Fatal(r)
	}
}

func TestWorkbenchLinuxBackgroundInheritedPriority(t *testing.T) {
	binary, _ := requireWorkbenchBwrap(t)
	l := linuxTestLayout(t)
	for _, initial := range []int{5, 10} {
		t.Run(strconv.Itoa(initial), func(t *testing.T) {
			args, e := bwrapArgs(l)
			if e != nil {
				t.Fatal(e)
			}
			args = append(args, "--", "/bin/sh", "-c", "ps -o ni= -p $$")
			name, args := linuxBackgroundLaunch(binary, args)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd, p, e := process.Command(ctx, "/usr/bin/nice", append([]string{"-n", strconv.Itoa(initial), name}, args...)...)
			if e != nil {
				t.Fatal(e)
			}
			defer p.Close()
			cmd.Env = workbenchProbeEnvironment(l)
			out := &workbenchOutput{limit: 4096}
			cmd.Stdout, cmd.Stderr = out, out
			cmd.WaitDelay = time.Second
			if e = p.Run(); e != nil || strings.TrimSpace(out.text()) != "10" {
				t.Fatalf("inherited nice %d: %v %s", initial, e, out.text())
			}
		})
	}
}

func TestWorkbenchLinuxKillingBwrapKillsSandbox(t *testing.T) {
	binary, _ := requireWorkbenchBwrap(t)
	l := linuxTestLayout(t)
	args, e := bwrapArgs(l)
	if e != nil {
		t.Fatal(e)
	}
	args = append(args, "--chdir", l.Work, "--", "/bin/sh", "-c", "sleep 300 & wait")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, p, e := process.Command(ctx, binary, args...)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	token := process.NewToken()
	since := time.Now()
	cmd.Env = process.TokenEnvironment(os.Environ(), token)
	ready := make(chan int, 1)
	p.Notify(func(pid int) { ready <- pid })
	done := make(chan error, 1)
	go func() { done <- p.Run() }()
	var pid int
	select {
	case pid = <-ready:
	case <-ctx.Done():
		t.Fatal("bwrap did not start")
	}
	time.Sleep(100 * time.Millisecond)
	if e = syscall.Kill(pid, syscall.SIGKILL); e != nil {
		t.Fatal(e)
	}
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for process.TokenPresent(token, since) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if process.TokenPresent(token, since) {
		t.Fatal("sandbox survived bwrap death")
	}
}

func TestWorkbenchLinuxLaunchFailureAfterProof(t *testing.T) {
	w, o := linuxCommandRunner(t, false)
	// Replace only the selected path, never an installed bwrap binary.
	fake := filepath.Join(t.TempDir(), "bwrap")
	if e := os.WriteFile(fake, []byte("#!/bin/sh\necho unavailable >&2; exit 1\n"), 0700); e != nil {
		t.Fatal(e)
	}
	o.Workbench.commandBinary = fake
	identity, e := workbenchBinaryFingerprint(fake)
	if e != nil {
		t.Fatal(e)
	}
	o.Workbench.commandIdentity = identity
	id := newID()
	if e := os.MkdirAll(sessionDir(o.RuntimeHome, id), 0700); e != nil {
		t.Fatal(e)
	}
	if e := w.commands.close(); e != nil {
		t.Fatal(e)
	}
	if e := setupWorkbenchCommands(w, o, id); e != nil {
		t.Fatal(e)
	}
	r, e := w.commands.run(context.Background(), "echo must-not-run", ".", time.Second)
	if e != nil || !strings.Contains(r.Content, "command_start_failed") {
		t.Fatalf("%+v %v", r, e)
	}
	if e = os.Remove(fake); e != nil {
		t.Fatal(e)
	}
	r, e = w.commands.run(context.Background(), "true", ".", time.Second)
	if e != nil || !strings.Contains(r.Content, "command_start_failed") {
		t.Fatalf("%+v %v", r, e)
	}
	if e = os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' '{\"child-pid\":123}' >&3\nexit 1\n"), 0700); e != nil {
		t.Fatal(e)
	}
	// Replacing the proved binary at the same path is refused.
	r, e = w.commands.run(context.Background(), "true", ".", time.Second)
	if e != nil || !strings.Contains(r.Content, "command_start_failed") {
		t.Fatalf("changed binary was launched: %+v %v", r, e)
	}
	if e = w.commands.close(); e != nil {
		t.Fatal(e)
	}
	identity, e = workbenchBinaryFingerprint(fake)
	if e != nil {
		t.Fatal(e)
	}
	o.Workbench.commandIdentity = identity
	if e = setupWorkbenchCommands(w, o, id); e != nil {
		t.Fatal(e)
	}
	r, e = w.commands.run(context.Background(), "true", ".", time.Second)
	if !errors.Is(e, errWorkbenchCommandUnknown) || !strings.Contains(r.Content, "command_outcome_unknown") {
		t.Fatalf("%+v %v", r, e)
	}
}

// Keep tests independent of nc variants: the recorded structural witness is
// judged with all three observations, even when the installed nc lacks -U.
func TestWorkbenchLinuxWitnessRecording(t *testing.T) {
	script := linuxWorkbenchCanary(workbenchLayout{Tmp: "/scratch"}, "/hidden", "/read", "/socket", "host-ns", workbenchLinuxWitness{"structural", "structural", "structural"}, "", "")
	for _, label := range []string{"network", "localhost", "socket"} {
		if !strings.Contains(script, "witness-"+label+"-structural") || !strings.Contains(script, label+"-structural-ok") {
			t.Fatal("witness missing")
		}
	}
	if !strings.Contains(script, "/proc/self/ns/net") || !strings.Contains(script, "/proc/net/dev") {
		t.Fatal("partial structural witness")
	}
	// Existing kernel namespace inode spelling is stable enough to compare as
	// a readlink target; test that the value is actually an inode, not a path.
	namespace, e := os.Readlink("/proc/self/ns/net")
	if e != nil {
		t.Fatal(e)
	}
	inode := strings.TrimSuffix(strings.TrimPrefix(namespace, "net:["), "]")
	if _, e = strconv.ParseUint(inode, 10, 64); e != nil {
		t.Fatal(namespace)
	}
}

func TestWorkbenchLinuxSocketClientSelection(t *testing.T) {
	for _, script := range []string{"", "#!/bin/sh\nexit 1\n"} {
		path := t.TempDir()
		t.Setenv("PATH", path)
		if script != "" {
			if e := os.WriteFile(filepath.Join(path, "nc"), []byte(script), 0700); e != nil {
				t.Fatal(e)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		witness, e := linuxSocketWitness(ctx, "nc -U -w 2 /unavailable </dev/null", make(chan struct{}))
		cancel()
		if e != nil || witness != "structural" {
			t.Fatalf("%s %v", witness, e)
		}
	}
	// A zero exit without the observed positive connect is never proof.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, e := linuxSocketWitness(ctx, "exit 0", make(chan struct{})); e == nil {
		t.Fatal("unobserved client success accepted")
	}
	observed := make(chan struct{}, 1)
	observed <- struct{}{}
	witness, e := linuxSocketWitness(context.Background(), "exit 0", observed)
	if e != nil || witness != "connect" {
		t.Fatalf("%s %v", witness, e)
	}
}

func TestWorkbenchLinuxSocketBaselineNonzeroAfterConnect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "socket")
	l, e := net.Listen("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	observed := make(chan struct{}, 1)
	done := make(chan struct{})
	accepted := make(chan struct{})
	go func() {
		defer close(done)
		c, e := l.Accept()
		if e == nil {
			c.Close()
			observed <- struct{}{}
		}
		close(accepted)
	}()
	c, e := net.Dial("unix", path)
	if e != nil {
		l.Close()
		<-done
		t.Fatal(e)
	}
	c.Close()
	<-accepted
	w, e := linuxSocketWitness(context.Background(), "exit 1", observed)
	if e != nil || w != "structural" {
		l.Close()
		t.Fatalf("%s %v", w, e)
	}
	next, e := linuxResetSocketBaseline(l, done, observed, path)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close()
	if len(observed) != 0 {
		t.Fatal("outside positive observation survived reset")
	}
	c, e = net.Dial("unix", path)
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
	c, e = next.Accept()
	if e != nil {
		t.Fatal(e)
	}
	c.Close() // The replacement still detects inside connections.
}

func TestWorkbenchLinuxStatusPipeIsPrivate(t *testing.T) {
	w, _ := linuxCommandRunner(t, false)
	r, e := w.commands.run(context.Background(), "printf '%s\\n' '{\"exit-code\":0}' >&3; echo actual; exit 7", ".", 5*time.Second)
	var result struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
	}
	if e != nil || r.IsError || json.Unmarshal([]byte(r.Content), &result) != nil || result.ExitCode != 7 || strings.TrimSpace(result.Stdout) != "actual" {
		t.Fatalf("%+v %v", r, e)
	}
}

func TestWorkbenchLinuxCanaryWithoutSandboxReportsEscapes(t *testing.T) {
	l := linuxTestLayout(t)
	hidden, read, tools := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(l.Work, ".git", "config"), "marker\n")
	writeFile(t, filepath.Join(read, "marker"), "marker\n")
	for _, label := range []string{"home", "outside", "runtime"} {
		writeFile(t, filepath.Join(hidden, label), "marker\n")
	}
	// Synthetic privileged tools exercise reporting without real host mounts.
	for _, tool := range []string{"umount", "mount"} {
		if e := os.WriteFile(filepath.Join(tools, tool), []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.WriteFile(filepath.Join(tools, "unshare"), []byte("#!/bin/sh\nshift; exec \"$@\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	x := filepath.Join(l.Tmp, "x")
	writeFile(t, filepath.Join(x, ".git", "config"), "marker\n")
	namespace, e := os.Readlink("/proc/self/ns/net")
	if e != nil {
		t.Fatal(e)
	}
	script := linuxWorkbenchCanary(l, hidden, read, filepath.Join(hidden, "socket"), namespace, workbenchLinuxWitness{"structural", "structural", "structural"}, "", "") + "echo canary-ran\n"
	script = strings.ReplaceAll(script, "/tmp/x", x)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, p, e := process.Command(ctx, "/bin/sh", "-c", script)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	cmd.Dir = l.Work
	cmd.Env = append(os.Environ(), "PATH="+tools+":/usr/bin:/bin", "TMPDIR="+l.Tmp)
	cmd.WaitDelay = time.Second
	out := &workbenchOutput{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	if e = p.Run(); e != nil {
		t.Fatalf("%v %s", e, out.text())
	}
	lines, _ := workbenchCanaryLines(out.text())
	for _, label := range []string{"inside", "nested", "tmp", "tmpdir", "sibling", "gitdir", "gitmove", "gitlink", "githardlink", "home", "outside", "runtime", "readset-write", "link", "socket", "network", "localhost", "privilege", "overlay"} {
		if !lines[label] {
			t.Errorf("escape %s not reported: %s", label, out.text())
		}
	}
	var cap *ProofError
	if e = judgeLinuxWorkbench(out.text(), true, false, false, false); !errors.As(e, &cap) || cap.Code != CapabilitySandboxNotEnforced {
		t.Fatalf("%v", e)
	}
}
