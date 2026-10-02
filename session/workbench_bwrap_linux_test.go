package session

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func requireWorkbenchBwrap(t *testing.T) (string, string) {
	t.Helper()
	testenv.RequireProcessGroup(t)
	l := linuxTestLayout(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var binary, version string
	testenv.RequireBwrap(t, func() error {
		var e error
		binary, version, e = checkBwrap(ctx, l)
		var cap *CapabilityError
		if errors.As(e, &cap) && (cap.Code == CapabilitySandboxToolMissing || cap.Code == CapabilitySandboxToolOutdated || cap.Code == CapabilitySandboxNamespacesUnavailable) {
			return errors.Join(fs.ErrPermission, e)
		}
		return e
	})
	if os.Getenv("AGENT_HARNESS_TEST_BWRAP_080") == "1" && version != "0.8.0" {
		t.Fatalf("expected minimum bwrap, got %s", version)
	}
	return binary, version
}

func TestBwrapRefusalsBeforeState(t *testing.T) {
	testenv.RequireProcessGroup(t)
	for _, tc := range []struct{ name, script, code string }{
		{"missing", "", CapabilitySandboxToolMissing},
		{"old", "echo 'bubblewrap 0.7.0'", CapabilitySandboxToolOutdated},
		{"garbage", "echo garbage", CapabilitySandboxToolOutdated},
		{"version failure", "echo 'bubblewrap 0.8.0'; exit 1", CapabilitySandboxToolOutdated},
		{"namespaces", "if [ \"$1\" = --version ]; then echo 'bubblewrap 0.8.0'; exit 0; fi; echo PRIVATE-PROVIDER-TEXT >&2; exit 1", CapabilitySandboxNamespacesUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir()
			t.Setenv("PATH", path)
			if tc.script != "" {
				if e := os.WriteFile(filepath.Join(path, "bwrap"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); e != nil {
					t.Fatal(e)
				}
			}
			o := workbenchOptions(t, nopHandler())
			o.Workbench.Commands = &Commands{}
			_, e := Start(context.Background(), o)
			var cap *CapabilityError
			if !errors.As(e, &cap) || cap.Code != tc.code || cap.Phase != BeforeLaunch || !reflect.DeepEqual(cap.Tools, []string{"bwrap"}) {
				t.Fatalf("%v", e)
			}
			if strings.Contains(e.Error(), "PRIVATE-PROVIDER-TEXT") {
				t.Fatal("stderr leaked")
			}
			entries, e := os.ReadDir(o.RuntimeHome)
			if e != nil || len(entries) != 0 {
				t.Fatalf("state created: %v %v", entries, e)
			}
		})
	}
	for _, version := range []string{"0.8.0", "0.10.0", "1.0"} {
		t.Run(version, func(t *testing.T) {
			path := t.TempDir()
			t.Setenv("PATH", path)
			script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'bubblewrap " + version + "'; fi\nexit 0\n"
			if e := os.WriteFile(filepath.Join(path, "bwrap"), []byte(script), 0700); e != nil {
				t.Fatal(e)
			}
			_, got, e := checkBwrap(context.Background(), linuxTestLayout(t))
			if e != nil || got != version {
				t.Fatalf("%s %v", got, e)
			}
		})
	}
	// Preserve the established macOS missing-tool wording.
	e := workbenchCapability(CapabilitySandboxToolMissing)
	e.Tools = []string{"sandbox-exec"}
	if !strings.Contains(e.Error(), "required OS sandbox tool is missing") {
		t.Fatal(e)
	}
}

// Simulate the mount operations on a new empty root. An implicit directory
// made by bwrap would hide a builder bug and is deliberately not permitted.

func TestWorkbenchLinuxNormalize(t *testing.T) {
	for _, edit := range []func(*Options){func(o *Options) { o.WorkDir = "/usr" }, func(o *Options) { o.RuntimeHome = "/etc" }, func(o *Options) { o.WorkDir, _ = os.UserHomeDir() }, func(o *Options) { o.WorkDir = "/" }} {
		o := workbenchOptions(t, nopHandler())
		o.Workbench.Commands = &Commands{}
		edit(&o)
		if _, e := normalizeWorkbenchCommands(o); e == nil {
			t.Fatal("accepted unsafe placement")
		}
	}
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Commands = &Commands{Read: []string{"/usr/bin"}}
	normalized, e := normalizeWorkbenchCommands(o)
	if e != nil || len(normalized.Workbench.Commands.Read) != 0 {
		t.Fatalf("%+v %v", normalized.Workbench.Commands, e)
	}
	o = workbenchOptions(t, nopHandler())
	o.Workbench.Commands = &Commands{Loopback: true}
	_, e = Start(context.Background(), o)
	var unsupported *UnsupportedError
	if !errors.As(e, &unsupported) || unsupported.Capability != harness.Support(harness.OpenAICompatible, harness.Session, harness.Loopback) {
		t.Fatalf("%v", e)
	}
	entries, _ := os.ReadDir(o.RuntimeHome)
	if len(entries) != 0 {
		t.Fatal("loopback wrote state")
	}
}

func TestWorkbenchLinuxSocketReadRefusedBeforeCanary(t *testing.T) {
	for _, path := range []string{"/run", "/var/run", "/tmp"} {
		t.Run(path, func(t *testing.T) {
			if _, e := os.Stat(path); os.IsNotExist(e) {
				t.Skip("host alias absent")
			}
			o := workbenchOptions(t, nopHandler())
			o.Workbench.Commands = &Commands{Read: []string{path}}
			t.Setenv("PATH", t.TempDir()) // No bwrap: normalization must refuse first.
			_, e := Start(context.Background(), o)
			var refused *UnsupportedError
			if !errors.As(e, &refused) || refused.Code != RefusedSandboxRead {
				t.Fatalf("%v", e)
			}
			entries, e := os.ReadDir(o.RuntimeHome)
			if e != nil || len(entries) != 0 {
				t.Fatalf("refusal wrote state: %v %v", entries, e)
			}
		})
	}
}

func TestWorkbenchLinuxProbeKey(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "bwrap")
	if e := os.WriteFile(binary, []byte("fixture"), 0700); e != nil {
		t.Fatal(e)
	}
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Commands = &Commands{}
	w := workbenchLinuxWitness{"connect", "connect", "structural"}
	base, e := workbenchLinuxProbeKey(o, binary, "0.8.0", w)
	if e != nil {
		t.Fatal(e)
	}
	again, e := workbenchLinuxProbeKey(o, binary, "0.8.0", w)
	if e != nil || again != base {
		t.Fatalf("temporary template changed key: %v", e)
	}
	for _, change := range []func(*Options, *string, *workbenchLinuxWitness){
		func(o *Options, v *string, w *workbenchLinuxWitness) { o.Workbench.Write = true },
		func(o *Options, v *string, w *workbenchLinuxWitness) {
			o.Workbench.Commands.Read = []string{"/toolchain"}
		},
		func(o *Options, v *string, w *workbenchLinuxWitness) { o.WorkDir += "/deeper" },
		func(o *Options, v *string, w *workbenchLinuxWitness) { *v = "0.10.0" },
		func(o *Options, v *string, w *workbenchLinuxWitness) { w.Socket = "connect" },
		func(o *Options, v *string, w *workbenchLinuxWitness) { o.Background = true },
	} {
		changed := o
		wb := *o.Workbench
		c := *wb.Commands
		wb.Commands = &c
		changed.Workbench = &wb
		v := "0.8.0"
		w2 := w
		change(&changed, &v, &w2)
		key, e := workbenchLinuxProbeKey(changed, binary, v, w2)
		if e != nil || key == base {
			t.Fatalf("key collision %v", e)
		}
	}
}
