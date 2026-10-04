package main

import (
	"strings"
	"testing"
)

func TestCheckCommandKeepsStrictModeInsideBoundary(t *testing.T) {
	path := "/tool chain/owner's go"
	for _, strict := range []bool{false, true} {
		command := checkCommand(path, strict)
		if strings.Contains(command, "AGENT_HARNESS_TEST_NO_SKIP=1") != strict || !strings.Contains(command, `GOCACHE="$TMPDIR/go-cache"`) {
			t.Fatal(command)
		}
		if strings.Count(command, `'/tool chain/owner'"'"'s go'`) != 2 || !strings.Contains(command, "vet ./... &&") || !strings.HasSuffix(command, "test -race ./...") {
			t.Fatal(command)
		}
	}
}
