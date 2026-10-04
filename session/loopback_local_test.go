package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestLoopbackLocalOnlySessionRefusal(t *testing.T) {
	for _, loopback := range []bool{false, true} {
		o := Options{Provider: harness.Provider{Engine: harness.Claude}, Sandbox: &Sandbox{Loopback: loopback, LoopbackLocalOnly: true}}
		_, err := normalizeSandbox(o)
		var r *UnsupportedError
		if !errors.As(err, &r) {
			t.Fatalf("%v", err)
		}
		want := RefusedConflict
		if loopback {
			want = RefusedLoopbackNotLocal
		}
		if r.Code != want {
			t.Fatalf("%s != %s", r.Code, want)
		}
	}
}

func TestLoopbackLocalOnlyCommandWrapperRefusal(t *testing.T) {
	if runtime.GOOS != "darwin" {
		return
	}
	_, err := OpenCommandSandbox(context.Background(), CommandSandboxOptions{Loopback: true, LoopbackLocalOnly: true})
	var r *UnsupportedError
	if !errors.As(err, &r) || r.Code != RefusedLoopbackNotLocal {
		t.Fatalf("%v", err)
	}
	o := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}, Workbench: &Workbench{Commands: &Commands{Loopback: true, LoopbackLocalOnly: true}}}
	_, err = normalizeWorkbenchCommands(o)
	if !errors.As(err, &r) || r.Code != RefusedLoopbackNotLocal {
		t.Fatalf("%v", err)
	}
}

func TestWorkbenchLocalOnlyPreProofRefusal(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}, Workbench: &Workbench{Commands: &Commands{Loopback: true, LoopbackLocalOnly: true}}}
	_, err := normalizeWorkbenchCommands(o)
	var r *UnsupportedError
	want := RefusedNotOffered
	if runtime.GOOS == "darwin" {
		want = RefusedLoopbackNotLocal
	}
	if !errors.As(err, &r) || r.Code != want {
		t.Fatalf("%v", err)
	}
}

func TestNativeLocalOnlyRefusesBeforePreparation(t *testing.T) {
	for _, operation := range []string{"start", "resume", "verify"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			o := Options{Provider: harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Binary: filepath.Join(root, "must-not-launch"), Home: filepath.Join(root, "login")}}, WorkDir: root, RuntimeHome: filepath.Join(root, "runtime"), Sandbox: &Sandbox{Loopback: true, LoopbackLocalOnly: true}}
			var err error
			switch operation {
			case "start":
				_, err = Start(context.Background(), o)
			case "resume":
				_, err = Resume(context.Background(), o, Ref{})
			case "verify":
				err = VerifySandbox(context.Background(), o)
			}
			var r *UnsupportedError
			if !errors.As(err, &r) || r.Code != RefusedLoopbackNotLocal {
				t.Fatalf("%v", err)
			}
			for _, p := range []string{o.RuntimeHome, o.Provider.CLI.Home} {
				if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refusal touched private state: %v", err)
				}
			}
		})
	}
}

func TestLoopbackLocalOnlyDigest(t *testing.T) {
	o := Options{WorkDir: "/work", Workbench: &Workbench{Commands: &Commands{Loopback: true}}}
	before, _ := json.Marshal(workbenchDigest(o))
	const golden = `{"WorkDir":"/work","Write":false,"Commands":true,"Loopback":true,"Read":null}`
	if string(before) != golden {
		t.Fatalf("legacy digest changed: %s", before)
	}
	o.Workbench.Commands.LoopbackLocalOnly = true
	after, _ := json.Marshal(workbenchDigest(o))
	if string(before) == string(after) {
		t.Fatal("local-only power missing from digest")
	}
	native := Options{Provider: harness.Provider{Engine: harness.Claude}, Sandbox: &Sandbox{Loopback: true}}
	ref := reference(native, "id")
	native.Sandbox.LoopbackLocalOnly = true
	if reference(native, "id").ConfigHash == ref.ConfigHash {
		t.Fatal("native digest omitted local-only")
	}
}
