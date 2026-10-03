//go:build linux

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Frozen stage-A payload: never update this to accommodate a new key.
func legacyworkbenchLinuxProbeKey(o Options, binary, version string, w workbenchLinuxWitness) (string, error) {
	info, e := os.Stat(binary)
	if e != nil {
		return "", e
	}
	data, e := os.ReadFile(binary)
	if e != nil {
		return "", e
	}
	binaryHash := sha256.Sum256(data)
	// Inspect owned fixtures only; canonicalize the random root for stable keys.
	root, e := os.MkdirTemp("", "wb-template-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(root)
	for _, name := range []string{"work/.git", "home", "tmp"} {
		if e := os.MkdirAll(filepath.Join(root, name), 0700); e != nil {
			return "", e
		}
	}
	template, e := legacyBwrapArgs(workbenchLayout{Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), System: legacyBwrapSystemDirs(), Write: true})
	if e != nil {
		return "", e
	}
	for i, arg := range template {
		template[i] = strings.ReplaceAll(arg, root, "/workbench-template")
	}
	var system []string
	for _, p := range legacyBwrapSystemDirs() {
		i, e := os.Lstat(p)
		if os.IsNotExist(e) {
			system = append(system, p+":absent")
			continue
		}
		if e != nil {
			return "", e
		}
		target := ""
		if i.Mode()&os.ModeSymlink != 0 {
			target, e = os.Readlink(p)
			if e != nil {
				return "", e
			}
		}
		system = append(system, fmt.Sprintf("%s:%s:%d:%d:%s", p, i.Mode(), i.Size(), i.ModTime().UnixNano(), target))
	}
	shapes := []string{legacyLinuxShape(o.WorkDir), legacyLinuxShape(o.RuntimeHome), legacyLinuxShape(filepath.Join(o.RuntimeHome, "sessions", "id", "workbench", "home")), legacyLinuxShape(filepath.Join(o.RuntimeHome, "sessions", "id", "workbench", "tmp"))}
	for _, p := range o.Read {
		shapes = append(shapes, legacyLinuxShape(p))
	}
	payload, _ := json.Marshal([]any{"workbench-linux", "bwrap-workbench-v2", binary, info.Size(), info.ModTime(), binaryHash, version, template, system, shapes, o.Write, o.Read, o.Env, o.Loopback, o.Background, struct{ Network, Localhost, Socket string }{w.Network, w.Localhost, w.Socket}})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:]), nil
}
func TestWorkbenchProofKeyLegacyPayload(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(binary, []byte("synthetic binary"), 0600); err != nil {
		t.Fatal(err)
	}
	o := Options{WorkDir: "/workspace", RuntimeHome: "/runtime", Background: true, Write: true, Read: []string{"/read"}, Env: []string{"LANG=C"}, Loopback: true}
	w := workbenchLinuxWitness{Network: "connect", Localhost: "structural", Socket: "connect"}
	want, err := legacyworkbenchLinuxProbeKey(o, binary, "0.8.0", w)
	if err != nil {
		t.Fatal(err)
	}
	got, err := workbenchLinuxProbeKey(o, binary, "0.8.0", w)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("proof key changed: %s != %s", got, want)
	}
}

func legacyLinuxShape(path string) string {
	cover := "/"
	if legacyWithin("/tmp", path) {
		cover = "/tmp"
	}
	if legacyWithin("/run", path) {
		cover = "/run"
	}
	home, _ := os.UserHomeDir()
	if resolved, e := filepath.EvalSymlinks(home); e == nil {
		home = resolved
	}
	return fmt.Sprintf("%s:%t:%d", cover, legacyWithin(home, path), strings.Count(filepath.Clean(path), "/"))
}
