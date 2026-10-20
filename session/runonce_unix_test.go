//go:build !windows

package session

import (
	"errors"
	"os"
	"strconv"
	"testing"
)

// A short command's output is read whole or not at all: one that prints more
// than the limit is refused rather than parsed from its first part.
func TestRunOnceRefusesOutputBeyondItsLimit(t *testing.T) {
	dir := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH")}
	out, err := runOnce(testContext(t), "/bin/sh", []string{"-c", "printf ok"}, dir, env)
	if err != nil || string(out) != "ok" {
		t.Fatalf("%q %v", out, err)
	}
	over := strconv.Itoa(runOnceLimit + 1)
	if _, err := runOnce(testContext(t), "/bin/sh", []string{"-c", "head -c " + over + " /dev/zero"}, dir, env); !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("cut output was accepted: %v", err)
	}
}
