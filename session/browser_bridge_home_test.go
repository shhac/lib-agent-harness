//go:build !windows

package session

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestCheckBrowserBridgeReasons(t *testing.T) {
	for _, reason := range []string{"", BridgeHomeUnavailable, BridgeNotDeclared, BridgeDeclarationUnsupported, BridgeInstallationMissing, BridgeInWorkspace} {
		t.Run(reason, func(t *testing.T) {
			binary, log := fakeHarness(t, fakeSandboxOK)
			o := sandboxOptions(t, "codex", binary, true)
			o.Browser = true
			o.BrowserBridgeHome = t.TempDir()
			fakeBrowserBridge(t, o)
			if err := os.Remove(filepath.Join(o.Provider.CLI.Home, codexCredentialFile)); err != nil {
				t.Fatal(err)
			}
			declaration := filepath.Join(o.BrowserBridgeHome, "fake-browser-bridge.json")
			raw, err := os.ReadFile(declaration)
			if err != nil {
				t.Fatal(err)
			}
			bridge, err := parseCodexBrowserBridge(raw)
			if err != nil {
				t.Fatal(err)
			}
			switch reason {
			case BridgeHomeUnavailable:
				o.BrowserBridgeHome = filepath.Join(t.TempDir(), "absent")
			case BridgeNotDeclared:
				if err := os.Remove(declaration); err != nil {
					t.Fatal(err)
				}
			case BridgeDeclarationUnsupported:
				raw = []byte(strings.Replace(string(raw), `"enabled":true`, `"enabled":false`, 1))
				if err := os.WriteFile(declaration, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case BridgeInstallationMissing:
				if err := os.Remove(bridge.Command); err != nil {
					t.Fatal(err)
				}
			case BridgeInWorkspace:
				o.WorkDir = filepath.Dir(bridge.Command)
			}
			before, _ := os.ReadDir(o.BrowserBridgeHome)
			err = CheckBrowserBridge(probeContext(t), o)
			if reason == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var failure *CapabilityError
				if !errors.As(err, &failure) || failure.Code != CapabilityBrowserBridgeUnavailable || failure.Phase != BeforeLaunch || failure.Reason != reason {
					t.Fatalf("reason %s: %v (%+v)", reason, err, failure)
				}
			}
			if reason == BridgeHomeUnavailable && invocations(t, log, "browser-config") != 0 {
				t.Fatal("missing home ran CLI")
			}
			if invocations(t, log, "browser-proof") != 0 || invocations(t, log, "canary") != 0 || invocations(t, log, "session") != 0 {
				t.Fatal("check started work")
			}
			if _, err := os.Stat(o.RuntimeHome); !os.IsNotExist(err) {
				t.Fatal("check wrote runtime home")
			}
			after, _ := os.ReadDir(o.BrowserBridgeHome)
			if len(before) != len(after) {
				t.Fatal("check added files")
			}
		})
	}
}

func TestReadBrowserBridgeNamedHome(t *testing.T) {
	binary, log := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, harness.Codex, binary, true)
	o.Browser = true
	o.BrowserBridgeHome = t.TempDir()
	fakeBrowserBridge(t, o)
	if _, err := os.Stat(filepath.Join(o.Provider.CLI.Home, "fake-browser-bridge.json")); !os.IsNotExist(err) {
		t.Fatal("CLI home has a declaration")
	}
	bridge, err := readCodexBrowserBridge(probeContext(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.home != o.BrowserBridgeHome || invocations(t, log, "browser-config") != 1 {
		t.Fatal("read did not use the named home exactly once")
	}
	raw, err := bridge.config(o.RuntimeHome)
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(raw)
	if !strings.Contains(cfg, "CODEX_HOME = ") || !strings.Contains(cfg, o.RuntimeHome) || strings.Count(cfg, "[mcp_servers.") != 2 {
		t.Fatal("private configuration missing narrowed bridge")
	}
	for _, forbidden := range []string{o.Provider.CLI.Home, o.BrowserBridgeHome, "secret", "desktop", "sky", "NODE_OPTIONS"} {
		if strings.Contains(cfg, forbidden) {
			t.Fatalf("configuration inherited %q", forbidden)
		}
	}
	if invocations(t, log, "browser-proof") != 0 || invocations(t, log, "session") != 0 {
		t.Fatal("read started work")
	}
}

func TestSandboxBrowserNamedHomeAndCache(t *testing.T) {
	testenv.RequireLoopback(t) // Provider fixtures and canaries need a real loopback bind.
	binary, log := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, "codex", binary, true)
	o.Browser = true
	o.BrowserBridgeHome = t.TempDir()
	fakeBrowserBridge(t, o)
	ctx := probeContext(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	testenv.SkipIfRefused(t, "loopback for sandbox proof", err)
	listener.Close()
	for i := range 2 {
		if err := VerifySandbox(ctx, o); err != nil {
			t.Fatal(err)
		}
		if invocations(t, log, "browser-config") != i+1 {
			t.Fatal("verification did not read exactly one declaration")
		}
	}
	if invocations(t, log, "browser-proof") != 1 {
		t.Fatal("same home not cached")
	}
	old := o.BrowserBridgeHome
	o.BrowserBridgeHome = t.TempDir()
	raw, err := os.ReadFile(filepath.Join(old, "fake-browser-bridge.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.BrowserBridgeHome, "fake-browser-bridge.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySandbox(ctx, o); err != nil {
		t.Fatal(err)
	}
	if invocations(t, log, "browser-proof") != 2 {
		t.Fatal("different home reused proof")
	}
	s, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	cfg, err := os.ReadFile(filepath.Join(o.RuntimeHome, codexConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), o.BrowserBridgeHome) || !strings.Contains(string(cfg), "CODEX_HOME = ") || !strings.Contains(string(cfg), o.RuntimeHome) {
		t.Fatal("bridge inherited owner home")
	}
	for _, forbidden := range []string{o.Provider.CLI.Home, "secret", "desktop", "sky", "NODE_OPTIONS"} {
		if strings.Contains(string(cfg), forbidden) {
			t.Fatalf("runtime inherited %q", forbidden)
		}
	}
}

func TestSandboxBrowserAbsentInNamedHome(t *testing.T) {
	binary, log := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, "codex", binary, true)
	o.Browser = true
	fakeBrowserBridge(t, o)
	o.BrowserBridgeHome = t.TempDir()
	for _, check := range []func() error{
		func() error { return VerifySandbox(probeContext(t), o) },
		func() error {
			s, err := Start(probeContext(t), o)
			if s != nil {
				s.Close()
				t.Fatal("started")
			}
			return err
		},
	} {
		var failure *CapabilityError
		if err := check(); !errors.As(err, &failure) || failure.Reason != BridgeNotDeclared {
			t.Fatalf("%v", err)
		}
	}
	if invocations(t, log, "browser-proof") != 0 || invocations(t, log, "session") != 0 {
		t.Fatal("started before bridge read")
	}
}

func TestBrowserBridgeHomeRefusals(t *testing.T) {
	for _, name := range []string{"unset", "claude", "no sandbox", "restriction", "inside", "equal", "containing", "symlink", "missing under symlink"} {
		t.Run(name, func(t *testing.T) {
			o := sandboxOptions(t, "codex", "unused", true)
			o.Browser = true
			o.BrowserBridgeHome = t.TempDir()
			want := RefusedConflict
			switch name {
			case "unset":
				o.Browser = false
				want = RefusedNotConfigured
			case "claude":
				o.Provider.Engine = "claude"
			case "no sandbox":
				o.Sandbox = nil
			case "restriction":
				o.Restriction = &Restriction{}
			case "inside":
				o.BrowserBridgeHome = filepath.Join(o.WorkDir, "home")
			case "equal":
				o.BrowserBridgeHome = o.WorkDir
			case "containing":
				o.BrowserBridgeHome = filepath.Dir(o.WorkDir)
			case "symlink":
				o.BrowserBridgeHome = filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(o.WorkDir, o.BrowserBridgeHome); err != nil {
					t.Fatal(err)
				}
			case "missing under symlink":
				alias := filepath.Join(t.TempDir(), "alias")
				parent := t.TempDir()
				if err := os.Mkdir(filepath.Join(parent, "workspace"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent, alias); err != nil {
					t.Fatal(err)
				}
				o.WorkDir = filepath.Join(alias, "workspace")
				o.BrowserBridgeHome = filepath.Join(o.WorkDir, "missing", "home")
			}
			var failure *UnsupportedError
			if err := CheckBrowserBridge(context.Background(), o); !errors.As(err, &failure) || failure.Code != want {
				t.Fatalf("%v", err)
			}
			family := harness.FailureCapability
			if want == RefusedNotConfigured {
				family = harness.FailurePreflight
			}
			if failure.HarnessFacts().Family != family {
				t.Fatal("wrong refusal family")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckBrowserBridge(ctx, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCheckBrowserBridgeCheckRefusals(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude} {
		t.Run(string(engine), func(t *testing.T) {
			o := sandboxOptions(t, engine, "must-not-run", true)
			o.Browser = true
			o.Sandbox = nil
			var failure *UnsupportedError
			if err := CheckBrowserBridge(context.Background(), o); !errors.As(err, &failure) || failure.Code != RefusedConflict || failure.Operation != "browser_bridge" {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestBrowserBridgeUnavailableWorkspace(t *testing.T) {
	for _, kind := range []string{"missing", "file", "broken symlink"} {
		t.Run(kind, func(t *testing.T) {
			binary, log := fakeHarness(t, fakeSandboxOK)
			o := sandboxOptions(t, harness.Codex, binary, true)
			o.Browser = true
			fakeBrowserBridge(t, o)
			bridge, err := readCodexBrowserBridge(probeContext(t), o)
			if err != nil {
				t.Fatal(err)
			}
			o.WorkDir = filepath.Join(t.TempDir(), "unavailable")
			switch kind {
			case "file":
				if err := os.WriteFile(o.WorkDir, []byte("file"), 0600); err != nil {
					t.Fatal(err)
				}
			case "broken symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), o.WorkDir); err != nil {
					t.Fatal(err)
				}
			}
			for _, check := range []func() error{
				func() error { return bridge.outsideWorkspace(o.WorkDir) },
				func() error { return CheckBrowserBridge(probeContext(t), o) },
				func() error { return VerifySandbox(probeContext(t), o) },
				func() error {
					s, err := Start(probeContext(t), o)
					if s != nil {
						s.Close()
						t.Fatal("started with unavailable workspace")
					}
					return err
				},
			} {
				var failure *UnsupportedError
				if err := check(); !errors.As(err, &failure) || failure.Code != RefusedWorkDir || failure.Operation != "work_dir" {
					t.Fatalf("%v", err)
				}
				if failure.HarnessFacts().Family != harness.FailurePreflight {
					t.Fatal("wrong workspace refusal family")
				}
			}
			if invocations(t, log, "browser-config") != 1 {
				t.Fatal("unavailable workspace ran CLI")
			}
		})
	}
}

func TestBrowserBridgeCancellationDuringRead(t *testing.T) {
	for _, operation := range []string{"check", "start", "resume", "verify"} {
		t.Run(operation, func(t *testing.T) {
			binary, log := fakeHarness(t, "browser-config-block")
			o := sandboxOptions(t, harness.Codex, binary, true)
			o.Browser = true
			o.BrowserBridgeHome = t.TempDir()
			normalized, err := normalize(o)
			if err != nil {
				t.Fatal(err)
			}
			ref := reference(normalized, "synthetic-thread")
			parent := probeContext(t)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				switch operation {
				case "check":
					result <- CheckBrowserBridge(ctx, o)
				case "verify":
					result <- VerifySandbox(ctx, o)
				case "start":
					s, err := Start(ctx, o)
					if s != nil {
						s.Close()
					}
					result <- err
				case "resume":
					s, err := Resume(ctx, o, ref)
					if s != nil {
						s.Close()
					}
					result <- err
				}
			}()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for invocations(t, log, "browser-config") == 0 {
				select {
				case err := <-result:
					t.Fatalf("read returned before cancellation: %v", err)
				case <-parent.Done():
					t.Fatal("fake never started mcp get")
				case <-ticker.C:
				}
			}
			cancel()
			select {
			case err := <-result:
				if operation == "check" {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation lost: %v", err)
					}
				} else {
					var failure *CapabilityError
					if !errors.As(err, &failure) || failure.Code != CapabilityBrowserBridgeUnavailable || failure.Reason != BridgeNotDeclared || failure.Phase != BeforeLaunch {
						t.Fatalf("%v", err)
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled mcp get did not settle")
			}
			if invocations(t, log, "session") != 0 || invocations(t, log, "browser-proof") != 0 || invocations(t, log, "canary") != 0 {
				t.Fatal("cancelled read started work")
			}
		})
	}
}

func TestSandboxToolHomeUnderSymlink(t *testing.T) {
	o := sandboxToolConfigOptions(t, harness.Codex, "must-not-run", false)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(o.WorkDir, alias); err != nil {
		t.Fatal(err)
	}
	o.WorkDir = alias
	o.Sandbox.Tools.Dir = filepath.Join(alias, "tools")
	if err := os.Mkdir(o.Sandbox.Tools.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	var failure *UnsupportedError
	if _, err := normalize(o); !errors.As(err, &failure) || failure.Code != RefusedConflict || failure.Operation != "tools" {
		t.Fatalf("%v", err)
	}
}

func TestPathContainmentWithMissingComponents(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{alias, filepath.Join(canonical, "missing", "home")},
		{filepath.Join(alias, "missing"), filepath.Join(canonical, "missing", "home")},
		{filepath.Join(alias, "missing", "home"), canonical},
	} {
		if !pathsOverlap(pair[0], pair[1]) || !pathsOverlap(pair[1], pair[0]) {
			t.Fatalf("overlap missed: %v", pair)
		}
	}
	if pathsOverlap(filepath.Join(alias, "home"), filepath.Join(canonical, "home-other")) {
		t.Fatal("sibling prefix is not containment")
	}
}

func TestSandboxBrowserHomeKey(t *testing.T) {
	binary, _ := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, "codex", binary, true)
	o.Browser = true
	o.BrowserBridgeHome = t.TempDir()
	fakeBrowserBridge(t, o)
	ctx := probeContext(t)
	bridge, err := readCodexBrowserBridge(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	key, err := sandboxKey(o, &launch{browser: bridge})
	if err != nil {
		t.Fatal(err)
	}
	other := o
	other.BrowserBridgeHome = t.TempDir()
	raw, err := os.ReadFile(filepath.Join(o.BrowserBridgeHome, "fake-browser-bridge.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other.BrowserBridgeHome, "fake-browser-bridge.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	otherBridge, err := readCodexBrowserBridge(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := sandboxKey(other, &launch{browser: otherBridge})
	if err != nil {
		t.Fatal(err)
	}
	if key == otherKey {
		t.Fatal("key omits bridge home")
	}
}

func TestBrowserBridgeHomeIsNotConversationIdentity(t *testing.T) {
	binary, _ := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, "codex", binary, true)
	o.Browser = true
	o.BrowserBridgeHome = t.TempDir()
	a, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	o.BrowserBridgeHome = t.TempDir()
	b, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	if reference(a, "same") != reference(b, "same") {
		t.Fatal("bridge home changed conversation identity")
	}
}

func TestCheckBrowserBridgeDefaultHome(t *testing.T) {
	binary, log := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, "codex", binary, true)
	o.Browser = true
	fakeBrowserBridge(t, o)
	if err := CheckBrowserBridge(probeContext(t), o); err != nil {
		t.Fatal(err)
	}
	if invocations(t, log, "browser-config") != 1 {
		t.Fatal("default did not read CLI home")
	}
	o.BrowserBridgeHome = filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(o.BrowserBridgeHome, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	var failure *CapabilityError
	if err := CheckBrowserBridge(probeContext(t), o); !errors.As(err, &failure) || failure.Reason != BridgeHomeUnavailable {
		t.Fatalf("%v", err)
	}
	if invocations(t, log, "browser-config") != 1 {
		t.Fatal("non-directory home ran CLI")
	}
}

func TestCheckBrowserBridgeUnreadableHome(t *testing.T) {
	binary, log := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, harness.Codex, binary, true)
	o.Browser = true
	o.BrowserBridgeHome = t.TempDir()
	if err := os.Chmod(o.BrowserBridgeHome, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(o.BrowserBridgeHome, 0700); err != nil {
			t.Error(err)
		}
	})
	if dir, err := os.Open(o.BrowserBridgeHome); err == nil {
		dir.Close()
		t.Skip("process privileges permit opening an unreadable directory")
	} else if !os.IsPermission(err) {
		t.Fatal(err)
	}
	var failure *CapabilityError
	if err := CheckBrowserBridge(probeContext(t), o); !errors.As(err, &failure) || failure.Reason != BridgeHomeUnavailable || failure.Phase != BeforeLaunch {
		t.Fatalf("%v", err)
	}
	if invocations(t, log, "browser-config") != 0 {
		t.Fatal("unreadable home ran CLI")
	}
}
