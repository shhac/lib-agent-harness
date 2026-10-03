//go:build darwin

package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// Frozen stage-A payload: never update this to accommodate a new key.
func legacyworkbenchProbeKey(o Options, system []string) (string, error) {
	info, err := os.Stat("/usr/bin/sandbox-exec")
	if err != nil {
		return "", err
	}
	binary, err := os.ReadFile("/usr/bin/sandbox-exec")
	if err != nil {
		return "", err
	}
	binaryHash := sha256.Sum256(binary)
	payload, _ := json.Marshal(struct {
		Kind, Template, Work, Runtime, Binary string
		Size                                  int64
		Modified                              time.Time
		Write, Loopback                       bool
		Read, System, Env                     []string
		Background                            bool
	}{"workbench", "seatbelt-workbench-v4" + ":" + legacySeatbeltTemplateDigest, o.WorkDir, o.RuntimeHome, hex.EncodeToString(binaryHash[:]), info.Size(), info.ModTime(), o.Workbench.Write, o.Workbench.Commands.Loopback, o.Workbench.Commands.Read, system, o.Workbench.Commands.Env, o.Background})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
func TestWorkbenchProofKeyLegacyPayload(t *testing.T) {

	o := Options{WorkDir: "/workspace", RuntimeHome: "/runtime", Background: true, Workbench: &Workbench{Write: true, Commands: &Commands{Read: []string{"/read"}, Env: []string{"LANG=C"}, Loopback: true}}}
	want, err := legacyworkbenchProbeKey(o, []string{"/System", "/usr"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := workbenchProbeKey(o, []string{"/System", "/usr"})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("proof key changed: %s != %s", got, want)
	}
}

// The profile bytes for this fixed layout are part of the cache key. Never
// regenerate this digest to accommodate the package move.
const legacySeatbeltTemplateDigest = "78a136fe74fd9956132489effed0d86409ed6e0c44fb181722a9c0dd636d4c33"

func TestWorkbenchSeatbeltTemplatePinned(t *testing.T) {
	system := []string{"/System", "/usr", "/bin", "/sbin", "/Library/Developer/CommandLineTools", "/opt/homebrew"}
	if !reflect.DeepEqual(workbenchSystemDirs(), system) {
		t.Fatal("proof system list changed")
	}
	hash := sha256.Sum256([]byte(seatbeltProfile(workbenchLayout{Work: "/workspace", Home: "/home", Tmp: "/tmp", Read: []string{"/read"}, System: system, Write: true, Loopback: true})))
	if hex.EncodeToString(hash[:]) != legacySeatbeltTemplateDigest {
		t.Fatalf("profile template changed: %x", hash)
	}
}
