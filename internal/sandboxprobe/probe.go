package sandboxprobe

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

const CanaryNoClient = "no-network-client"
const CanaryRan = "canary-ran"

// Canary outcomes for the loopback proof, one per line.
const (
	LoopbackReached = "loopback"
	LoopbackBound   = "bound"
	LoopbackOutside = "outside"
)

// loopbackWitnessHost is where the proof's off-machine address comes from: the
// Claude API, which the session must reach anyway, so the proof sends nothing
// to a third party.
const loopbackWitnessHost = "api.anthropic.com"

// LoopbackCanary connects to the probe's loopback listener, binds and reaches
// its own loopback listener, and tries an off-machine address the probe has
// just reached itself, which must be refused. It is a flat list of plain
// commands: in dontAsk mode Claude Code 2.1.283 auto-allows a sandboxed
// command only in that shape, and refuses one that defines a function or
// opens a subshell.
func LoopbackCanary(loopPort int, witness string, witnessPort, bindPort int) string {
	return fmt.Sprintf(`command -v nc >/dev/null 2>&1 || echo %s
nc -z -w 3 127.0.0.1 %d >/dev/null 2>&1 && echo %s
nc -z -w 3 %s %d >/dev/null 2>&1 && echo %s
nc -l 127.0.0.1 %d >/dev/null 2>&1 &
sleep 1
nc -z -w 3 127.0.0.1 %d >/dev/null 2>&1 && echo %s
echo %s`, CanaryNoClient, loopPort, LoopbackReached, witness, witnessPort, LoopbackOutside, bindPort, bindPort, LoopbackBound, CanaryRan)
}

// OffMachineWitness is an IPv4 address off this machine that the probe itself
// reaches on port 443. A refusal inside the sandbox only means something when
// the same connection succeeds outside it; without such an address nothing is
// proved, and the session is refused.
func OffMachineWitness(ctx context.Context) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, loopbackWitnessHost)
	if err != nil {
		return "", false
	}
	var dialer net.Dialer
	for _, addr := range addrs {
		if addr.IP.To4() == nil || addr.IP.IsLoopback() || addr.IP.IsPrivate() {
			continue
		}
		conn, dialErr := dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr.IP.String(), "443"))
		if dialErr != nil {
			continue
		}
		conn.Close()
		return addr.IP.String(), true
	}
	return "", false
}

// CountingListener accepts and closes connections, recording that one came.
func CountingListener(address string) (net.Listener, *sync.WaitGroup, func() bool, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, nil, err
	}
	var mu sync.Mutex
	reached := false
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			conn.Close()
			mu.Lock()
			reached = true
			mu.Unlock()
		}
	}()
	return listener, &wg, func() bool { mu.Lock(); defer mu.Unlock(); return reached }, nil
}

func FreeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
