package sandbox

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

const sandboxProbeTimeout = 60 * time.Second
const loopbackReached = sandboxprobe.LoopbackReached
const loopbackBound = sandboxprobe.LoopbackBound
const loopbackOutside = sandboxprobe.LoopbackOutside

func loopbackCanary(a int, b string, c, d int) string { return sandboxprobe.LoopbackCanary(a, b, c, d) }
func offMachineWitness(ctx context.Context) (string, bool) {
	return sandboxprobe.OffMachineWitness(ctx)
}
func countingListener(a string) (net.Listener, *sync.WaitGroup, func() bool, error) {
	return sandboxprobe.CountingListener(a)
}
func freeLoopbackPort() (int, error) { return sandboxprobe.FreeLoopbackPort() }

const canaryRan = sandboxprobe.CanaryRan
const canaryNoClient = sandboxprobe.CanaryNoClient
