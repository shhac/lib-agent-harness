package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

type workbenchLinuxProof struct {
	Witness workbenchLinuxWitness
	Output  string
	key     string
}

func proveWorkbench(ctx context.Context, o Options) error {
	if o.Workbench == nil || o.Workbench.Commands == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	// The trial uses disposable paths, before any session state exists.
	root, e := os.MkdirTemp("", "wb-trial-")
	if e != nil {
		return workbenchCapability(CapabilitySandboxUnavailable)
	}
	defer os.RemoveAll(root)
	l := workbenchLayout{Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), System: workbenchSystemDirs()}
	for _, p := range []string{l.Work, l.Home, l.Tmp} {
		if os.Mkdir(p, 0700) != nil {
			return workbenchCapability(CapabilitySandboxUnavailable)
		}
	}
	binary, version, e := checkBwrap(ctx, l)
	if ctx.Err() != nil {
		return workbenchCapability(CapabilityProbeTimeout)
	}
	if e != nil {
		return e
	}
	o.Workbench.commandBinary = binary
	identity, e := workbenchBinaryFingerprint(binary)
	if e != nil {
		return workbenchCapability(CapabilitySandboxUnavailable)
	}
	if o.Workbench.Write {
		info, e := os.Lstat(filepath.Join(o.WorkDir, ".git"))
		if e != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return refuse(o, "work_dir", RefusedWorkDir, "Linux writable command workspaces require an existing non-symlink .git for the read-only overlay")
		}
	}
	proof, e := probeWorkbenchLinux(ctx, o, binary, version)
	if ctx.Err() != nil {
		return workbenchCapability(CapabilityProbeTimeout)
	}
	if e != nil {
		return e
	}
	current, e := workbenchBinaryFingerprint(binary)
	if e != nil || current != identity {
		return workbenchCapability(CapabilitySandboxUnavailable)
	}
	if ctx.Err() != nil {
		return workbenchCapability(CapabilityProbeTimeout)
	}
	o.Workbench.commandIdentity = identity
	verified.record(proof.key)
	return nil
}

func linuxShape(path string) string {
	cover := "/"
	if lexicallyWithin("/tmp", path) {
		cover = "/tmp"
	}
	if lexicallyWithin("/run", path) {
		cover = "/run"
	}
	home, _ := os.UserHomeDir()
	if resolved, e := filepath.EvalSymlinks(home); e == nil {
		home = resolved
	}
	return fmt.Sprintf("%s:%t:%d", cover, lexicallyWithin(home, path), strings.Count(filepath.Clean(path), "/"))
}

func workbenchLinuxProbeKey(o Options, binary, version string, w workbenchLinuxWitness) (string, error) {
	info, e := os.Stat(binary)
	if e != nil {
		return "", e
	}
	data, e := os.ReadFile(binary)
	if e != nil {
		return "", e
	}
	binaryHash := sha256.Sum256(data)
	// Inspect owned fixtures only; canonicalize the random root for stable keys.
	root, e := os.MkdirTemp("", "wb-template-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(root)
	for _, name := range []string{"work/.git", "home", "tmp"} {
		if e := os.MkdirAll(filepath.Join(root, name), 0700); e != nil {
			return "", e
		}
	}
	template, e := bwrapArgs(workbenchLayout{Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), System: workbenchSystemDirs(), Write: true})
	if e != nil {
		return "", e
	}
	for i, arg := range template {
		template[i] = strings.ReplaceAll(arg, root, "/workbench-template")
	}
	var system []string
	for _, p := range workbenchSystemDirs() {
		i, e := os.Lstat(p)
		if os.IsNotExist(e) {
			system = append(system, p+":absent")
			continue
		}
		if e != nil {
			return "", e
		}
		target := ""
		if i.Mode()&os.ModeSymlink != 0 {
			target, e = os.Readlink(p)
			if e != nil {
				return "", e
			}
		}
		system = append(system, fmt.Sprintf("%s:%s:%d:%d:%s", p, i.Mode(), i.Size(), i.ModTime().UnixNano(), target))
	}
	shapes := []string{linuxShape(o.WorkDir), linuxShape(o.RuntimeHome), linuxShape(filepath.Join(o.RuntimeHome, "sessions", "id", "workbench", "home")), linuxShape(filepath.Join(o.RuntimeHome, "sessions", "id", "workbench", "tmp"))}
	for _, p := range o.Workbench.Commands.Read {
		shapes = append(shapes, linuxShape(p))
	}
	payload, _ := json.Marshal([]any{"workbench-linux", workbenchBwrapVersion, binary, info.Size(), info.ModTime(), binaryHash, version, template, system, shapes, o.Workbench.Write, o.Workbench.Commands.Read, o.Workbench.Commands.Env, o.Workbench.Commands.Loopback, o.Background, w})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:]), nil
}

// A fixture uses the same covering tmpfs, home placement and directory depth.
// All ancestors are disposable; no fixture binds the owner's real workspace.
func linuxProbePath(actual string) (path, root string, err error) {
	base := "/var/tmp"
	home, _ := os.UserHomeDir()
	if resolved, e := filepath.EvalSymlinks(home); e == nil {
		home = resolved
	}
	if lexicallyWithin(home, actual) {
		base = home
	} else if lexicallyWithin("/tmp", actual) {
		base = "/tmp"
	} else if lexicallyWithin("/run", actual) {
		base = "/run"
	}
	root, err = os.MkdirTemp(base, "wb-proof-")
	if err != nil {
		return
	}
	path = root
	depth := strings.Count(filepath.Clean(actual), "/")
	if strings.Count(path, "/") > depth {
		os.RemoveAll(root)
		err = fmt.Errorf("cannot reproduce sandbox depth")
		return
	}
	for strings.Count(path, "/") < depth {
		path = filepath.Join(path, "d")
	}
	err = os.MkdirAll(path, 0700)
	return
}

func runLinuxProbe(ctx context.Context, binary string, l workbenchLayout, script string, background bool) (string, error) {
	r, status, e := os.Pipe()
	if e != nil {
		return "", e
	}
	defer r.Close()
	defer status.Close()
	args, e := bwrapArgs(l)
	if e != nil {
		return "", e
	}
	args = append(args, "--json-status-fd", "3", "--chdir", l.Work, "--", "/bin/sh", "-c", script)
	if background {
		binary, args = linuxBackgroundLaunch(binary, args)
	}
	cmd, p, e := process.Command(ctx, binary, args...)
	if e != nil {
		return "", e
	}
	defer p.Close()
	cmd.ExtraFiles = []*os.File{status}
	p.Notify(func(int) { status.Close() })
	out := &workbenchOutput{limit: 64 << 10}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.Env = workbenchProbeEnvironment(l)
	cmd.WaitDelay = 2 * time.Second
	done := make(chan error, 1)
	go func() { e := p.Run(); status.Close(); done <- e }()
	started, code, known := readBwrapStatus(r, nil)
	e = <-done
	if e != nil {
		return out.text(), e
	}
	if !started || !known || code != 0 {
		return out.text(), fmt.Errorf("bubblewrap status protocol unavailable")
	}
	return out.text(), nil
}

func linuxConnect(ctx context.Context, command string) bool {
	cmd, p, e := process.Command(ctx, "/bin/sh", "-c", command)
	if e != nil {
		return false
	}
	defer p.Close()
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	// Never pass credentials or shell startup hooks to outside controls.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.WaitDelay = time.Second
	return p.Run() == nil
}
func linuxTCP(host string, port int) string {
	return "/usr/bin/bash -c " + workbenchShellQuote("exec 3<>/dev/tcp/"+host+"/"+strconv.Itoa(port))
}

func linuxSocketWitness(ctx context.Context, command string, observed <-chan struct{}) (string, error) {
	if !linuxConnect(ctx, command) {
		if ctx.Err() != nil {
			return "", workbenchCapability(CapabilityProbeTimeout)
		}
		return "structural", nil
	}
	select {
	case <-observed:
		return "connect", nil
	case <-ctx.Done():
		return "", workbenchCapability(CapabilitySandboxUnavailable)
	}
}

func probeWorkbenchLinux(ctx context.Context, o Options, binary, version string) (result workbenchLinuxProof, err error) {
	unavailable := workbenchCapability(CapabilitySandboxUnavailable)
	if o.Workbench.Commands.Loopback {
		found := false
		for _, path := range []string{"/usr/bin/nc", "/bin/nc"} {
			if info, e := os.Stat(path); e == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				found = true
				break
			}
		}
		if !found {
			err := workbenchCapability(CapabilitySandboxToolMissing)
			err.Tools = []string{"nc"}
			return result, err
		}
	}
	var roots []string
	defer func() {
		for _, r := range roots {
			os.RemoveAll(r)
		}
	}()
	mapped := map[string]string{}
	targets := append([]string{o.WorkDir, o.RuntimeHome, filepath.Join(o.RuntimeHome, "sessions", "probe", "workbench", "home"), filepath.Join(o.RuntimeHome, "sessions", "probe", "workbench", "tmp")}, o.Workbench.Commands.Read...)
	shallowPaths(targets)
	for _, actual := range targets {
		if _, ok := mapped[actual]; ok {
			continue
		}
		path := ""
		for _, ancestor := range targets {
			if existing, ok := mapped[ancestor]; ok && lexicallyWithin(ancestor, actual) {
				suffix, e := filepath.Rel(ancestor, actual)
				if e != nil {
					return result, unavailable
				}
				path = filepath.Join(existing, suffix)
				break
			}
		}
		if path == "" {
			p, r, e := linuxProbePath(actual)
			if r != "" {
				roots = append(roots, r)
			}
			if e != nil {
				return result, unavailable
			}
			path = p
		}
		if os.MkdirAll(path, 0700) != nil {
			return result, unavailable
		}
		mapped[actual] = path
	}
	work := mapped[o.WorkDir]
	scratch := mapped[filepath.Join(o.RuntimeHome, "sessions", "probe", "workbench", "home")]
	tmp := mapped[filepath.Join(o.RuntimeHome, "sessions", "probe", "workbench", "tmp")]
	read, e := os.MkdirTemp("/tmp", "wb-read-")
	if e != nil {
		return result, unavailable
	}
	roots = append(roots, read)
	l := workbenchLayout{Work: work, Home: scratch, Tmp: tmp, System: workbenchSystemDirs(), Write: o.Workbench.Write, Loopback: o.Workbench.Commands.Loopback, Read: []string{read}}
	runtimeMarker := filepath.Join(mapped[o.RuntimeHome], "transcript")
	if os.WriteFile(runtimeMarker, []byte("marker"), 0600) != nil {
		return result, unavailable
	}
	sibling := filepath.Join(filepath.Dir(work), filepath.Base(work)+"-sibling-write")
	if _, e = os.Lstat(sibling); !os.IsNotExist(e) {
		return result, unavailable
	}
	defer os.Remove(sibling)
	// Caller read shapes are reproduced without reading their actual contents.
	for _, actual := range o.Workbench.Commands.Read {
		l.Read = append(l.Read, mapped[actual])
	}
	git := filepath.Join(work, ".git", "config")
	if os.Mkdir(filepath.Dir(git), 0700) != nil || os.WriteFile(git, []byte("marker\n"), 0600) != nil || os.WriteFile(filepath.Join(read, "marker"), []byte("marker"), 0600) != nil {
		return result, unavailable
	}
	hiddenBase := "/tmp"
	for _, actual := range o.Workbench.Commands.Read {
		if lexicallyWithin(actual, "/tmp") {
			hiddenBase = mapped[actual]
			break
		}
	}
	hidden, e := os.MkdirTemp(hiddenBase, "wb-hidden-")
	if e != nil {
		return result, unavailable
	}
	roots = append(roots, hidden)
	for _, name := range []string{"home", "outside", "runtime"} {
		if os.WriteFile(filepath.Join(hidden, name), []byte("marker"), 0600) != nil {
			return result, unavailable
		}
	}
	namespace, e := os.Readlink("/proc/self/ns/net")
	if e != nil {
		return result, unavailable
	}
	listener, wg, reached, e := linuxCountingListener("127.0.0.1:0")
	if e != nil {
		return result, unavailable
	}
	defer func() { listener.Close(); wg.Wait() }()
	port := listener.Addr().(*net.TCPAddr).Port
	socket := filepath.Join(hidden, "socket")
	unixListener, e := net.Listen("unix", socket)
	if e != nil {
		return result, unavailable
	}
	socketReached := make(chan struct{}, 16)
	acceptDone := make(chan struct{})
	defer func() {
		if unixListener != nil {
			unixListener.Close()
		}
		<-acceptDone
	}()
	startSocket := func(listener net.Listener, done chan struct{}) {
		go func() {
			defer close(done)
			for {
				c, e := listener.Accept()
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
	}
	startSocket(unixListener, acceptDone)
	w := workbenchLinuxWitness{Network: "structural", Localhost: "structural", Socket: "structural"}
	socketCommand := "/usr/bin/nc -U -w 2 " + workbenchShellQuote(socket) + " </dev/null"
	w.Socket, e = linuxSocketWitness(ctx, socketCommand, socketReached)
	if e != nil {
		return result, e
	}
	// A baseline client may connect, then exit nonzero. Join its accept loop
	// before draining and recreating the listener so late accepts cannot escape.
	unixListener, e = linuxResetSocketBaseline(unixListener, acceptDone, socketReached, socket)
	acceptDone = make(chan struct{})
	if e != nil {
		close(acceptDone)
		return result, unavailable
	}
	startSocket(unixListener, acceptDone)
	network, local := "", ""
	if _, e := os.Stat("/usr/bin/bash"); e == nil {
		witness, ok := offMachineWitness(ctx)
		if !ok {
			return result, unavailable
		}
		network, local = linuxTCP(witness, 443), linuxTCP("127.0.0.1", port)
		if !linuxConnect(ctx, network) || !linuxConnect(ctx, local) {
			return result, unavailable
		}
		w.Network, w.Localhost = "connect", "connect"
	} else if !os.IsNotExist(e) {
		return result, unavailable
	}
	result.Witness = w
	// Positive controls must actually have been observed; reset host counts.
	for w.Localhost == "connect" && reached() == 0 {
		select {
		case <-ctx.Done():
			return result, unavailable
		case <-time.After(time.Millisecond):
		}
	}
	baseline := reached()
	key, e := workbenchLinuxProbeKey(o, binary, version, w)
	if e != nil {
		return result, unavailable
	}
	// Always run the current trial and canary. Records identify evidence but
	// are not reused across changes to namespaces, listeners or mount state.
	workbenchCanaryRuns.Add(1)
	script := linuxWorkbenchCanary(l, hidden, read, socket, namespace, w, network, local, runtimeMarker)
	// Workspace parents are private tmpfs directories. A write there may
	// succeed inside; only a corresponding host file would escape confinement.
	script += "try sibling-tmpfs /bin/sh -c " + workbenchShellQuote("echo x > "+workbenchShellQuote(sibling)) + "\n"
	if o.Background {
		script += "[ \"$(ps -o ni= -p $$ | tr -d ' ')\" = 10 ] && echo background\n"
	}
	script += "echo canary-ran\n"
	result.Output, e = runLinuxProbe(ctx, binary, l, script, o.Background)
	if e != nil {
		return result, unavailable
	}
	data, e := os.ReadFile(git)
	if e != nil || !bytes.Equal(data, []byte("marker\n")) {
		return result, workbenchCapability(CapabilitySandboxNotEnforced)
	}
	if _, e = os.Lstat(filepath.Join(hidden, "sibling-write")); !os.IsNotExist(e) {
		return result, workbenchCapability(CapabilitySandboxNotEnforced)
	}
	if _, e = os.Lstat(sibling); !os.IsNotExist(e) {
		return result, workbenchCapability(CapabilitySandboxNotEnforced)
	}
	if l.Write {
		if _, e = os.Stat(filepath.Join(work, "deep", "nested", "file")); e != nil {
			return result, unavailable
		}
	}
	// Give the accept goroutines an opportunity to report successful connects.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-socketReached:
		return result, workbenchCapability(CapabilitySandboxNotEnforced)
	default:
	}
	if e = judgeLinuxWorkbench(result.Output, l.Write, reached() != baseline, o.Background, l.Loopback); e != nil {
		return result, e
	}
	result.key = key
	return result, nil
}

func linuxResetSocketBaseline(listener net.Listener, done <-chan struct{}, observed chan struct{}, path string) (net.Listener, error) {
	listener.Close()
	<-done
	for len(observed) > 0 {
		<-observed
	}
	return net.Listen("unix", path)
}

func linuxCountingListener(address string) (net.Listener, *sync.WaitGroup, func() int, error) {
	l, e := net.Listen("tcp", address)
	if e != nil {
		return nil, nil, nil, e
	}
	var n atomic.Int64
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			n.Add(1)
			c.Close()
		}
	}()
	return l, wg, func() int { return int(n.Load()) }, nil
}
