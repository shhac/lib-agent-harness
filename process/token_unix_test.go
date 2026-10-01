//go:build darwin || linux

package process

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestSharedSessionTokenSweep(t *testing.T) {
	token := NewToken()
	since := time.Now()
	var pids []int
	var handles []*Process
	for range 3 {
		p, pid := escapee(t, context.Background(), "exit 0")
		marker := token
		if len(handles) == 2 {
			marker = NewToken()
		}
		p.cmd.Env = TokenEnvironment(p.cmd.Env, marker)
		if err := p.Run(); err != nil {
			t.Fatal(err)
		}
		handles = append(handles, p)
		pids = append(pids, pid())
		t.Cleanup(p.Close)
	}
	if err := SweepToken(token, since); err != nil {
		t.Fatal(err)
	}
	waitGone(t, pids[0])
	waitGone(t, pids[1])
	if !alive(pids[2]) {
		t.Fatal("different session swept")
	}
}

func TestSweepTokenRefusesUnavailableInspection(t *testing.T) {
	for _, scan := range []func(time.Time) []candidate{
		func(time.Time) []candidate { return nil },
		func(time.Time) []candidate { return []candidate{{pid: os.Getpid()}} },
	} {
		if err := sweepToken(NewToken(), time.Now(), scan); err == nil {
			t.Fatal("unavailable identity inspection accepted as absence")
		}
	}
	calls := 0
	if err := sweepToken(NewToken(), time.Now(), func(time.Time) []candidate {
		calls++
		if calls == 1 {
			return []candidate{{pid: os.Getpid(), identity: "self"}}
		}
		return nil
	}); err == nil {
		t.Fatal("inspection lost during sweep accepted as absence")
	}
}

func TestSweepSparesReusedPID(t *testing.T) {
	for _, current := range []string{"", "reused", "original"} {
		called := false
		signalOwned(123, "original", func(int) string { return current }, func(int) { called = true })
		if called != (current == "original") {
			t.Fatalf("identity %q signalled: %t", current, called)
		}
	}
}
