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

func TestNativeSelectedPortsRefuseBeforeLogin(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Claude, harness.Codex, harness.Grok, harness.CommandCode} {
		for _, operation := range []string{"start", "resume", "verify"} {
			t.Run(string(engine)+"/"+operation, func(t *testing.T) {
				root := t.TempDir()
				o := Options{Provider: harness.Provider{Engine: engine, CLI: harness.CLI{Home: filepath.Join(root, "login"), Binary: filepath.Join(root, "missing")}}, RuntimeHome: filepath.Join(root, "runtime"), Sandbox: &Sandbox{Loopback: true, LoopbackPorts: []int{3000}}}
				var err error
				switch operation {
				case "start":
					_, err = Start(context.Background(), o)
				case "resume":
					_, err = Resume(context.Background(), o, Ref{})
				case "verify":
					err = VerifySandbox(context.Background(), o)
				}
				var refused *UnsupportedError
				if !errors.As(err, &refused) || refused.Code != RefusedNotOffered || refused.Operation != "loopback_ports" || refused.Capability != harness.Support(engine, harness.Session, harness.LoopbackPorts) {
					t.Fatalf("wrong pre-launch refusal: %v", err)
				}
				for _, p := range []string{o.RuntimeHome, o.Provider.CLI.Home} {
					if _, err := os.Stat(p); !os.IsNotExist(err) {
						t.Fatal("refusal created state")
					}
				}
			})
		}
	}
}

func TestAPISandboxSelectedPortsRefusalIsUnsupported(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}, Sandbox: &Sandbox{Loopback: true, LoopbackPorts: []int{3000}}}
	_, err := normalize(o)
	var refusal *UnsupportedError
	if !errors.As(err, &refusal) || refusal.Code != RefusedNotOffered || refusal.Capability.Availability != harness.Unsupported {
		t.Fatal("native Sandbox advertised workbench support", err)
	}
}

func TestWorkbenchSelectedPortRefPolicy(t *testing.T) {
	o := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible, API: harness.API{BaseURL: "https://gateway.invalid/v1", Dialect: harness.OpenAIChatCompletions}}, Model: "model", RuntimeHome: "/state", WorkDir: "/work", Restriction: &Restriction{Tools: ToolHost{Server: "work"}}, Workbench: &Workbench{Commands: &Commands{Loopback: true}}}
	legacy, _ := json.Marshal(workbenchDigest(o))
	if string(legacy) != `{"WorkDir":"/work","Write":false,"Commands":true,"Loopback":true,"Read":null}` {
		t.Fatalf("legacy digest changed: %s", legacy)
	}
	id := newID()
	legacyRef := apiReference(o, id)
	o.Workbench.Commands.LoopbackPorts = []int{3000, 8080}
	ref := apiReference(o, id)
	if compatible(o, legacyRef) {
		t.Fatal("adding ports reused unrestricted reference")
	}
	if !compatible(o, ref) {
		t.Fatal("same ports incompatible")
	}
	o.Workbench.Commands.LoopbackControl = "192.0.2.53"
	if !compatible(o, ref) {
		t.Fatal("control altered permissions")
	}
	o.Workbench.Commands.LoopbackPorts = []int{3001, 8080}
	if compatible(o, ref) {
		t.Fatal("changed ports accepted")
	}
	o.Workbench.Commands.LoopbackPorts = nil
	if compatible(o, ref) {
		t.Fatal("removed ports accepted")
	}
	if !compatible(o, legacyRef) {
		t.Fatal("legacy reference changed")
	}
}

func TestPortPolicyCompatibilityWrapperAndWorkbenchRefusals(t *testing.T) {
	for _, ports := range [][]int{{}, {-1}, {3000}} {
		if runtime.GOOS == "darwin" && len(ports) == 1 && ports[0] == 3000 {
			continue
		}
		_, wrapper := OpenCommandSandbox(context.Background(), CommandSandboxOptions{Loopback: true, LoopbackPorts: ports})
		_, workbench := normalize(Options{Provider: harness.Provider{Engine: harness.OpenAICompatible}, Workbench: &Workbench{Commands: &Commands{Loopback: true, LoopbackPorts: ports}}})
		var a, b *UnsupportedError
		if !errors.As(wrapper, &a) || !errors.As(workbench, &b) || a.Code != b.Code || a.Capability != b.Capability {
			t.Fatalf("policy mismatch: %v / %v", wrapper, workbench)
		}
	}
}
