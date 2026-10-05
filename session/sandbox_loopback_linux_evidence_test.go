package session

import (
	"context"
	"fmt"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

func TestClaudeLinuxScopeCandidate(t *testing.T) {
	for _, tc := range []struct {
		output  string
		reached bool
		scope   string
		code    string
	}{
		{"bound\ncanary-ran\n", false, "per-command", ""},
		{"loopback\nbound\ncanary-ran\n", true, "host-shared", ""},
		{"loopback\nbound\ncanary-ran\n", false, "", CapabilitySandboxUnavailable},
		{"bound\ncanary-ran\n", true, "", CapabilitySandboxUnavailable},
		{"canary-ran\n", false, "", CapabilitySandboxUnavailable},
		{"outside\ngarbage", false, "", CapabilitySandboxNotEnforced},
	} {
		scope, err := judgeClaudeLinuxScope(context.Background(), tc.output, tc.reached)
		if scope != tc.scope {
			t.Fatalf("scope %q want %q", scope, tc.scope)
		}
		if tc.code == "" {
			if err != nil {
				t.Fatal(err)
			}
		} else if e, ok := err.(*CapabilityError); !ok || e.Code != tc.code {
			t.Fatalf("error %v want %s", err, tc.code)
		}
		if tc.output == "canary-ran\n" && err.(*CapabilityError).Reason != claudeLinuxInCommandLoopbackReason {
			t.Fatal("missing named in-command refusal")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct{ output, code string }{{"bound\ncanary-ran\n", CapabilityProbeTimeout}, {"outside\n", CapabilitySandboxNotEnforced}} {
		_, err := judgeClaudeLinuxScope(ctx, tc.output, false)
		if e, ok := err.(*CapabilityError); !ok || e.Code != tc.code {
			t.Fatal(err)
		}
	}
}

func TestClaudeLinuxInterfaceCandidate(t *testing.T) {
	a := sandboxprobe.InterfaceAttempts([]string{"192.0.2.1"}, true, true)
	var b strings.Builder
	for i, attempt := range a {
		errno := 0
		if i < 2 {
			errno = 99
		}
		if attempt.Operation == "udp" {
			errno = 101
		}
		fmt.Fprintf(&b, "interface-result:%d:%d\n", i, errno)
	}
	b.WriteString("interface-canary-ran\nnamespace-interfaces:[\"lo\"]\n")
	good := b.String()
	rows, summary, err := judgeClaudeLinuxInterfaces(good, a, false, "per-command")
	if err != nil || summary != "private-namespace" || rows[2].Errno != 101 {
		t.Fatalf("%s %v", summary, err)
	}
	for _, output := range []string{strings.ReplaceAll(good, "interface-result:0:99\n", ""), good + "interface-result:0:99\n", strings.ReplaceAll(good, "0:99", "0:bad"), strings.ReplaceAll(good, "interface-canary-ran\n", "")} {
		if rows, _, err := judgeClaudeLinuxInterfaces(output, a, false, "per-command"); err == nil || rows != nil {
			t.Fatal("accepted incomplete measurement")
		}
	}
	for _, output := range []string{strings.ReplaceAll(good, "namespace-interfaces:[\"lo\"]\n", ""), good + "namespace-interfaces:bad\n"} {
		if rows, _, err := judgeClaudeLinuxInterfaces(output, a, false, "per-command"); err == nil || rows != nil {
			t.Fatal("incomplete namespace report accepted")
		}
	}
	if rows, _, err := judgeClaudeLinuxInterfaces(good+"outside\n", a, false, "per-command"); err.(*CapabilityError).Code != CapabilitySandboxNotEnforced || len(rows) != len(a) {
		t.Fatal("escape discarded diagnostic table")
	}
	if _, _, err := judgeClaudeLinuxInterfaces("garbage", a, true, "per-command"); err.(*CapabilityError).Code != CapabilitySandboxNotEnforced {
		t.Fatal(err)
	}
	partialExposure := strings.Replace(good, "interface-result:0:99", "interface-result:0:0", 1)
	if _, summary, err := judgeClaudeLinuxInterfaces(partialExposure, a, false, "per-command"); err != nil || summary != "all-interfaces" {
		t.Fatal("one successful host bind understated")
	}
	if _, summary, err := judgeClaudeLinuxInterfaces(good, a, true, "host-shared"); err != nil || summary != "all-interfaces" {
		t.Fatal("host-shared delivery treated as escape")
	}
	rows, _, err = judgeClaudeLinuxInterfaces(good, a, true, "per-command")
	if err == nil || len(rows) != len(a) {
		t.Fatal("escape discarded complete rows")
	}
	for _, errno := range []string{"0", "1", "13"} {
		output := strings.ReplaceAll(good, ":99\n", ":"+errno+"\n")
		_, summary, err := judgeClaudeLinuxInterfaces(output, a, false, "per-command")
		want := "mixed/unavailable"
		if errno == "0" {
			want = "all-interfaces"
		}
		if err != nil || summary != want {
			t.Fatalf("errno=%s summary=%s error=%v", errno, summary, err)
		}
	}
	if _, summary, err := judgeClaudeLinuxInterfaces("interface-canary-ran\nnamespace-interfaces:[\"lo\"]\n", nil, false, "per-command"); err != nil || summary != "mixed/unavailable" {
		t.Fatal("offline host claimed confinement")
	}
}

func TestClaudeLinuxScopeClaimAndCache(t *testing.T) {
	for _, tc := range []struct {
		output       string
		reached      bool
		claim, scope string
		code         string
	}{
		{"bound\ncanary-ran\n", false, "host-shared", "per-command", ""},
		{"loopback\nbound\ncanary-ran\n", true, "per-command", "", CapabilitySandboxNotEnforced},
		{"loopback\nbound\ncanary-ran\n", true, "host-shared", "host-shared", ""},
	} {
		evidence, err := judgeClaudeLinuxClaim(context.Background(), tc.output, tc.reached, tc.claim)
		if evidence.scope != tc.scope {
			t.Fatal(evidence)
		}
		cache := &verificationCache{seen: map[string]bool{}}
		settled := cache.settleSandbox(context.Background(), "claude", "scope", evidence, err)
		if tc.code != "" {
			if e, ok := settled.(*CapabilityError); !ok || e.Code != tc.code || cache.holds("scope") {
				t.Fatal(settled)
			}
			continue
		}
		if settled != nil || cache.loopback["scope"].scope != tc.scope {
			t.Fatal("scope not copied")
		}
	}
}

func TestClaudeLoopbackPlatformCandidate(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		good := "loopback\nbound\ncanary-ran\n"
		evidence, err := judgeClaudeLoopbackForPlatform(context.Background(), goos, "host-shared", good, true, nil)
		if err != nil || (goos == "linux" && evidence.scope != "host-shared") {
			t.Fatal(goos, err)
		}
		_, err = judgeClaudeLoopbackForPlatform(context.Background(), goos, "per-command", "bound\ncanary-ran\n", false, nil)
		if goos == "linux" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("non-Linux gate relaxed")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for _, tc := range []struct{ output, code string }{{good, CapabilityProbeTimeout}, {"outside\nmalformed", CapabilitySandboxNotEnforced}} {
			_, err := judgeClaudeLoopbackForPlatform(ctx, goos, "host-shared", tc.output, true, nil)
			if e, ok := err.(*CapabilityError); !ok || e.Code != tc.code {
				t.Fatal(goos, err)
			}
		}
	}
}

func TestClaudeLinuxNamespaceNamesRejectMalformedEvidence(t *testing.T) {
	for _, output := range []string{"namespace-interfaces:garbage\n", "namespace-interfaces:[\"lo\"]\nnamespace-interfaces:[\"lo\"]\n", "namespace-interfaces:[\"lo\",\"lo\"]\n", "namespace-interfaces:[\"secret\\ntext\"]\n", "namespace-interfaces:[]\n"} {
		if _, ok := claudeLinuxNamespaceNames(output); ok {
			t.Fatal("malformed namespace report accepted")
		}
	}
}

func TestClaudeLinuxNamedFailureAndWiderScopeFacts(t *testing.T) {
	if ClaudeLinuxInCommandLoopbackFailed != "claude_linux_in_command_loopback_failed" || ClaudeLinuxLoopbackScopeWiderThanClaimed != "claude_linux_loopback_scope_wider_than_claimed" {
		t.Fatal("stable exported refusal changed")
	}
	_, err := judgeClaudeLinuxClaim(context.Background(), "canary-ran\n", false, "per-command")
	if e, ok := err.(*CapabilityError); !ok || e.Reason != claudeLinuxInCommandLoopbackReason || !strings.Contains(e.Error(), claudeLinuxInCommandLoopbackReason) {
		t.Fatal(err)
	}
	_, err = judgeClaudeLinuxClaim(context.Background(), "loopback\nbound\ncanary-ran\n", true, "per-command")
	if e, ok := err.(*CapabilityError); !ok || e.Code != CapabilitySandboxNotEnforced || e.Reason != ClaudeLinuxLoopbackScopeWiderThanClaimed || !strings.Contains(e.Error(), ClaudeLinuxLoopbackScopeWiderThanClaimed) {
		t.Fatal(err)
	}
}

func TestClaudeLinuxOwnerProofOptionalMeasurement(t *testing.T) {
	attempts := sandboxprobe.InterfaceAttempts([]string{"192.0.2.1"}, false, true)
	for _, output := range []string{"", "garbled", "interface-result:0:99\n"} {
		_, summary, err := judgeClaudeLinuxInterfaces(output, attempts, false, "per-command")
		if err == nil || !strings.Contains(summary, "measurement unavailable:") {
			t.Fatalf("missing measurement diagnostic: %s %v", summary, err)
		}
		if err := judgeClaudeLinuxOwnerProof(nil, nil, "per-command", "per-command", ""); err != nil {
			t.Fatalf("optional unavailable measurement gated production: %v", err)
		}
	}
	for _, tc := range []struct{ scope, recorded, nc string }{
		{"host-shared", "per-command", ""}, {"per-command", "", ""}, {"per-command", "per-command", "outside\n"},
	} {
		if judgeClaudeLinuxOwnerProof(nil, nil, tc.scope, tc.recorded, tc.nc) == nil {
			t.Fatal("scope failure or off-machine escape accepted")
		}
	}
	for _, code := range []string{CapabilitySandboxUnavailable, CapabilitySandboxNotEnforced} {
		err := &CapabilityError{Engine: harness.Claude, Code: code}
		if judgeClaudeLinuxOwnerProof(err, nil, "per-command", "per-command", "") == nil || judgeClaudeLinuxOwnerProof(nil, err, "per-command", "per-command", "") == nil {
			t.Fatal("production/base failure accepted")
		}
	}
}
