package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

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
