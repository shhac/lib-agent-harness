package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/sandbox"
)

func selectedPortsCode() string {
	if runtime.GOOS == "darwin" {
		return sandbox.RefusedLoopbackPortsUnenforceable
	}
	return sandbox.RefusedNotOffered
}

func requireSelectedPortsRefusal(t *testing.T, err error, code string) {
	t.Helper()
	var r *UnsupportedError
	if !errors.As(err, &r) || r.Code != code || !strings.Contains(r.Capability.Reason, "Loopback") || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("want %s, got %v", code, err)
	}
	if !strings.Contains(r.Capability.Reason, "sandbox.Open") || !strings.Contains(r.Capability.Reason, "on macOS or Linux") || !strings.Contains(r.Capability.Reason, "unavailable on Windows") {
		t.Fatalf("refusal does not locate the working alternative: %s", r.Capability.Reason)
	}
	if code == RefusedLoopbackPortsUnenforceable {
		if r.HarnessFacts().Family != harness.FailureCapability || !strings.Contains(r.Capability.Reason, harness.LoopbackPortsSeatbeltReason) {
			t.Fatalf("Seatbelt refusal code and reason disagree: %+v", r)
		}
	} else if strings.Contains(r.Capability.Reason, "loopback_ports_unenforceable:") {
		t.Fatalf("non-Seatbelt refusal carries Seatbelt reason: %+v", r)
	}
}

func TestSelectedPortsSessionRefusesBeforePreparation(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok, harness.CommandCode, harness.OpenAICompatible} {
		for _, kind := range []string{"native", "workbench"} {
			for _, operation := range []string{"start", "resume", "verify"} {
				t.Run(string(engine)+"/"+kind+"/"+operation, func(t *testing.T) {
					root := t.TempDir()
					// An unavailable executable cannot launch unnoticed. Even discovery
					// would return a different error; ports must win before that point.
					o := Options{Provider: harness.Provider{Engine: engine, CLI: harness.CLI{Binary: filepath.Join(root, "must-not-launch"), Home: filepath.Join(root, "login")}}, WorkDir: filepath.Join(root, "work"), RuntimeHome: filepath.Join(root, "runtime")}
					if kind == "native" {
						o.Sandbox = &Sandbox{Loopback: true, LoopbackPorts: []int{8080}}
					} else {
						o.Workbench = &Workbench{Commands: &Commands{Loopback: true, LoopbackPorts: []int{8080}}}
					}
					var err error
					switch operation {
					case "start":
						_, err = Start(context.Background(), o)
					case "resume":
						_, err = Resume(context.Background(), o, Ref{})
					case "verify":
						err = VerifySandbox(context.Background(), o)
					}
					requireSelectedPortsRefusal(t, err, selectedPortsCode())
					var r *UnsupportedError
					if !errors.As(err, &r) || r.Capability != harness.Support(engine, harness.Session, harness.LoopbackPorts) {
						t.Fatalf("engine-specific capability lost: %v", err)
					}
					entries, err := os.ReadDir(root)
					if err != nil || len(entries) != 0 {
						t.Fatalf("refusal wrote login/work/runtime state: %v %v", entries, err)
					}
				})
			}
		}
	}
}

func TestSelectedPortsCommandWrapperRefuses(t *testing.T) {
	root := t.TempDir()
	_, err := OpenCommandSandbox(context.Background(), CommandSandboxOptions{WorkDir: root, RuntimeHome: root, Loopback: true, LoopbackPorts: []int{8080}})
	requireSelectedPortsRefusal(t, err, selectedPortsCode())
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("wrapper refusal wrote state: %v %v", entries, err)
	}
}

func TestSelectedPortsSessionValidation(t *testing.T) {
	for _, tt := range []struct {
		ports         []int
		loopback      bool
		control, code string
	}{
		{[]int{}, false, "", sandbox.RefusedLimit},
		{make([]int, 33), false, "", sandbox.RefusedLimit},
		{[]int{0}, false, "resolver.example", sandbox.RefusedLimit},
		{[]int{65536}, true, "", sandbox.RefusedLimit},
		{[]int{80}, false, "", sandbox.RefusedConflict},
		{nil, true, "192.0.2.1", sandbox.RefusedConflict},
		{[]int{80}, true, "127.0.0.1", sandbox.RefusedConflict},
		{[]int{80}, true, "192.0.2.1", selectedPortsCode()},
	} {
		o := Options{Provider: harness.Provider{Engine: harness.Claude}, Sandbox: &Sandbox{Loopback: tt.loopback, LoopbackPorts: tt.ports, LoopbackControl: tt.control}}
		_, err := normalize(o)
		requireSelectedPortsRefusal(t, err, tt.code)
		_, err = normalizeSandbox(o)
		requireSelectedPortsRefusal(t, err, tt.code)
		o.Sandbox = nil
		o.Workbench = &Workbench{Commands: &Commands{Loopback: tt.loopback, LoopbackPorts: tt.ports, LoopbackControl: tt.control}}
		_, err = normalizeWorkbench(o)
		requireSelectedPortsRefusal(t, err, tt.code)
		_, err = normalizeWorkbenchCommands(o)
		requireSelectedPortsRefusal(t, err, tt.code)
	}
}

func TestNilSelectedPortsDigestUnchanged(t *testing.T) {
	o := Options{WorkDir: "/work", Workbench: &Workbench{Commands: &Commands{Loopback: true}}}
	digest, err := json.Marshal(workbenchDigest(o))
	const golden = `{"WorkDir":"/work","Write":false,"Commands":true,"Loopback":true,"Read":null}`
	if err != nil || string(digest) != golden {
		t.Fatalf("nil ports changed legacy digest: %s %v", digest, err)
	}
}
