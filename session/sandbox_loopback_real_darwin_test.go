//go:build darwin

package session

import (
	"context"
	stdflag "flag"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
)

// Selecting this test by exact name opts in, as does the environment variable
// when running the whole suite. CI needs no commercial CLI or connectivity.
// Once requested, prerequisites fail, never skip.
func TestClaudeLoopbackInterfaceRealProof(t *testing.T) {
	selected := stdflag.Lookup("test.run").Value.String()
	if selected != "TestClaudeLoopbackInterfaceRealProof" && selected != "^TestClaudeLoopbackInterfaceRealProof$" && os.Getenv("AGENT_HARNESS_TEST_CLAUDE_INTERFACE_PROOF") != "1" {
		t.Skip("owner-only installed Claude proof; select its exact name or set AGENT_HARNESS_TEST_CLAUDE_INTERFACE_PROOF=1")
	}
	if os.Getenv("AGENT_HARNESS_TEST_NO_SKIP") != "1" {
		t.Fatal("real proof requires AGENT_HARNESS_TEST_NO_SKIP=1")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("installed Claude Code CLI required")
	}
	o := sandboxOptions(t, harness.Claude, binary, true)
	o.Provider.CLI.Home = t.TempDir()
	o.Sandbox.Loopback = true
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// Keep o raw. VerifySandbox owns normalization and foreign-policy checks.
	if err := VerifySandbox(ctx, o); err != nil {
		t.Fatal(err)
	}
	if support := harness.Support(harness.Claude, harness.Session, harness.Loopback); support.Availability != harness.Unknown || !strings.Contains(support.Reason, "every local interface") {
		t.Fatalf("Loopback not offered: %+v", support)
	}
	key, err := realClaudeProofKey(o)
	if err != nil {
		t.Fatal(err)
	}
	verified.mu.Lock()
	proved := verified.seen[key]
	rows := append([]ncInterfaceObservation(nil), verified.loopback[key].interfaces...)
	verified.mu.Unlock()
	if !proved {
		t.Fatal("base Loopback proof not recorded")
	}
	t.Log("address class | operation | optional observation (no socket errno)")
	for _, row := range rows {
		t.Logf("%s | %s | %s", claudeInterfaceAddressClass(row.address), row.operation, row.outcome)
	}
	if len(rows) == 0 {
		t.Log("interface diagnostics unavailable; base Loopback proof passed")
	}
	t.Log("Loopback offered after base proof; all-interface exposure is LAH-39 platform evidence; optional measurements do not gate support")
}

func claudeInterfaceAddressClass(address string) string {
	host, _, _ := strings.Cut(address, "%")
	ip := net.ParseIP(host)
	if ip == nil {
		return "unknown"
	}
	family := "IPv6"
	if ip.To4() != nil {
		family = "IPv4"
	}
	switch {
	case ip.IsUnspecified():
		return family + " wildcard"
	case ip.IsLinkLocalUnicast():
		return family + " link-local"
	case ip.IsPrivate():
		return family + " private"
	case ip.IsLoopback():
		return family + " loopback"
	default:
		return family + " other"
	}
}
