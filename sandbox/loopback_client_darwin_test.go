package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// Validate socket mechanics without Seatbelt, using the test interpreter and
// only local controls. The real proof tests separately require Apple's shim
// inside the command profile; this test cannot establish that enforcement.
func TestSelectedPortSocketClientControls(t *testing.T) {
	testenv.RequireLoopback(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, wg, reached, err := countingListener("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { listener.Close(); wg.Wait() }()
	port := listener.Addr().(*net.TCPAddr).Port
	free, err := freeLoopbackPort()
	if err != nil {
		t.Fatal(err)
	}
	testenv.RequireExplicitLoopback(t, free)
	if _, err := workbenchSystem(ctx); err != nil {
		t.Fatalf("socket helper developer-tools prerequisite: %v", err)
	}
	interpreter, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("socket helper test interpreter", err)
	}
	attempts := []portAttempt{{"positive-bind", "127.0.0.1", free, "bind"}, {"positive-bind", "localhost", free, "hostname"}, {"positive-reach", "127.0.0.1", port, "reach"}, {"reach", "127.0.0.1", port, "outside-reach"}, {"bind", "127.0.0.1", free, "outside-bind"}, {"udp-bind", "127.0.0.1", free, "outside-udp-bind"}, {"udp", "127.0.0.1", free, "outside-udp"}}
	data, err := json.Marshal(attempts)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(ctx, interpreter, "-c", portClient, string(data)).CombinedOutput()
	if err != nil {
		t.Fatalf("socket helper failed: %v; status: %s", err, output)
	}
	for _, marker := range []string{"positive-bind", "positive-hostname", "positive-reach", "escape-outside-reach", "escape-outside-bind", "escape-outside-udp-bind", "escape-outside-udp", "ports-ran"} {
		if !strings.Contains(string(output), marker+"\n") {
			t.Fatalf("missing %s on local control port %s", marker, strconv.Itoa(free))
		}
	}
	if !reached() {
		t.Fatal("helper reach not observed outside")
	}
	var escaped *ProofError
	if err := judgePortCanary(string(output)+"canary-ran\n", attempts, false); !errors.As(err, &escaped) || escaped.Code != CapabilitySandboxNotEnforced {
		t.Fatal("outside controls did not report an escape", err)
	}
}
