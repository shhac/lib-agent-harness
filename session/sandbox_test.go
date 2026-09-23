//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Sandbox scenarios: how the fake canary behaves, what the fake status check
// reports, and what policy a fake Codex thread says it runs under.
const (
	fakeSandboxOK       = "sandbox-ok"         // every refusal holds; the thread reports the profile
	fakeSandboxLegacy   = "sandbox-legacy"     // canary passes, but the thread runs without the profile
	fakeCanaryTmp       = "canary-tmp"         // /tmp is writable
	fakeCanaryTmpdir    = "canary-tmpdir"      // $TMPDIR is writable
	fakeCanarySibling   = "canary-sibling"     // a file outside the workspace is written
	fakeCanaryNetwork   = "canary-network"     // the probe's listener is reached
	fakeCanaryNoClient  = "canary-no-client"   // nothing to attempt a connection with
	fakeCanaryNoInside  = "canary-no-inside"   // the workspace is not writable
	fakeCanaryCrash     = "canary-crash"       // the sandbox command fails
	fakeStatusDisabled  = "status-disabled"    // sandbox status: not enabled
	fakeStatusLoose     = "status-loose"       // sandbox status: not strict
	fakeStatusMissing   = "status-missing"     // sandbox status: unavailable
	fakeStatusGarbage   = "status-garbage"     // sandbox status: not JSON
	fakeStatusCrash     = "status-crash"       // sandbox status: the command fails
	fakeCanaryWriteOnRO = "canary-write-on-ro" // a read-only workspace is writable
	fakeCanarySilent    = "canary-silent"      // exits cleanly without running the script
	fakeCanaryOpen      = "canary-open"        // runs the real canary script with no sandbox at all
	fakeResumeLegacy    = "resume-legacy"      // thread/start keeps the profile, thread/resume loses it
)

func fakeCodexCanary(scenario string, args []string) int {
	logInvocation("canary")
	write := strings.Contains(strings.Join(args, " "), `"."="write"`)
	if scenario == fakeCanaryOpen {
		// A "sandbox" that sandboxes nothing: the real script, run as is, must
		// be caught by every one of its checks.
		script := args[len(args)-1]
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if cmd.Run() != nil {
			return 2
		}
		return 0
	}
	if scenario == fakeCanarySilent {
		return 0
	}
	lines := []string{}
	switch {
	case scenario == fakeCanaryCrash:
		return 1
	case scenario == fakeCanaryNoInside:
	case write || scenario == fakeCanaryWriteOnRO:
		lines = append(lines, canaryInside)
	}
	switch scenario {
	case fakeCanaryTmp:
		lines = append(lines, canaryTmp)
	case fakeCanaryTmpdir:
		lines = append(lines, canaryTmpdir)
	case fakeCanaryNoClient:
		lines = append(lines, canaryNoClient)
	case fakeCanarySibling:
		if os.WriteFile(os.Getenv("CANARY_SIBLING"), []byte("x"), 0600) != nil {
			return 2
		}
	case fakeCanaryNetwork:
		conn, err := net.Dial("tcp", "127.0.0.1:"+os.Getenv("CANARY_PORT"))
		if err != nil {
			return 2
		}
		conn.Close()
	}
	lines = append(lines, canaryRan)
	_, err := os.Stdout.WriteString(strings.Join(lines, "\n") + "\n")
	if err != nil {
		return 2
	}
	return 0
}

func fakeClaudeSandboxStatus(scenario string) int {
	logInvocation("status")
	status := map[string]any{"statusVersion": 3, "available": false, "installed": false, "supported": true, "enabled": true, "strictMode": true, "unavailableReason": nil}
	switch scenario {
	case fakeStatusCrash:
		return 1
	case fakeStatusGarbage:
		_, _ = os.Stdout.WriteString("sandbox: on\n")
		return 0
	case fakeStatusDisabled:
		status["enabled"] = false
	case fakeStatusLoose:
		status["strictMode"] = false
	case fakeStatusMissing:
		status["unavailableReason"] = "no sandbox runtime"
	}
	if json.NewEncoder(os.Stdout).Encode(status) != nil {
		return 2
	}
	return 0
}

// fakeCodexThread answers thread/start and thread/resume. A sandboxed launch
// carries a permission profile; the fake reports it back the way the installed
// app-server does, unless the scenario is one where the profile was lost.
func fakeCodexThread(scenario string, overrides map[string]string) map[string]any {
	result := map[string]any{"thread": map[string]any{"id": "fake-thread"}}
	if overrides["default_permissions"] == "" {
		return result
	}
	write := strings.Contains(overrides["permissions."+sandboxProfile+".filesystem"], `"."="write"`)
	if scenario == fakeSandboxLegacy || (scenario == fakeResumeLegacy && os.Getenv("AGENT_HARNESS_TEST_RESUMING") == "1") {
		result["sandbox"] = map[string]any{"type": "workspaceWrite", "writableRoots": []string{}, "networkAccess": false, "excludeSlashTmp": false, "excludeTmpdirEnvVar": false}
		result["activePermissionProfile"] = nil
		return result
	}
	if write {
		result["sandbox"] = map[string]any{"type": "workspaceWrite", "writableRoots": []string{}, "networkAccess": false, "excludeSlashTmp": true, "excludeTmpdirEnvVar": true}
	} else {
		result["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": false}
	}
	result["activePermissionProfile"] = map[string]any{"id": sandboxProfile, "extends": nil}
	return result
}

func sandboxOptions(t *testing.T, engine Engine, binary string, write bool) Options {
	t.Helper()
	o := Options{Engine: engine, Binary: binary, WorkDir: t.TempDir(), Sandbox: &Sandbox{Write: write}}
	if engine == Codex {
		o.Home = t.TempDir()
		if err := os.WriteFile(filepath.Join(o.Home, codexCredentialFile), []byte(`{"synthetic":true}`), 0600); err != nil {
			t.Fatal(err)
		}
		o.RuntimeHome = filepath.Join(t.TempDir(), "runtime")
	}
	return o
}

func capabilityCode(t *testing.T, err error) string {
	t.Helper()
	var failure *CapabilityError
	if !errors.As(err, &failure) {
		t.Fatalf("expected a capability error, got %v", err)
	}
	return failure.Code
}

func TestSandboxRejectsSettingsItWouldOverride(t *testing.T) {
	work := t.TempDir()
	cases := map[string]Options{
		"restricted and sandboxed": {Engine: Claude, WorkDir: work, Sandbox: &Sandbox{}, Restriction: &Restriction{}},
		"codex legacy sandbox":     {Engine: Codex, WorkDir: work, RuntimeHome: t.TempDir(), Sandbox: &Sandbox{}, Policy: Policy{CodexSandbox: "workspace-write"}},
		"codex approval":           {Engine: Codex, WorkDir: work, RuntimeHome: t.TempDir(), Sandbox: &Sandbox{}, Policy: Policy{CodexApproval: "on-request"}},
		"codex without home":       {Engine: Codex, WorkDir: work, Sandbox: &Sandbox{}},
		"claude permission":        {Engine: Claude, WorkDir: work, Sandbox: &Sandbox{Write: true}, Policy: Policy{ClaudePermission: "acceptEdits"}},
		"claude web tool":          {Engine: Claude, WorkDir: work, Sandbox: &Sandbox{Write: true}, Policy: Policy{ClaudeTools: []string{"Bash", "WebFetch"}}},
		"claude read-only edit":    {Engine: Claude, WorkDir: work, Sandbox: &Sandbox{}, Policy: Policy{ClaudeTools: []string{"Read", "Edit"}}},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := normalize(o); err == nil {
				t.Fatal("setting accepted")
			}
		})
	}
}

func TestSandboxedClaudeArguments(t *testing.T) {
	for _, write := range []bool{true, false} {
		o, err := normalize(Options{Engine: Claude, WorkDir: t.TempDir(), Sandbox: &Sandbox{Write: write}})
		if err != nil {
			t.Fatal(err)
		}
		args := commandArgs(o, "id", false, &launch{extra: claudeSandboxArgs(o)})
		joined := strings.Join(args, " ")
		for _, want := range []string{"--permission-mode dontAsk", "--setting-sources=", "--strict-mcp-config", "--disallowedTools WebFetch,WebSearch,mcp__*"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("write=%v: missing %q in %v", write, want, args)
			}
		}
		tools := "--tools=Bash,Read,Glob,Grep"
		if write {
			tools = "--tools=Bash,Read,Edit,Write,Glob,Grep"
		}
		if !slices.Contains(args, tools) {
			t.Fatalf("write=%v: expected %s in %v", write, tools, args)
		}
		var settings struct {
			Sandbox struct {
				Enabled, FailIfUnavailable, AllowUnsandboxedCommands bool
				Network                                              struct {
					AllowedDomains  []string
					StrictAllowlist bool
				}
				Filesystem struct{ DenyWrite []string }
			}
			DisableAllHooks bool
			Permissions     struct {
				Allow, Deny                         []string
				DefaultMode                         string
				BlockReadsOutsideWorkingDirectories bool
			}
		}
		if err = json.Unmarshal([]byte(args[slices.Index(args, "--settings")+1]), &settings); err != nil {
			t.Fatal(err)
		}
		if !settings.Sandbox.Enabled || !settings.Sandbox.FailIfUnavailable || settings.Sandbox.AllowUnsandboxedCommands || settings.Sandbox.Network.AllowedDomains == nil || len(settings.Sandbox.Network.AllowedDomains) != 0 || !settings.Sandbox.Network.StrictAllowlist || !settings.DisableAllHooks || settings.Permissions.DefaultMode != "dontAsk" || !settings.Permissions.BlockReadsOutsideWorkingDirectories {
			t.Fatalf("write=%v: sandbox settings too loose: %+v", write, settings)
		}
		if write {
			if !slices.Equal(settings.Permissions.Allow, []string{"Edit(/" + o.WorkDir + "/**)"}) || !slices.Equal(settings.Sandbox.Filesystem.DenyWrite, []string{filepath.Join(o.WorkDir, ".git")}) {
				t.Fatalf("writer confined wrongly: %+v", settings)
			}
		} else if !slices.Equal(settings.Sandbox.Filesystem.DenyWrite, []string{o.WorkDir}) || !slices.Contains(settings.Permissions.Deny, "Edit") || len(settings.Permissions.Allow) != 0 {
			t.Fatalf("read-only session may write: %+v", settings)
		}
	}
}

func TestSandboxedCodexThreadCarriesNoLegacyMode(t *testing.T) {
	o, err := normalize(Options{Engine: Codex, WorkDir: t.TempDir(), RuntimeHome: t.TempDir(), Sandbox: &Sandbox{Write: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, resume := range []bool{false, true} {
		params := codexThreadParams(o, o.WorkDir, resume, "thread")
		if _, present := params["sandbox"]; present {
			t.Fatalf("resume=%v: a legacy sandbox mode would replace the profile: %v", resume, params)
		}
		if params["approvalPolicy"] != "never" {
			t.Fatalf("resume=%v: approval %v", resume, params["approvalPolicy"])
		}
	}
	ordinary, err := normalize(Options{Engine: Codex, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if codexThreadParams(ordinary, ordinary.WorkDir, false, "")["sandbox"] != "read-only" {
		t.Fatal("ordinary sessions changed")
	}
}

func TestSandboxIsPartOfTheReference(t *testing.T) {
	work, home := t.TempDir(), t.TempDir()
	hashes := map[string]bool{}
	for _, sandbox := range []*Sandbox{nil, {Write: false}, {Write: true}} {
		o, err := normalize(Options{Engine: Claude, WorkDir: work, Home: home, Sandbox: sandbox})
		if err != nil {
			t.Fatal(err)
		}
		hashes[reference(o, "id").ConfigHash] = true
	}
	if len(hashes) != 3 {
		t.Fatal("a resume could change the sandbox without changing the reference")
	}
}

func TestCodexSandboxReadback(t *testing.T) {
	profile := `"activePermissionProfile":{"id":"` + sandboxProfile + `"}`
	write := `"sandbox":{"type":"workspaceWrite","writableRoots":[],"networkAccess":false,"excludeSlashTmp":true,"excludeTmpdirEnvVar":true}`
	cases := []struct {
		name   string
		write  bool
		body   string
		accept bool
	}{
		{"writer", true, "{" + write + "," + profile + "}", true},
		{"reader", false, `{"sandbox":{"type":"readOnly","networkAccess":false},` + profile + "}", true},
		{"no profile", true, "{" + write + `,"activePermissionProfile":null}`, false},
		{"other profile", true, "{" + write + `,"activePermissionProfile":{"id":"workspace"}}`, false},
		{"network", true, strings.Replace("{"+write+","+profile+"}", `"networkAccess":false`, `"networkAccess":true`, 1), false},
		{"tmp", true, strings.Replace("{"+write+","+profile+"}", `"excludeSlashTmp":true`, `"excludeSlashTmp":false`, 1), false},
		{"tmpdir", true, strings.Replace("{"+write+","+profile+"}", `"excludeTmpdirEnvVar":true`, `"excludeTmpdirEnvVar":false`, 1), false},
		{"extra roots", true, strings.Replace("{"+write+","+profile+"}", `"writableRoots":[]`, `"writableRoots":["/"]`, 1), false},
		{"reader writes", false, "{" + write + "," + profile + "}", false},
		{"unreadable", true, `not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkCodexSandbox(Options{Sandbox: &Sandbox{Write: c.write}}, []byte(c.body))
			if c.accept != (err == nil) {
				t.Fatalf("accept=%v, got %v", c.accept, err)
			}
			if err != nil && capabilityCode(t, err) != CapabilitySandboxNotEnforced {
				t.Fatalf("unexpected code for %v", err)
			}
		})
	}
}

func TestCodexSandboxCanary(t *testing.T) {
	cases := []struct {
		scenario string
		write    bool
		want     string // "" = verified
	}{
		{fakeSandboxOK, true, ""},
		{fakeSandboxOK, false, ""},
		{fakeCanaryTmp, true, CapabilitySandboxNotEnforced},
		{fakeCanaryTmpdir, true, CapabilitySandboxNotEnforced},
		{fakeCanarySibling, true, CapabilitySandboxNotEnforced},
		{fakeCanaryNetwork, true, CapabilitySandboxNotEnforced},
		{fakeCanaryNetwork, false, CapabilitySandboxNotEnforced},
		{fakeCanaryWriteOnRO, false, CapabilitySandboxNotEnforced},
		{fakeCanaryNoInside, true, CapabilitySandboxUnavailable},
		{fakeCanaryNoClient, true, CapabilitySandboxUnavailable},
		{fakeCanaryCrash, true, CapabilitySandboxUnavailable},
		{fakeCanarySilent, false, CapabilitySandboxUnavailable},
		{fakeCanarySilent, true, CapabilitySandboxUnavailable},
		{fakeCanaryOpen, true, CapabilitySandboxNotEnforced},
		{fakeCanaryOpen, false, CapabilitySandboxNotEnforced},
	}
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			binary, log := fakeHarness(t, c.scenario)
			err := VerifySandbox(context.Background(), sandboxOptions(t, Codex, binary, c.write))
			if c.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if code := capabilityCode(t, err); code != c.want {
				t.Fatalf("want %s, got %s", c.want, code)
			}
			if invocations(t, log, "canary") != 1 {
				t.Fatal("the canary did not run exactly once")
			}
		})
	}
}

func TestClaudeSandboxStatus(t *testing.T) {
	cases := map[string]string{
		fakeSandboxOK:      "",
		fakeStatusDisabled: CapabilitySandboxNotEnforced,
		fakeStatusLoose:    CapabilitySandboxNotEnforced,
		fakeStatusMissing:  CapabilitySandboxUnavailable,
		fakeStatusGarbage:  CapabilitySandboxUnavailable,
		fakeStatusCrash:    CapabilitySandboxUnavailable,
	}
	for scenario, want := range cases {
		t.Run(scenario, func(t *testing.T) {
			binary, log := fakeHarness(t, scenario)
			err := VerifySandbox(context.Background(), sandboxOptions(t, Claude, binary, true))
			if want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if code := capabilityCode(t, err); code != want {
				t.Fatalf("want %s, got %s", want, code)
			}
			if invocations(t, log, "status") != 1 {
				t.Fatal("the status check did not run exactly once")
			}
		})
	}
}

func TestSandboxEvidenceIsCachedPerBinaryAndMode(t *testing.T) {
	binary, log := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, Codex, binary, true)
	for range 2 {
		if err := VerifySandbox(context.Background(), o); err != nil {
			t.Fatal(err)
		}
	}
	if invocations(t, log, "canary") != 1 {
		t.Fatal("proved evidence was not reused")
	}
	o.Sandbox = &Sandbox{Write: false}
	if err := VerifySandbox(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if invocations(t, log, "canary") != 2 {
		t.Fatal("a different sandbox reused another's evidence")
	}
}

func TestSandboxedCodexSessionChecksItsThread(t *testing.T) {
	t.Setenv("LIB_HARNESS_SESSION_FIXTURE", "1")
	binary, log := fakeHarness(t, fakeSandboxOK)
	s, err := Start(context.Background(), sandboxOptions(t, Codex, binary, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if invocations(t, log, "canary") != 1 || invocations(t, log, "session") != 1 {
		t.Fatal("expected one canary before one session")
	}

	binary, log = fakeHarness(t, fakeSandboxLegacy)
	s, err = Start(context.Background(), sandboxOptions(t, Codex, binary, true))
	if s != nil {
		s.Close()
		t.Fatal("a session running without its profile was returned")
	}
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.Code != CapabilitySandboxNotEnforced || failure.Phase != BeforeFirstPrompt {
		t.Fatalf("expected a pre-prompt refusal, got %v", err)
	}
	if invocations(t, log, "session") != 1 {
		t.Fatal("expected the session to have been launched and then refused")
	}
}

func TestSandboxedCodexFailingCanaryStartsNothing(t *testing.T) {
	binary, log := fakeHarness(t, fakeCanaryNetwork)
	s, err := Start(context.Background(), sandboxOptions(t, Codex, binary, true))
	if s != nil {
		s.Close()
		t.Fatal("session started")
	}
	if capabilityCode(t, err) != CapabilitySandboxNotEnforced || invocations(t, log, "session") != 0 {
		t.Fatalf("a credentialed session launched after a failed canary: %v", err)
	}
}

// The open canary must be caught on each check independently, not just once.
func TestOpenCanaryEscapesAreEachDetected(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			conn.Close()
		}
	}()
	cmd := exec.Command("/bin/sh", "-c", canaryScript)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "TMPDIR="+sessionTempDir(), "CANARY_SIBLING="+filepath.Join(root, "outside"), "CANARY_NAME="+filepath.Base(root)+".canary", "CANARY_PORT="+strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(out))
	for _, want := range []string{canaryInside, canarySibling, canaryTmp, canaryTmpdir, canaryRan} {
		if !slices.Contains(got, want) {
			t.Errorf("unsandboxed %s write was not reported: %v", want, got)
		}
	}
}

func TestSandboxedCodexResumeChecksItsThread(t *testing.T) {
	t.Setenv("LIB_HARNESS_SESSION_FIXTURE", "1")
	binary, _ := fakeHarness(t, fakeResumeLegacy)
	o := sandboxOptions(t, Codex, binary, true)
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	ref := s.Ref()
	if _, err = s.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	binary, _ = fakeHarness(t, fakeResumeLegacy, "AGENT_HARNESS_TEST_RESUMING=1")
	o.Binary = binary
	ref = reference(mustNormalize(t, o), ref.ID)
	resumed, err := Resume(context.Background(), o, ref)
	if resumed != nil {
		resumed.Close()
		t.Fatal("a resumed thread without its profile was returned")
	}
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.Code != CapabilitySandboxNotEnforced || failure.Phase != BeforeFirstPrompt {
		t.Fatalf("expected a pre-prompt refusal on resume, got %v", err)
	}
}

func TestSandboxedCodexReleaseReturnsARefreshedLogin(t *testing.T) {
	t.Setenv("LIB_HARNESS_SESSION_FIXTURE", "1")
	binary, _ := fakeHarness(t, fakeSandboxOK, fakeRefreshEnv+`={"refreshed":true}`)
	o := sandboxOptions(t, Codex, binary, true)
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Release(context.Background())
	if err != nil || !out.Confirmed {
		t.Fatalf("release: %+v %v", out, err)
	}
	raw, err := os.ReadFile(filepath.Join(o.Home, codexCredentialFile))
	if err != nil || string(raw) != `{"refreshed":true}` {
		t.Fatalf("refreshed login was not returned to the source home: %q %v", raw, err)
	}
}

func TestVerifySandboxRequiresACodexLogin(t *testing.T) {
	binary, log := fakeHarness(t, fakeSandboxOK)
	o := sandboxOptions(t, Codex, binary, true)
	if err := os.Remove(filepath.Join(o.Home, codexCredentialFile)); err != nil {
		t.Fatal(err)
	}
	if code := capabilityCode(t, VerifySandbox(context.Background(), o)); code != CapabilityLoginUnavailable {
		t.Fatalf("got %s", code)
	}
	if invocations(t, log, "canary") != 0 {
		t.Fatal("the canary ran for a session that could not start")
	}
}

func mustNormalize(t *testing.T, o Options) Options {
	t.Helper()
	out, err := normalize(o)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The sandbox switches off surfaces that reach past it, and nothing a
// sandboxed session needs to do its work.
func TestSandboxKeepsNativeToolsWorking(t *testing.T) {
	args := strings.Join(codexSandboxArgs(true), " ")
	for _, off := range []string{"features.apps=false", "features.plugins=false", "features.hooks=false", "features.browser_use=false", "features.computer_use=false", "web_search=\"disabled\""} {
		if !strings.Contains(args, off) {
			t.Errorf("outward surface left on: %s", off)
		}
	}
	for _, needed := range []string{"code_mode_host", "shell_tool", "unified_exec", "apply_patch"} {
		if strings.Contains(args, "features."+needed+"=false") {
			t.Errorf("a sandboxed session could not work with %s disabled", needed)
		}
	}
}

func TestClaudeSandboxKeepsOperatorInstructionsOut(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	o, err := normalize(Options{Engine: Claude, WorkDir: t.TempDir(), Sandbox: &Sandbox{Write: true}})
	if err != nil {
		t.Fatal(err)
	}
	excludes := claudeInstructionExcludes(o)
	for _, want := range []string{filepath.Join(home, ".claude", "CLAUDE.md"), filepath.Join(filepath.Dir(o.WorkDir), "CLAUDE.local.md"), "/CLAUDE.md"} {
		if !slices.Contains(excludes, want) {
			t.Errorf("%s would still load", want)
		}
	}
	for _, e := range excludes {
		if strings.HasPrefix(e, o.WorkDir+string(filepath.Separator)) {
			t.Errorf("the workspace's own instructions are excluded: %s", e)
		}
	}
	var settings struct {
		ClaudeMdExcludes []string
		Sandbox          struct{ Filesystem struct{ DenyWrite []string } }
		Permissions      struct{ Deny []string }
	}
	if err = json.Unmarshal([]byte(claudeSandboxSettings(o)), &settings); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(o.WorkDir, ".git")
	if len(settings.ClaudeMdExcludes) == 0 || !slices.Contains(settings.Sandbox.Filesystem.DenyWrite, gitDir) || !slices.Contains(settings.Permissions.Deny, "Edit(/"+gitDir+"/**)") {
		t.Fatalf("writer may change repository metadata or read operator instructions: %+v", settings)
	}
}

func TestEnvAdditionsComeLastAndCannotTouchManagedKeys(t *testing.T) {
	o, err := normalize(Options{Engine: Claude, WorkDir: t.TempDir(), Env: []string{"GOCACHE=/work/.crew/go", "TMPDIR=/work/.crew/tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	env := environment(o)
	if env[len(env)-2] != "GOCACHE=/work/.crew/go" || env[len(env)-1] != "TMPDIR=/work/.crew/tmp" {
		t.Fatalf("additions must come last to take effect: %v", env[len(env)-2:])
	}
	for _, bad := range []string{"HOME=/x", "PATH=/x", "CODEX_HOME=/x", "ANTHROPIC_API_KEY=x", "CLAUDE_CODE_USE_BEDROCK=1", "no-equals", "1BAD=x"} {
		if _, err := normalize(Options{Engine: Claude, WorkDir: t.TempDir(), Env: []string{bad}}); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
