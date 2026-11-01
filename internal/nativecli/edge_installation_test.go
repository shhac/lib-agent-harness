package nativecli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNpmEdgeAliasAndInterpreterIdentity(t *testing.T) {
	for _, native := range [][]string{{"bin", "codex"}, {"codex", "codex"}} {
		t.Run(filepath.Join(native...), func(t *testing.T) { testNpmEdge(t, native) })
	}
}

func testNpmEdge(t *testing.T, native []string) {
	root := t.TempDir()
	pkg := filepath.Join(root, "@openai", "codex")
	vendor := filepath.Join(root, "@openai", "codex-darwin-arm64")
	launcher := filepath.Join(pkg, "bin", "codex.js")
	node := filepath.Join(root, "node-bin", "node")
	for path, data := range map[string]string{launcher: "#!/usr/bin/env node\n", filepath.Join(pkg, "package.json"): `{"name":"@openai/codex"}`, filepath.Join(vendor, "package.json"): `{"name":"@openai/codex"}`, filepath.Join(append([]string{vendor, "vendor", "aarch64-apple-darwin"}, native...)...): "native", node: "node-one"} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	pathEnv := filepath.Dir(node)
	installation, err := DiscoverEdgeInstallation(launcher, pathEnv)
	if err != nil || installation.Interpreter == "" || len(installation.VendorRoots) != 1 {
		t.Fatal("alias/interpreter not discovered", err)
	}
	first, err := EdgeIdentityWithPath(launcher, pathEnv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, []byte("node-two"), 0700); err != nil {
		t.Fatal(err)
	}
	second, err := EdgeIdentityWithPath(launcher, pathEnv)
	if err != nil || second == first {
		t.Fatal("interpreter not bound", err)
	}
	_, err = DiscoverEdgeInstallation(launcher, "")
	if !errors.Is(err, ErrEdgeInterpreter) {
		t.Fatal("missing interpreter admitted", err)
	}
}

func TestStandaloneEdgeInstallationIsTheFileAlone(t *testing.T) {
	dir := t.TempDir()
	native := filepath.Join(dir, "agy")
	script := filepath.Join(dir, "fx")
	if err := os.WriteFile(native, []byte("\xcf\xfa\xed\xfe native"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	installation, err := DiscoverEdgeInstallation(native, "")
	resolved, _ := filepath.EvalSymlinks(native)
	if err != nil || !installation.Standalone || len(installation.Executables) != 1 || installation.Executables[0] != resolved {
		t.Fatal("standalone binary not recognized alone", installation, err)
	}
	if installation, err = DiscoverEdgeInstallation(script, ""); err != nil || installation.Standalone {
		t.Fatal("script launcher recognized as a standalone native binary", err)
	}
}
