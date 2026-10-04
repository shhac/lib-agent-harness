package testenv

import (
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestLinuxPrivilegeProbeSharesSettledVerdict(t *testing.T) {
	verdicts := make([]error, 16)
	var wg sync.WaitGroup
	for i := range verdicts {
		wg.Go(func() { verdicts[i] = linuxPrivilegeControl.result() })
	}
	wg.Wait()
	if verdicts[0] != nil && !Refused(verdicts[0]) {
		t.Fatalf("unexpected privilege observation: %v", verdicts[0])
	}
	for _, verdict := range verdicts {
		if verdict != verdicts[0] {
			t.Fatalf("privilege verdict changed: %v / %v", verdict, verdicts[0])
		}
	}
}

func TestLinuxPrivilegeVerdict(t *testing.T) {
	status := "CapEff: 0000\nCapPrm: 0000\nCapInh: 0000\nCapAmb: 0000\nNoNewPrivs: 1\n"
	if err := linuxPrivilegeVerdict([]byte(status), nil); !Refused(err) {
		t.Fatalf("contained: %v", err)
	}
	for _, field := range []string{"CapEff", "CapPrm", "CapInh", "CapAmb", "NoNewPrivs"} {
		old, next := field+": 0000", field+": 0001"
		if field == "NoNewPrivs" {
			old, next = field+": 1", field+": 0"
		}
		if err := linuxPrivilegeVerdict([]byte(strings.ReplaceAll(status, old, next)), nil); err != nil {
			t.Fatalf("%s: %v", field, err)
		}
	}
	for _, bad := range []string{"", strings.ReplaceAll(status, "CapEff: 0000", "CapEff: invalid"), status + "CapEff: 0\n", strings.ReplaceAll(status, "NoNewPrivs: 1", "NoNewPrivs: 2")} {
		if err := linuxPrivilegeVerdict([]byte(bad), nil); err == nil || Refused(err) {
			t.Fatalf("malformed status: %v", err)
		}
	}
	if err := linuxPrivilegeVerdict(nil, syscall.EACCES); !Refused(err) {
		t.Fatal(err)
	}
	if err := linuxPrivilegeVerdict(nil, errors.New("interrupted")); err == nil || Refused(err) {
		t.Fatal(err)
	}
}

func TestLinuxPrivilegeControlRefusalModes(t *testing.T) {
	status := []byte("CapEff: 0\nCapPrm: 0\nCapInh: 0\nCapAmb: 0\nNoNewPrivs: 1\n")
	for _, strict := range []string{"", "1"} {
		t.Setenv(NoSkipVariable, strict)
		r := &recorder{}
		func() {
			defer func() {
				if v := recover(); v != nil && v != r {
					panic(v)
				}
			}()
			require(r, "an unconfined Linux privilege control", linuxPrivilegeVerdict(status, nil))
		}()
		message := r.skipped + r.failed
		if !strings.HasPrefix(message, "environment refuses an unconfined Linux privilege control:") || strings.ContainsAny(message, "\r\n") || !strings.Contains(message, "NoNewPrivs=1") {
			t.Fatalf("lost named confinement diagnostic: %q", message)
		}
		if strict == "1" {
			if r.skipped != "" || !strings.Contains(r.failed, "forbids skipping") {
				t.Fatalf("%+v", r)
			}
		} else if r.failed != "" || r.skipped == "" {
			t.Fatalf("%+v", r)
		}
	}
}
