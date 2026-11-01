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
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A versioned native file reached through an alias of another name, as
	// ~/.local/bin/agy -> ~/.local/share/agy/1.4.12/agy-arm64.
	versions := filepath.Join(dir, "share", "agy", "1.4.12")
	if err := os.MkdirAll(versions, 0700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(versions, "agy-arm64")
	if err := os.WriteFile(native, []byte("\xcf\xfa\xed\xfe native"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "agy")
	if err := os.Symlink(native, alias); err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{native, alias} {
		installation, err := DiscoverStandaloneInstallation(binary)
		if err != nil || !installation.Standalone || len(installation.Executables) != 1 || installation.Executables[0] != native {
			t.Fatal("standalone binary not the file alone", binary, installation, err)
		}
	}
	first, err := EdgeIdentityFor(alias, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte("\xcf\xfa\xed\xfe changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if second, err := EdgeIdentityFor(alias, "", true); err != nil || second == first {
		t.Fatal("standalone identity not bound to the target file", err)
	}
	launcher := filepath.Join(dir, "launcher", "agy")
	if err := os.MkdirAll(filepath.Dir(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec \"$(dirname \"$0\")/agy-arm64\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverStandaloneInstallation(launcher); !errors.Is(err, ErrEdgeStandalone) {
		t.Fatal("launcher script admitted as a standalone installation", err)
	}
	if _, err := EdgeIdentityFor(launcher, "", true); !errors.Is(err, ErrEdgeStandalone) {
		t.Fatal("launcher script identified", err)
	}
	// Name no longer decides: a native file named agy is not standalone for
	// an engine that does not declare it.
	if installation, err := DiscoverEdgeInstallation(alias, ""); err != nil || installation.Standalone {
		t.Fatal("name decided standalone-ness", err)
	}
}
