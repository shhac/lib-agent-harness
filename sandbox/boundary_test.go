package sandbox

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSandboxImportBoundary(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, pkg := range []string{"session", "completion", "catalog", "account", "native"} {
			prefix := "github.com/shhac/lib-agent-harness/" + pkg
			if dep == prefix || strings.HasPrefix(dep, prefix+"/") {
				t.Fatalf("sandbox depends on %s", dep)
			}
		}
	}
}
