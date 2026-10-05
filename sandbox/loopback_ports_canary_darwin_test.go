package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
	"github.com/shhac/lib-agent-harness/process"
)

// Retained from 4b7e9d6 for direct tests only. Production has no reference to
// this canary while normalization refuses selected ports.

type portAttempt struct {
	Kind  string
	Addr  string
	Port  int
	Label string
}

// The installed Python socket client reports errno directly. Missing Python,
// unreadable runtime dependencies, timeouts and partial output refuse proof.
// This disposable helper changes neither PATH nor the command profile's reads.
const portClient = `import socket, errno, json, sys, signal
def deadline(signum, frame): raise TimeoutError()
signal.signal(signal.SIGALRM, deadline)
def attempt(t):
    kind, addr, port, label = t['Kind'], t['Addr'], t['Port'], t['Label']
    s = c = a = None
    phase = 'socket'
    signal.setitimer(signal.ITIMER_REAL, 1.5)
    try:
        s = socket.socket(socket.AF_INET6 if ':' in addr else socket.AF_INET, socket.SOCK_DGRAM if kind in ('udp', 'udp-bind') else socket.SOCK_STREAM)
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        s.settimeout(0.8)
        if kind in ('bind', 'udp-bind', 'positive-bind'):
            phase = 'bind'
            s.bind((addr, port))
            if kind == 'positive-bind':
                phase = 'listen'; s.listen(1)
                phase = 'connect'; c = socket.create_connection((addr, port), 0.8)
                phase = 'send'; c.sendall(b'harness-positive')
                phase = 'accept'; a, _ = s.accept(); a.settimeout(0.8)
                phase = 'receipt'; data = a.makefile('rb').read(len(b'harness-positive'))
                if data != b'harness-positive': raise RuntimeError()
        elif kind == 'udp':
            phase = 'send'; s.sendto(bytes([0]), (addr, port))
        else:
            phase = 'connect'; s.connect((addr, port))
        print(('positive-' if kind.startswith('positive') else 'escape-') + label, flush=True)
        return True
    except OSError as e:
        if not kind.startswith('positive') and phase in ('bind', 'connect', 'send') and e.errno in (errno.EPERM, errno.EACCES):
            print('denied-' + label, flush=True)
            return True
        print('unavailable-' + label + '-' + phase + '-errno-' + str(e.errno), flush=True)
        return not kind.startswith('positive')
    except Exception:
        print('unavailable-' + label + '-' + phase, flush=True)
        return not kind.startswith('positive')
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        for conn in (a, c, s):
            if conn is not None: conn.close()
for t in json.loads(sys.argv[1]):
    if not attempt(t): sys.exit(1)
print('ports-ran', flush=True)
`

func portScript(attempts []portAttempt) string {
	data, _ := json.Marshal(attempts)
	return "LC_ALL=C /usr/bin/python3 -c " + workbenchShellQuote(portClient) + " " + workbenchShellQuote(string(data)) + "\n"
}

func judgePortCanary(output string, attempts []portAttempt, inbound bool) error {
	lines := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		lines[line] = true
		if strings.HasPrefix(line, "escape-") {
			return &ProofError{Code: CapabilitySandboxNotEnforced, Step: proofStepSelectedPortNetwork}
		}
	}
	if !lines["ports-ran"] || !lines[canaryRan] {
		return &ProofError{Code: CapabilitySandboxUnavailable, Step: proofStepSelectedPortNetwork}
	}
	if inbound && (!lines["inbound"] || !lines["receipt"]) {
		return &ProofError{Code: CapabilitySandboxUnavailable, Step: proofStepSelectedPortNetwork}
	}
	for _, t := range attempts {
		prefix := "denied-"
		if strings.HasPrefix(t.Kind, "positive") {
			prefix = "positive-"
		}
		if !lines[prefix+t.Label] {
			return &ProofError{Code: CapabilitySandboxUnavailable, Step: proofStepSelectedPortNetwork}
		}
	}
	return nil
}

// Stage A proves the rule generator with disposable free selected ports.
// Stage B proves the caller's exact list, never contacting a busy selected port.
type selectedPortDeps struct {
	control func(context.Context, string) (sandboxprobe.ControlTarget, string)
	witness func(context.Context) (string, bool)
	ready   func(context.Context) error
}

func proveSelectedPorts(ctx context.Context, o Options, system []string) (selectedPortEvidence, error) {
	return proveSelectedPortsWith(ctx, o, system, selectedPortDeps{sandboxprobe.DiscoverAndProveControl, offMachineWitness, portClientReady})
}
func proveSelectedPortsWith(ctx context.Context, o Options, system []string, deps selectedPortDeps) (evidence selectedPortEvidence, resultErr error) {
	var chosen sandboxprobe.ControlTarget
	defer func() {
		if ctx.Err() != nil {
			resultErr = &ProofError{Code: CapabilityProbeTimeout, Step: proofStepSelectedPortNetwork}
		}
		var failure *ProofError
		if errors.As(resultErr, &failure) {
			failure.controlAddr, failure.controlSource = chosen.Addr, chosen.Source
		}
	}()
	fail := func(reason string) (selectedPortEvidence, error) {
		return evidence, &ProofError{Code: CapabilitySandboxUnavailable, Step: reason}
	}
	controlCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	control, reason := deps.control(controlCtx, o.LoopbackControl)
	chosen = control
	evidence.control = control
	cancel()
	if reason != "" {
		return fail(reason)
	}
	if ctx.Err() != nil {
		return evidence, ctx.Err()
	}
	witness, ok := deps.witness(ctx)
	if !ok {
		return fail(ProofStepOutside)
	}
	if ctx.Err() != nil {
		return evidence, ctx.Err()
	}
	if err := deps.ready(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return evidence, &ProofError{Code: CapabilityProbeTimeout, Step: proofStepSelectedPortNetwork}
		}
		return fail(proofStepPortClientUnavailable)
	}
	if ctx.Err() != nil {
		return evidence, ctx.Err()
	}
	root, err := os.MkdirTemp("", "agent-harness-ports-")
	if err != nil {
		return fail(ProofStepFixture)
	}
	defer os.RemoveAll(root)
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fail(ProofStepFixture)
	}
	for _, dir := range []string{"work", "home", "tmp"} {
		if os.Mkdir(filepath.Join(root, dir), 0700) != nil {
			return fail(ProofStepFixture)
		}
	}
	l := workbenchLayout{Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), System: system, Read: o.Read, Write: o.Write, Loopback: true}
	a, err := freeLoopbackPort()
	if err != nil {
		return fail(ProofStepFixture)
	}
	listener, wg, reached, err := countingListener("127.0.0.1:0")
	if err != nil {
		return fail(ProofStepFixture)
	}
	defer func() { listener.Close(); wg.Wait() }()
	b := listener.Addr().(*net.TCPAddr).Port
	if a == b || a == 8340 || b == 8340 {
		return fail(ProofStepFixture)
	}
	ipv6, wg6, reached6, err := countingListener(net.JoinHostPort("::1", strconv.Itoa(b)))
	if err != nil {
		return fail(proofStepIPv6ControlUnavailable)
	}
	defer func() { ipv6.Close(); wg6.Wait() }()
	l.LoopbackPorts = []int{a, b}
	c, err := unselectedPort(l.LoopbackPorts)
	if err != nil {
		return fail(ProofStepFixture)
	}
	deniedClose, deniedReached, err := excludedListeners(c)
	if err != nil {
		return fail(ProofStepFixture)
	}
	defer deniedClose()
	forms := []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "localhost"}
	var attempts []portAttempt
	for i, addr := range forms {
		attempts = append(attempts, portAttempt{"positive-bind", addr, a, fmt.Sprintf("bind-%d", i)}, portAttempt{"positive-reach", addr, b, fmt.Sprintf("reach-%d", i)})
	}
	denied, err := portDenials([]int{c, 8340}, a, witness, control.Addr)
	if err != nil {
		return fail(ProofStepFixture)
	}
	attempts = append(attempts, denied...)
	var interfaceReach []portAttempt
	filtered := attempts[:0]
	for _, test := range attempts {
		if test.Kind == "reach" && strings.Contains(test.Label, "interface-") {
			interfaceReach = append(interfaceReach, test)
		} else {
			filtered = append(filtered, test)
		}
	}
	attempts = filtered
	stageErr := runPortStage(ctx, l, attempts, a)
	addresses, err := interfaceAddresses()
	if err != nil {
		return fail(ProofStepFixture)
	}
	interfaces := interfaceAttemptsAtPort(addresses, true, true, a)
	output, interfaceErr := runWorkbenchProbe(ctx, l, interfaceCanary("", interfaces)+"echo canary-ran\n", false, workbenchInboundProbe{})
	evidence.observations, _ = judgeInterfaceAttempts(output, interfaces, interfaceAllLocal)
	_, judgment := judgeInterfaceAttempts(output, interfaces, interfaceLoopbackOnly)
	var interfaceFailure *ProofError
	if errors.As(judgment, &interfaceFailure) && interfaceFailure.Code == CapabilitySandboxNotEnforced {
		interfaceFailure.Step = proofStepSelectedPortNetwork
		return evidence, interfaceFailure
	}
	if stageErr == nil {
		if interfaceErr != nil {
			return fail(proofStepSelectedPortNetwork)
		}
		if judgment != nil {
			return evidence, judgment
		}
	}

	if deniedReached() {
		return evidence, &ProofError{Code: CapabilitySandboxNotEnforced, Step: proofStepSelectedPortNetwork}
	}
	if stageErr != nil {
		return evidence, stageErr
	}
	if !reached() || !reached6() {
		return fail(proofStepSelectedPortNetwork)
	}
	// The inside bind/receipt server has settled. Now host listeners on that
	// port positively witness forbidden wildcard/interface reach without
	// interfering with the selected-port bind controls.
	interfaceClose, interfaceReached, err := excludedListeners(a)
	if err != nil {
		return fail(ProofStepFixture)
	}
	stageErr = runPortStage(ctx, l, interfaceReach, 0)
	interfaceClose()
	if interfaceReached() {
		return evidence, &ProofError{Code: CapabilitySandboxNotEnforced, Step: proofStepSelectedPortNetwork}
	}
	if stageErr != nil {
		return evidence, stageErr
	}
	// Actual network policy, with the same readable PATH/exec selectors and
	// disposable scratch. No workspace or durable command state is created.
	l.Work = o.WorkDir
	l.LoopbackPorts = o.LoopbackPorts
	c, err = unselectedPort(o.LoopbackPorts)
	if err != nil {
		return fail(ProofStepFixture)
	}
	actualClose, actualReached, err := excludedListeners(c)
	if err != nil {
		return fail(ProofStepFixture)
	}
	defer actualClose()
	excluded := []int{c}
	if !slices.Contains(o.LoopbackPorts, 8340) {
		excluded = append(excluded, 8340)
	}
	attempts, err = portDenials(excluded, 0, witness, control.Addr)
	if err != nil {
		return fail(ProofStepFixture)
	}
	free := 0
	for _, port := range o.LoopbackPorts {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			ln.Close()
			free = port
			break
		}
	}
	if free != 0 {
		attempts = append(attempts, portAttempt{"positive-bind", "127.0.0.1", free, "actual-bind"})
	}
	stageErr = runPortStage(ctx, l, attempts, free)
	if actualReached() {
		return evidence, &ProofError{Code: CapabilitySandboxNotEnforced, Step: proofStepSelectedPortNetwork}
	}
	if stageErr != nil {
		return evidence, stageErr
	}
	if free != 0 {
		// Prove reach under the actual list after the disposable inside server
		// has settled. Never connect to a service occupying a selected port.
		ln, workers, received, err := countingListener(net.JoinHostPort("127.0.0.1", strconv.Itoa(free)))
		if err != nil {
			return fail(proofStepSelectedPortNetwork)
		}
		attempt := []portAttempt{{"positive-reach", "127.0.0.1", free, "actual-reach"}}
		err = runPortStage(ctx, l, attempt, 0)
		ln.Close()
		workers.Wait()
		if err != nil {
			return evidence, err
		}
		if !received() {
			return fail(proofStepSelectedPortNetwork)
		}
	}
	return evidence, nil
}

func checkPortClient(ctx context.Context, tools, python func(context.Context) error) error {
	if err := tools(ctx); err != nil {
		return err
	}
	return python(ctx)
}

func portClientReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return checkPortClient(ctx, func(ctx context.Context) error { _, err := workbenchSystem(ctx); return err }, func(ctx context.Context) error {
		cmd, child, err := process.Command(ctx, "/usr/bin/python3", "-c", "import socket,errno,json,signal; print('port-client-ready')")
		if err != nil {
			return err
		}
		defer child.Close()
		out := &workbenchOutput{limit: 1024}
		cmd.Stdout, cmd.Stderr = out, out
		cmd.WaitDelay = time.Second
		if err := child.Run(); err != nil {
			return err
		}
		if strings.TrimSpace(out.text()) != "port-client-ready" {
			return &ProofError{Code: CapabilitySandboxUnavailable, Step: proofStepPortClientUnavailable}
		}
		return nil
	})
}

const inboundPortClient = `import socket, sys, signal
def deadline(signum, frame): raise TimeoutError()
signal.signal(signal.SIGALRM, deadline)
s = c = None
phase = 'socket'
signal.setitimer(signal.ITIMER_REAL, 3)
try:
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.settimeout(0.8)
    phase = 'bind'; s.bind(('127.0.0.1', int(sys.argv[1])))
    phase = 'listen'; s.listen(1)
    phase = 'accept'; c, _ = s.accept(); c.settimeout(0.8)
    phase = 'receipt'; data = c.makefile('rb').read(len(b'port-inbound'))
    if data != b'port-inbound': raise RuntimeError()
    phase = 'send'; c.sendall((sys.argv[2] + chr(10)).encode())
    print('receipt', flush=True)
except OSError as e:
    print('unavailable-inbound-' + phase + '-errno-' + str(e.errno), flush=True); sys.exit(1)
except Exception:
    print('unavailable-inbound-' + phase, flush=True); sys.exit(1)
finally:
    signal.setitimer(signal.ITIMER_REAL, 0)
    if c is not None: c.close()
    if s is not None: s.close()
`

// Independent IPv4/IPv6 listeners make an admitted excluded-port connect an
// observed escape rather than an ambiguous ECONNREFUSED. Wildcard host binds
// also cover available interface addresses when this helper is used there.
func excludedListeners(port int) (func(), func() bool, error) {
	v4, w4, r4, err := sandboxprobe.CountingListenerNetwork("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return nil, nil, err
	}
	v6, w6, r6, err := sandboxprobe.CountingListenerNetwork("tcp6", net.JoinHostPort("::", strconv.Itoa(port)))
	if err != nil {
		v4.Close()
		w4.Wait()
		return nil, nil, err
	}
	close := func() { v4.Close(); v6.Close(); w4.Wait(); w6.Wait() }
	return close, func() bool { return r4() || r6() }, nil
}

func unselectedPort(ports []int) (int, error) {
	for i := 0; i < 32; i++ {
		port, err := freeLoopbackPort()
		if err != nil {
			return 0, err
		}
		if port != 8340 && !slices.Contains(ports, port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no disposable unselected port")
}

func portDenials(excluded []int, selected int, witness, control string) ([]portAttempt, error) {
	var tests []portAttempt
	for _, p := range excluded {
		for i, addr := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "localhost"} {
			for _, kind := range []string{"bind", "udp-bind", "reach", "udp"} {
				tests = append(tests, portAttempt{kind, addr, p, fmt.Sprintf("%s-%d-%d", kind, p, i)})
			}
		}
	}
	if selected != 0 {
		addresses, err := interfaceAddresses()
		if err != nil {
			return nil, err
		}
		for i, attempt := range interfaceAttemptsAtPort(addresses, true, true, selected) {
			host, _, _ := strings.Cut(attempt.Address, "%")
			// Wildcard destinations are redirected to loopback by the kernel.
			// Keep their bind attempts in the shared interface stage, but do
			// not mistake a permitted selected-port loopback connect for escape.
			if attempt.Operation != "bind" || net.ParseIP(host).IsUnspecified() {
				continue
			}
			tests = append(tests, portAttempt{"reach", attempt.Address, selected, fmt.Sprintf("reach-interface-%d", i)})
		}
	}
	tests = append(tests, portAttempt{"reach", witness, 443, "tcp443"}, portAttempt{"reach", control, 53, "tcp53"}, portAttempt{"udp", control, 53, "udp53"})
	return tests, nil
}

func runPortStage(ctx context.Context, l workbenchLayout, attempts []portAttempt, bind int) error {
	script := portScript(attempts)
	script += "echo canary-ran\n"
	output, err := runWorkbenchProbe(ctx, l, script, false, workbenchInboundProbe{})
	// An observed escape wins over incomplete output or a later launch error.
	judgment := judgePortCanary(output, attempts, false)
	var failure *ProofError
	if errors.As(judgment, &failure) && failure.Code == CapabilitySandboxNotEnforced {
		return judgment
	}
	if ctx.Err() != nil {
		return &ProofError{Code: CapabilityProbeTimeout, Step: proofStepSelectedPortNetwork}
	}
	if err != nil {
		return &ProofError{Code: CapabilitySandboxUnavailable, Step: proofStepSelectedPortNetwork}
	}
	if judgment != nil {
		return judgment
	}
	inbound := workbenchInboundProbe{}
	if bind != 0 {
		inbound = workbenchInboundProbe{Port: bind, Nonce: process.NewToken(), Message: "port-inbound"}
		script = "/usr/bin/python3 -c " + workbenchShellQuote(inboundPortClient) + " " + strconv.Itoa(bind) + " " + workbenchShellQuote(inbound.Nonce) + "\necho ports-ran\necho canary-ran\n"
		output, err = runWorkbenchProbe(ctx, l, script, false, inbound)
		if ctx.Err() != nil {
			return &ProofError{Code: CapabilityProbeTimeout, Step: proofStepSelectedPortNetwork}
		}
		if err != nil {
			return &ProofError{Code: CapabilitySandboxUnavailable, Step: proofStepSelectedPortNetwork}
		}
		if err = judgePortCanary(output, nil, true); err != nil {
			return err
		}
	}
	return nil
}
