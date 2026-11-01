package sandbox

import (
	"errors"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestEdgeNpmLiteralInterpreterAndAliasGrant(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "install", "@openai", "codex")
	arch, triple := "arm64", "aarch64-apple-darwin"
	if runtime.GOARCH == "amd64" {
		arch, triple = "x64", "x86_64-apple-darwin"
	}
	vendor := filepath.Join(filepath.Dir(pkg), "codex-darwin-"+arch)
	native := filepath.Join(vendor, "vendor", triple, "codex", "codex")
	launcher := filepath.Join(pkg, "bin", "codex.js")
	node := filepath.Join(root, "node-bin", "node")
	work, home := filepath.Join(root, "work"), filepath.Join(root, "runtime")
	for _, dir := range []string{work, home} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{launcher: "#!/usr/bin/env node\n", filepath.Join(pkg, "package.json"): `{"name":"@openai/codex"}`, filepath.Join(vendor, "package.json"): `{"name":"@openai/codex"}`, native: "native", node: "node"} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	c := sandboxbridge.EdgeConfig{Binary: launcher, Work: work, Runtime: home, CLIHome: filepath.Join(root, "owner-login"), Env: []string{"PATH=" + filepath.Dir(node)}, ProxyPort: 1234}
	plan, err := buildEdge(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Args) != 5 || plan.Args[3] != node || plan.Args[4] != launcher {
		t.Fatal("interpreter not invoked explicitly")
	}
	for _, operation := range []string{"file-read*", "process-exec"} {
		if !strings.Contains(plan.Args[1], "(allow "+operation+" (literal "+strconv.Quote(node)+"))") {
			t.Fatal("literal grant missing")
		}
	}
	if !strings.Contains(plan.Args[1], "(subpath "+strconv.Quote(vendor)+")") {
		t.Fatal("aliased vendor grant missing")
	}
	if strings.Contains(plan.Args[1], "(allow file-read* (subpath "+strconv.Quote(filepath.Dir(node))+")") {
		t.Fatal("interpreter directory widened")
	}
	c.Env = nil
	_, err = buildEdge(c)
	if !errors.Is(err, nativecli.ErrEdgeInterpreter) {
		t.Fatal("missing interpreter admitted", err)
	}
	c.Env = []string{"PATH=" + filepath.Dir(node)}
	if err := os.Remove(native); err != nil {
		t.Fatal(err)
	}
	_, err = buildEdge(c)
	if !errors.Is(err, nativecli.ErrEdgeNative) {
		t.Fatal("missing vendor admitted", err)
	}
}
