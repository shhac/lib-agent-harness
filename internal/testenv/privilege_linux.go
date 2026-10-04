package testenv

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

var linuxPrivilegeControl = &probe{check: func() error {
	status, err := os.ReadFile("/proc/self/status")
	return linuxPrivilegeVerdict(status, err)
}}

// RequireLinuxPrivilegeControl requires an unconfined privilege escape control.
// It does not gate enforcement checks in a real sandbox.
func RequireLinuxPrivilegeControl(t testing.TB) {
	t.Helper()
	require(t, "an unconfined Linux privilege control", linuxPrivilegeControl.result())
}

func linuxPrivilegeVerdict(status []byte, err error) error {
	if err != nil {
		return fmt.Errorf("read Linux privilege status: %w", err)
	}
	wanted := map[string]bool{"CapEff:": true, "CapPrm:": true, "CapInh:": true, "CapAmb:": true, "NoNewPrivs:": true}
	seen := map[string]bool{}
	confined := true
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !wanted[fields[0]] {
			continue
		}
		key := fields[0]
		if seen[key] || len(fields) != 2 {
			return fmt.Errorf("malformed Linux privilege field %s", key)
		}
		seen[key] = true
		base := 16
		if key == "NoNewPrivs:" {
			base = 10
		}
		value, err := strconv.ParseUint(fields[1], base, 64)
		if err != nil || (key == "NoNewPrivs:" && value > 1) {
			return fmt.Errorf("invalid Linux privilege field %s", key)
		}
		if key == "NoNewPrivs:" {
			confined = confined && value == 1
		} else {
			confined = confined && value == 0
		}
	}
	if len(seen) != len(wanted) {
		return fmt.Errorf("incomplete Linux privilege status")
	}
	if confined {
		return &Refusal{Op: "zero effective, permitted, inheritable and ambient capabilities with NoNewPrivs=1", Err: syscall.EPERM}
	}
	return nil
}
