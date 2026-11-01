package nativecli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrEdgeInterpreter = errors.New("edge_interpreter_unavailable")
var ErrEdgeNative = errors.New("edge_native_unavailable")

// EdgeInstallation follows fixed recognized npm layouts, including aliases.
// Interpreter permission is literal, never a directory grant.
// A Standalone installation is one self-contained native executable, such as
// agy or fx in ~/.local/bin: only that file is granted, never the directory
// that holds it beside unrelated tools.
type EdgeInstallation struct {
	Root         string
	VendorRoots  []string
	Executables  []string
	Interpreter  string
	NativeBinary string
	NpmCodex     bool
	Standalone   bool
}

func DiscoverEdgeInstallation(binary, pathEnv string) (EdgeInstallation, error) {
	binary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return EdgeInstallation{}, err
	}
	result := EdgeInstallation{Root: filepath.Dir(binary), Executables: []string{binary}}
	add := func(path string) bool {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return false
		}
		st, err := os.Stat(real)
		if err != nil || !st.Mode().IsRegular() {
			return false
		}
		result.Executables = append(result.Executables, real)
		return true
	}
	if standaloneEngine(binary) {
		result.Standalone = true
		return result, nil
	}
	if filepath.Base(binary) == "codex" {
		add(filepath.Join(result.Root, "codex-code-mode-host"))
	}
	for dir, depth := result.Root, 0; depth < 3; dir, depth = filepath.Dir(dir), depth+1 {
		raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			continue
		}
		var manifest struct{ Name string }
		if json.Unmarshal(raw, &manifest) != nil || (manifest.Name != "@openai/codex" && manifest.Name != "@anthropic-ai/claude-code") {
			continue
		}
		result.Root = dir
		add(filepath.Join(dir, "package.json"))
		f, err := os.Open(binary)
		if err != nil {
			return result, err
		}
		header := make([]byte, 128)
		n, _ := f.Read(header)
		f.Close()
		line := strings.SplitN(string(header[:n]), "\n", 2)[0]
		if strings.HasPrefix(line, "#!") && strings.Contains(line, "node") {
			for _, parent := range filepath.SplitList(pathEnv) {
				if !filepath.IsAbs(parent) {
					continue
				}
				path := filepath.Join(parent, "node")
				st, e := os.Stat(path)
				if e != nil || !st.Mode().IsRegular() || (runtime.GOOS != "windows" && st.Mode().Perm()&0111 == 0) {
					continue
				}
				result.Interpreter, e = filepath.EvalSymlinks(path)
				if e == nil {
					add(result.Interpreter)
					break
				}
			}
			if result.Interpreter == "" {
				return result, ErrEdgeInterpreter
			}
		}
		if manifest.Name == "@openai/codex" {
			result.NpmCodex = true
			found := false
			for _, arch := range []string{"arm64", "x64"} {
				triple := "aarch64-apple-darwin"
				if arch == "x64" {
					triple = "x86_64-apple-darwin"
				}
				for _, root := range []string{dir, filepath.Join(filepath.Dir(dir), "codex-darwin-"+arch)} {
					// Codex 0.160 and 0.166 ship the binary under bin/; codex/ is
					// the layout this first looked for.
					path := filepath.Join(root, "vendor", triple, "bin", "codex")
					if !add(path) {
						path = filepath.Join(root, "vendor", triple, "codex", "codex")
						if !add(path) {
							continue
						}
					}
					found = true
					if arch == runtime.GOARCH || (arch == "x64" && runtime.GOARCH == "amd64") {
						result.NativeBinary, _ = filepath.EvalSymlinks(path)
					}
					add(filepath.Join(filepath.Dir(path), "codex-code-mode-host"))
					if root != dir {
						real, e := filepath.EvalSymlinks(root)
						if e != nil {
							return result, e
						}
						result.VendorRoots = append(result.VendorRoots, real)
						add(filepath.Join(root, "package.json"))
					}
				}
			}
			if !found {
				return result, ErrEdgeNative
			}
		}
		break
	}
	return result, nil
}

// standaloneEngines are the CLIs that ship as one native executable with no
// package beside it. Their installation is that file alone.
var standaloneEngines = []string{"agy", "fx"}

// standaloneEngine reports a recognized single-file native CLI: a known name
// and a Mach-O or ELF executable rather than a script or launcher.
func standaloneEngine(binary string) bool {
	known := false
	for _, name := range standaloneEngines {
		known = known || filepath.Base(binary) == name
	}
	if !known {
		return false
	}
	f, err := os.Open(binary)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	if n, _ := f.Read(magic); n != 4 {
		return false
	}
	switch string(magic) {
	case "\xcf\xfa\xed\xfe", "\xca\xfe\xba\xbe", "\x7fELF":
		return true
	}
	return false
}
