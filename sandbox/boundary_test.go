package sandbox

import (
	"go/ast"
	"go/parser"
	"go/token"
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

func TestSandboxIntegrationHelpersRemainInternal(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			switch fn.Name.Name {
			case "Normalize", "NormalizeWorkbench", "ReadDirs", "PrivateStateDir", "LockState":
				t.Errorf("%s exports internal helper %s", name, fn.Name.Name)
			}
		}
	}
}
