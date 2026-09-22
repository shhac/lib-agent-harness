//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// probedOptions is a restricted configuration whose harness is a fake running
// the given scenario.
func probedOptions(t *testing.T, engine Engine, scenario string, env ...string) (Options, string) {
	t.Helper()
	binary, log := fakeHarness(t, scenario, env...)
	o := restrictedOptions(t, engine)
	o.Binary = binary
	if engine == Codex {
		putSynthetic(t, o.Home, codexCredentialFile, "synthetic-login")
	}
	return o, log
}

func probeContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The whole judgement, driven through a real harness process and a real
// loopback provider: what the harness actually sent decides, and a refusal
// names its reason.
func TestProbeJudgesWhatTheHarnessActuallySent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		engine   Engine
		scenario string
		code     string
	}{
		{"exactly the hosted tools", Claude, fakeClean, ""},
		{"a built-in kept", Claude, fakeBuiltIn, CapabilityNativeToolsPresent},
		{"only auxiliary requests", Claude, fakeNoTools, CapabilityHostedToolsMissing},
		{"no request at all", Claude, fakeSilent, CapabilityProbeNoRequest},
		{"an unreadable request", Claude, fakeGarbage, CapabilityProbeUnreadable},
		// Codex defers MCP tools and never puts them in a request, so the tool
		// channel having served them is the only positive evidence.
		{"deferred tools the channel served", Codex, fakeListed, ""},
		{"deferred tools nobody asked for", Codex, fakeUnlisted, CapabilityHostedToolsMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, log := probedOptions(t, tc.engine, tc.scenario)
			err := VerifyRestriction(probeContext(t), o)
			if invocations(t, log, "probe") != 1 {
				t.Fatalf("the harness was probed %d times", invocations(t, log, "probe"))
			}
			if tc.code == "" {
				if err != nil {
					t.Fatalf("a correctly restricted harness was refused: %v", err)
				}
				return
			}
			var failure *CapabilityError
			if !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
			if failure.Phase != BeforeLaunch {
				t.Errorf("a probe refusal reported phase %q", failure.Phase)
			}
			if tc.code == CapabilityNativeToolsPresent && (len(failure.Tools) != 1 || failure.Tools[0] != "Bash") {
				t.Errorf("refusal did not name the retained built-in: %v", failure.Tools)
			}
			if invocations(t, log, "session") != 0 {
				t.Fatal("a harness was launched for real after its probe failed")
			}
		})
	}
}

// A second check of the same binary and arguments is answered from the record
// of the first, and the record is only of success.
func TestVerifiedRestrictionIsNotProvedTwice(t *testing.T) {
	o, log := probedOptions(t, Claude, fakeClean)
	for range 2 {
		if err := VerifyRestriction(probeContext(t), o); err != nil {
			t.Fatal(err)
		}
	}
	if got := invocations(t, log, "probe"); got != 1 {
		t.Fatalf("a proved configuration was probed %d times", got)
	}
	refused, refusedLog := probedOptions(t, Claude, fakeBuiltIn)
	for range 2 {
		if err := VerifyRestriction(probeContext(t), refused); err == nil {
			t.Fatal("a refused configuration was accepted")
		}
	}
	if got := invocations(t, refusedLog, "probe"); got != 2 {
		t.Fatalf("a refusal was remembered as evidence: probed %d times", got)
	}
}

// Starting a restricted session names the running harness on disk before it is
// asked for anything, and releasing it clears that record once the harness is
// provably gone.
func TestRestrictedStartRecordsTheHarnessAndReleaseClearsIt(t *testing.T) {
	o, _ := probedOptions(t, Claude, fakeClean)
	ctx := probeContext(t)
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record, err := readLaunchRecord(o.Restriction.Tools.Dir)
	if err != nil || record == nil {
		t.Fatalf("no launch record: %v", err)
	}
	if !record.identified() || record.PID != record.Group {
		t.Fatalf("the launch record does not name a contained process: %+v", record)
	}
	if alive, aliveErr := groupAlive(record.Group); aliveErr != nil || !alive {
		t.Fatalf("the recorded group is not the running harness: %v %v", alive, aliveErr)
	}
	if got := s.Capabilities().RestrictTools.Availability; got != Native {
		t.Errorf("the startup cross-check did not record the advertised surface: %q", got)
	}
	out, err := s.Release(ctx)
	if err != nil || !out.Confirmed {
		t.Fatalf("release did not confirm the harness gone: %+v %v", out, err)
	}
	if alive, _ := groupAlive(record.Group); alive {
		t.Fatal("the harness survived its release")
	}
	if _, err = os.Stat(launchPath(o.Restriction.Tools.Dir)); !os.IsNotExist(err) {
		t.Fatal("release left the launch record behind")
	}
}

// A refresh the harness made to its runtime copy goes back to the source once
// the harness is gone, so the operator's own CLI and the next worker share it.
func TestReleaseReturnsARefreshedLoginToItsSource(t *testing.T) {
	o, _ := probedOptions(t, Codex, fakeListed, fakeRefreshEnv+"=refreshed-login")
	ctx := probeContext(t)
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := credentialText(t, o.RuntimeHome); got != "refreshed-login" {
		t.Fatalf("the fake harness did not refresh its runtime login: %q", got)
	}
	if got := credentialText(t, o.Home); got != "synthetic-login" {
		t.Fatalf("the source changed while the harness ran: %q", got)
	}
	out, err := s.Release(ctx)
	if err != nil || !out.Confirmed {
		t.Fatalf("release did not confirm the harness gone: %+v %v", out, err)
	}
	if got := credentialText(t, o.Home); got != "refreshed-login" {
		t.Fatalf("a refreshed login was not returned to its source: %q", got)
	}
}

// An operator who logs in again while a worker runs has made a newer login. The
// worker's refresh of the older one must not replace it.
func TestReleaseLeavesANewerSourceLoginAlone(t *testing.T) {
	o, _ := probedOptions(t, Codex, fakeListed, fakeRefreshEnv+"=refreshed-login")
	ctx := probeContext(t)
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	putSynthetic(t, o.Home, codexCredentialFile, "owner-new-login")
	out, err := s.Release(ctx)
	if err != nil || !out.Confirmed {
		t.Fatalf("release did not confirm the harness gone: %+v %v", out, err)
	}
	if got := credentialText(t, o.Home); got != "owner-new-login" {
		t.Fatalf("a worker's refresh replaced a newer owner login: %q", got)
	}
	if _, err = os.Stat(launchPath(o.Restriction.Tools.Dir)); !os.IsNotExist(err) {
		t.Fatal("declining a write-back left the launch record behind")
	}
}

// A restricted session's ordinary work runs through the same launch: a turn
// reaches the harness and its answer comes back.
func TestRestrictedSessionRunsATurn(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			scenario := fakeClean
			if engine == Codex {
				scenario = fakeListed
			}
			o, _ := probedOptions(t, engine, scenario)
			ctx := probeContext(t)
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _, _ = s.Release(ctx) }()
			turn, err := s.StartTurn(ctx, Input{"hello"})
			if err != nil {
				t.Fatal(err)
			}
			for range turn.Events() {
			}
			result, err := turn.Wait(ctx)
			if err != nil || result.Status != "completed" {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}
