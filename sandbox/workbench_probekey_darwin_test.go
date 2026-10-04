//go:build darwin

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

// Frozen stage-A payload shape: only the pinned template below may change it.
func legacyworkbenchProbeKey(o Options, system []string, template string) (string, error) {
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
	}{"workbench", template, o.WorkDir, o.RuntimeHome, hex.EncodeToString(binaryHash[:]), info.Size(), info.ModTime(), o.Write, o.Loopback, o.Read, system, o.Env, o.Background})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
func TestWorkbenchProofKeyLegacyPayload(t *testing.T) {

	o := Options{WorkDir: "/workspace", RuntimeHome: "/runtime", Background: true, Write: true, Read: []string{"/read"}, Env: []string{"LANG=C"}, Loopback: true}
	want, err := legacyworkbenchProbeKey(o, []string{"/System", "/usr"}, "seatbelt-workbench-v7:"+legacySeatbeltTemplateDigest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := workbenchProbeKey(o, []string{"/System", "/usr"})
	if err != nil {
		t.Fatal(err)
	}
	if got == want {
		t.Fatal("old proof certifies changed policy")
	}
	v8, err := legacyworkbenchProbeKey(o, []string{"/System", "/usr"}, "seatbelt-workbench-v8:readable-path-v1:965de8a96277296731642e137ae818d5de2b28c970467bb52e2fe7784d0fe23b")
	if err != nil || got == v8 {
		t.Fatalf("old fixture proof certifies corrected native canary: %v", err)
	}
	v9, err := legacyworkbenchProbeKey(o, []string{"/System", "/usr"}, "seatbelt-workbench-v9:readable-path-v1:965de8a96277296731642e137ae818d5de2b28c970467bb52e2fe7784d0fe23b")
	if err != nil || got == v9 {
		t.Fatalf("draft-4 proof reused: %v", err)
	}
	v10, err := legacyworkbenchProbeKey(o, []string{"/System", "/usr"}, "seatbelt-workbench-v10:readable-path-v2:965de8a96277296731642e137ae818d5de2b28c970467bb52e2fe7784d0fe23b")
	if err != nil || got == v10 {
		t.Fatalf("draft-5 proof reused: %v", err)
	}
	again, err := workbenchProbeKey(o, []string{"/System", "/usr"})
	if err != nil || got != again {
		t.Fatalf("unstable proof: %s %s %v", got, again, err)
	}
}

// The profile bytes for this fixed layout are part of the cache key. Regenerate
// this digest only together with a version bump.
const legacySeatbeltTemplateDigest = "88cb94d215ea86a2f47fbbbb4ccf909836fa1cf8ec5798517af6dae3565b8dad"

func TestWorkbenchSeatbeltTemplatePinned(t *testing.T) {
	system := []string{"/System", "/usr", "/bin", "/sbin", "/Library/Developer/CommandLineTools", "/opt/homebrew"}
	if !reflect.DeepEqual(workbenchSystemDirs(), system) {
		t.Fatal("proof system list changed")
	}
	hash := sha256.Sum256([]byte(seatbeltProfile(workbenchLayout{Work: "/workspace", Home: "/home", Tmp: "/tmp", Read: []string{"/read"}, System: system, Write: true, Loopback: true})))
	if hex.EncodeToString(hash[:]) != "965de8a96277296731642e137ae818d5de2b28c970467bb52e2fe7784d0fe23b" {
		t.Fatalf("profile template changed: %x", hash)
	}
}
