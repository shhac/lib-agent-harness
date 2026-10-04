package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func requireWorkbenchBwrap(t *testing.T) (string, string) {
	t.Helper()
	testenv.RequireLoopback(t) // Real command proofs need network witnesses.
	testenv.RequireProcessGroup(t)
	l := linuxTestLayout(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var binary, version string
	testenv.RequireBwrap(t, ctx, func() error {
		var e error
		binary, version, e = checkBwrap(ctx, l)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var cap *ProofError
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

func TestWorkbenchLinuxProbeKey(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "bwrap")
	if e := os.WriteFile(binary, []byte("fixture"), 0700); e != nil {
		t.Fatal(e)
	}
	o := workbenchOptions(t, nopHandler())
	o.Workbench.Commands = &Commands{}
	w := workbenchLinuxWitness{"connect", "connect", "structural"}
	base, e := workbenchLinuxProbeKey(testCommandOptions(o), binary, "0.8.0", w)
	if e != nil {
		t.Fatal(e)
	}
	again, e := workbenchLinuxProbeKey(testCommandOptions(o), binary, "0.8.0", w)
	if e != nil || again != base {
		t.Fatalf("temporary template changed key: %v", e)
	}
	for _, change := range []func(*testOptions, *string, *workbenchLinuxWitness){
		func(o *testOptions, v *string, w *workbenchLinuxWitness) { o.Workbench.Write = true },
		func(o *testOptions, v *string, w *workbenchLinuxWitness) { o.Workbench.Commands.Loopback = true },
		func(o *testOptions, v *string, w *workbenchLinuxWitness) {
			o.Workbench.Commands.Read = []string{"/toolchain"}
		},
		func(o *testOptions, v *string, w *workbenchLinuxWitness) { o.WorkDir += "/deeper" },
		func(o *testOptions, v *string, w *workbenchLinuxWitness) { *v = "0.10.0" },
		func(o *testOptions, v *string, w *workbenchLinuxWitness) { w.Socket = "connect" },
		func(o *testOptions, v *string, w *workbenchLinuxWitness) { o.Background = true },
	} {
		changed := o
		wb := *o.Workbench
		c := *wb.Commands
		wb.Commands = &c
		changed.Workbench = &wb
		v := "0.8.0"
		w2 := w
		change(&changed, &v, &w2)
		key, e := workbenchLinuxProbeKey(testCommandOptions(changed), binary, v, w2)
		if e != nil || key == base {
			t.Fatalf("key collision %v", e)
		}
	}
}

func TestBwrapSupportedTrials(t *testing.T) {
	testenv.RequireProcessGroup(t)
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
