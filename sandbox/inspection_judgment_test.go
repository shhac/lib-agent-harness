package sandbox

import (
	"context"
	"errors"
	harness "github.com/shhac/lib-agent-harness"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProcessInspectionJudgment(t *testing.T) {
	good := "expected-nice 5\nexpected-group 2\npositive 3 2 5 S\nstat 3 2 5 S 123\ncommand Mon Oct 5 12:00:00 2026 /bin/sleep 30\nprivate-namespace\nnegative-invisible\ninspection-ran\n"
	for _, row := range []struct{ name, output, code string }{
		{"good", good, ""}, {"collision", strings.ReplaceAll(good, "negative-invisible", "negative-collision"), ""},
		{"argv-source", good + "echo foreign-process\necho shared-namespace\necho negative-visible-refused\n", ""},
		{"membership-unavailable", good + "membership-unavailable\n", CapabilityProcessInspectionUnavailable},
		{"foreign", good + "foreign-process\n", CapabilitySandboxNotEnforced},
		{"visible", strings.ReplaceAll(good, "negative-invisible", "negative-visible-refused"), CapabilitySandboxNotEnforced},
		{"empty", "", CapabilityProcessInspectionUnavailable},
		{"shared", good + "shared-namespace\n", CapabilitySandboxNotEnforced},
		{"missing-negative", strings.ReplaceAll(good, "negative-invisible", ""), CapabilityProcessInspectionUnavailable},
		{"missing-positive", strings.ReplaceAll(good, "positive 3 2 5 S\n", ""), CapabilityProcessInspectionUnavailable},
		{"wrong-nice", strings.ReplaceAll(good, " 5 ", " 6 "), CapabilityProcessInspectionUnavailable},
		{"wrong-group", strings.ReplaceAll(good, "positive 3 2", "positive 3 9"), CapabilityProcessInspectionUnavailable},
		{"missing-birth", strings.ReplaceAll(good, "123", "0"), CapabilityProcessInspectionUnavailable},
	} {
		t.Run(row.name, func(t *testing.T) {
			err := judgeProcessInspection(row.output, false)
			if row.code == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var p *ProofError
			if !errors.As(err, &p) || p.Code != row.code {
				t.Fatalf("%v", err)
			}
			facts, _ := harness.ErrorFacts(err)
			if facts.ProofStep != ProofStepProcessInspection {
				t.Fatalf("%+v", facts)
			}
		})
	}
	if err := judgeProcessInspection(strings.ReplaceAll(strings.ReplaceAll(good, " 5 ", " 15 "), "expected-nice 5", "expected-nice 15"), true); err != nil {
		t.Fatal(err)
	}
	for _, nice := range []string{"-5", "19"} {
		inherited := strings.ReplaceAll(good, "positive 3 2 5", "positive 3 2 "+nice)
		inherited = strings.ReplaceAll(inherited, "stat 3 2 5", "stat 3 2 "+nice)
		inherited = strings.ReplaceAll(inherited, "expected-nice 5", "expected-nice "+nice)
		if err := judgeProcessInspection(inherited, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProcessInspectionHostJudgment(t *testing.T) {
	good := "host 3 2 5 123\nstat 3 2 5 S 123\n"
	for _, row := range []struct {
		output string
		ok     bool
	}{
		{good, true}, {"", false}, {strings.Replace(good, "host 3 2", "host 4 2", 1), false},
		{strings.Replace(good, "host 3 2", "host 3 9", 1), false}, {strings.Replace(good, "5 123", "7 123", 1), false},
		{strings.Replace(good, "5 123", "5 124", 1), false},
	} {
		if err := judgeInspectionHost(row.output); (err == nil) != row.ok {
			t.Fatalf("%q: %v", row.output, err)
		}

	}
}

func TestProcessInspectionMarkerLeaks(t *testing.T) {
	for _, interfaceName := range []string{"cmdline", "environ", "fd", "ps"} {
		err := judgeInspectionMarker(interfaceName+" fixture-marker", "fixture-marker")
		var p *ProofError
		if !errors.As(err, &p) || p.Code != CapabilitySandboxNotEnforced || p.Step != ProofStepProcessInspection {
			t.Fatalf("%s: %v", interfaceName, err)
		}
	}
	if err := judgeInspectionMarker("only own metadata", "fixture-marker"); err != nil {
		t.Fatal(err)
	}
}

func TestProcessInspectionDiagnostics(t *testing.T) {
	got := inspectionDiagnostic("secret-marker foreign-process\n", "secret-marker")
	if strings.Contains(got, "secret-marker") || !strings.Contains(got, "[fixture-marker]") {
		t.Fatal(got)
	}
	if inspectionControl("echo foreign-process\nforeign-process\n") != "foreign-process" {
		t.Fatal("missing control")
	}
	if len(inspectionDiagnostic(strings.Repeat("x", 100000), "marker")) > 66000 {
		t.Fatal("unbounded output")
	}
	e := &ProofError{Code: CapabilitySandboxNotEnforced, Step: ProofStepProcessInspection}
	if !strings.Contains(e.Error(), "process inspection could see processes outside") {
		t.Fatal(e)
	}
}

func TestProcessInspectionProcMembership(t *testing.T) {
	if _, err := os.Stat("/usr/bin/awk"); err != nil {
		t.Skip("awk unavailable on this platform")
	}
	// No ns symlink exists: this models PID 1 with a ptrace-refused namespace
	// link, without weakening readable status membership or the proc anchor.
	for _, row := range []struct {
		name, status string
		rooted, ok   bool
	}{
		{"init", "Name:\tbwrap\nNSpid:\t1\n", false, true},
		{"root", "NSpid:\t1\n", true, true},
		{"ancestor-proc", "NSpid:\t1\t9\n", true, false},
		{"descendant", "NSpid:\t1\t9\n", false, true},
		// Corrupt synthetic status only; real procfs cannot disagree with directory PID.
		{"malformed-status", "NSpid:\t2\n", false, false},
		{"missing", "Name:\tbwrap\n", false, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "status")
			if err := os.WriteFile(path, []byte(row.status), 0600); err != nil {
				t.Fatal(err)
			}
			err := exec.Command("/bin/sh", "-c", inspectionStatusCheck(strconv.Quote(path), "1", row.rooted)).Run()
			if (err == nil) != row.ok {
				t.Fatalf("membership: %v", err)
			}
		})
	}
}

func TestProcessInspectionCancelledTranscript(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, row := range []struct{ output, code string }{{"fixture-marker", CapabilitySandboxNotEnforced}, {"only own metadata", CapabilityProbeTimeout}} {
		err := judgeInspectionTranscript(ctx, row.output, "fixture-marker")
		var proof *ProofError
		if !errors.As(err, &proof) || proof.Code != row.code || proof.Step != ProofStepProcessInspection {
			t.Fatalf("%q: %v", row.output, err)
		}

	}
}
