package session

// Proving a sandbox: the check that the installed harness enforces it, run
// before a credentialed launch, and the read-back of what a started Codex
// session reports it is running under.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const sandboxProbeTimeout = 60 * time.Second

func verifySandbox(ctx context.Context, o Options, l *launch) error {
	key, err := sandboxKey(o, l)
	if err != nil {
		return err
	}
	if verified.holds(key) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, sandboxProbeTimeout)
	defer cancel()
	if o.Engine == Codex {
		err = probeCodexSandbox(ctx, o)
	} else {
		err = probeClaudeSandbox(ctx, o)
	}
	if err != nil {
		return err
	}
	verified.record(key)
	return nil
}

// sandboxKey identifies exactly what a sandbox check established: this binary
// as it is on disk now, with these sandbox arguments.
func sandboxKey(o Options, l *launch) (string, error) {
	binary, info, err := binaryIdentity(o)
	if err != nil {
		return "", &CapabilityError{Engine: string(o.Engine), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	payload, _ := json.Marshal(struct {
		Kind     string
		Engine   Engine
		Binary   string
		Size     int64
		Modified time.Time
		Write    bool
		Read     []string
		Args     []string
		TempDir  string
	}{"sandbox", o.Engine, binary, info.Size(), info.ModTime(), o.Sandbox.Write, o.Sandbox.Read, l.extra, sessionTempDir()})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// probeClaudeSandbox asks the installed CLI for the sandbox status it would
// apply with exactly this session's settings and no other settings source.
//
// "available" and "installed" are not required: on macOS they describe an
// optional Windows installer and read false while the Seatbelt sandbox is in
// force. What must hold is that the sandbox is supported, enabled and strict,
// with no reason it is unavailable.
func probeClaudeSandbox(ctx context.Context, o Options) error {
	// A throwaway home: with every settings source but this one dropped, the
	// operator's own configuration cannot change the answer, and the login is
	// never needed to ask the question.
	home, err := os.MkdirTemp("", "agent-harness-sandbox-status-")
	if err != nil {
		return &CapabilityError{Engine: string(Claude), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	defer os.RemoveAll(home)
	args := []string{"--setting-sources=", "--settings", claudeSandboxSettings(o), "sandbox", "status"}
	out, err := runOnce(ctx, o.Binary, args, o.WorkDir, disposableEnvironment(o, home))
	if err != nil {
		return &CapabilityError{Engine: string(Claude), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	var status struct {
		Supported         bool    `json:"supported"`
		Enabled           bool    `json:"enabled"`
		StrictMode        bool    `json:"strictMode"`
		UnavailableReason *string `json:"unavailableReason"`
	}
	if json.Unmarshal(out, &status) != nil {
		return &CapabilityError{Engine: string(Claude), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	if !status.Supported || status.UnavailableReason != nil {
		return &CapabilityError{Engine: string(Claude), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	if !status.Enabled || !status.StrictMode {
		return &CapabilityError{Engine: string(Claude), Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch}
	}
	return nil
}

// Canary outcomes, one per line, written by the probe script.
const (
	canaryInside   = "inside"
	canarySibling  = "sibling"
	canaryTmp      = "tmp"
	canaryTmpdir   = "tmpdir"
	canaryNetwork  = "network"
	canaryNoClient = "no-network-client"
	canaryGitDir   = "gitdir"
	// canaryRan is the script's last line. Without it an empty result could
	// mean "everything refused" or "nothing ran", and only one is evidence.
	canaryRan = "canary-ran"
)

// canaryScript attempts, from inside the sandbox, everything the session must
// be refused, and reports each attempt that succeeded. It never reaches past
// the machine: the network attempt targets a listener this probe owns.
const canaryScript = `
try() { name=$1; shift; if ( "$@" ) >/dev/null 2>&1; then echo "$name"; fi; }
try inside sh -c 'echo x > ./canary'
try sibling sh -c 'echo x > "$1"' _ "$CANARY_SIBLING"
try tmp sh -c 'echo x > "/tmp/$CANARY_NAME" && rm -f "/tmp/$CANARY_NAME"'
try tmpdir sh -c 'echo x > "$TMPDIR/$CANARY_NAME" && rm -f "$TMPDIR/$CANARY_NAME"'
try gitdir sh -c 'echo x >> .git/config'
if command -v curl >/dev/null 2>&1; then curl -s -m 3 --noproxy '*' -o /dev/null "http://127.0.0.1:$CANARY_PORT/"
elif command -v nc >/dev/null 2>&1; then nc -z -w 3 127.0.0.1 "$CANARY_PORT"
else echo no-network-client; fi
echo canary-ran
exit 0
`

// probeCodexSandbox runs the canary under the same permission profile the
// session uses, in a disposable home, and checks every refusal. The temporary
// directory is the one a real session inherits, not the probe's own.
func probeCodexSandbox(ctx context.Context, o Options) error {
	unavailable := &CapabilityError{Engine: string(Codex), Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	notEnforced := &CapabilityError{Engine: string(Codex), Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch}
	root, err := os.MkdirTemp("", "agent-harness-sandbox-")
	if err != nil {
		return unavailable
	}
	defer os.RemoveAll(root)
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	sibling := filepath.Join(root, "outside")
	for _, dir := range []string{home, filepath.Join(workspace, ".git"), sibling} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return unavailable
		}
	}
	// Repository metadata must stay read-only even for a writing session.
	if err = os.WriteFile(filepath.Join(workspace, ".git", "config"), []byte("[core]\n"), 0600); err != nil {
		return unavailable
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return unavailable
	}
	defer listener.Close()
	reached := make(chan struct{}, 1)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			conn.Close()
			select {
			case reached <- struct{}{}:
			default:
			}
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	env := disposableEnvironment(o, home)
	env = slices.DeleteFunc(env, func(entry string) bool { return strings.HasPrefix(entry, "TMPDIR=") })
	env = append(env, "TMPDIR="+sessionTempDir(), "CANARY_SIBLING="+filepath.Join(sibling, "canary"), "CANARY_NAME="+filepath.Base(root)+".canary", "CANARY_PORT="+strconv.Itoa(port))
	args := append([]string{"sandbox", "-P", sandboxProfile, "-C", workspace}, codexSandboxArgs(*o.Sandbox)...)
	args = append(args, "--", "/bin/sh", "-c", canaryScript)
	out, err := runOnce(ctx, o.Binary, args, workspace, env)
	if err != nil {
		if ctx.Err() != nil {
			return &CapabilityError{Engine: string(Codex), Code: CapabilityProbeTimeout, Phase: BeforeLaunch}
		}
		return unavailable
	}
	escaped := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		escaped[strings.TrimSpace(line)] = true
	}
	if !escaped[canaryRan] || escaped[canaryNoClient] {
		return unavailable
	}
	// The listener may accept a moment after the canary's client has exited.
	select {
	case <-reached:
		escaped[canaryNetwork] = true
	case <-time.After(500 * time.Millisecond):
	}
	if escaped[canaryInside] != o.Sandbox.Write {
		if o.Sandbox.Write {
			return unavailable // the workspace it must write to is not writable
		}
		return notEnforced
	}
	if escaped[canarySibling] || escaped[canaryTmp] || escaped[canaryTmpdir] || escaped[canaryNetwork] || escaped[canaryGitDir] {
		return notEnforced
	}
	if _, err = os.Stat(filepath.Join(sibling, "canary")); err == nil {
		return notEnforced
	}
	return nil
}

// sessionTempDir is the temporary directory a harness launched from this
// process would inherit.
func sessionTempDir() string {
	if dir := os.Getenv("TMPDIR"); dir != "" {
		return dir
	}
	return os.TempDir()
}

// checkCodexSandbox reads back the policy the running harness applied to its
// thread. A thread that is not under this session's profile, with the network
// closed and temporary directories excluded, is closed before any prompt.
func checkCodexSandbox(o Options, body []byte) error {
	var response struct {
		Sandbox struct {
			Type                string   `json:"type"`
			NetworkAccess       bool     `json:"networkAccess"`
			WritableRoots       []string `json:"writableRoots"`
			ExcludeSlashTmp     bool     `json:"excludeSlashTmp"`
			ExcludeTmpdirEnvVar bool     `json:"excludeTmpdirEnvVar"`
		} `json:"sandbox"`
		Profile *struct {
			ID string `json:"id"`
		} `json:"activePermissionProfile"`
	}
	notEnforced := &CapabilityError{Engine: string(Codex), Code: CapabilitySandboxNotEnforced, Phase: BeforeFirstPrompt}
	if json.Unmarshal(body, &response) != nil {
		return notEnforced
	}
	sandbox := response.Sandbox
	if response.Profile == nil || response.Profile.ID != sandboxProfile || sandbox.NetworkAccess || len(sandbox.WritableRoots) > 0 {
		return notEnforced
	}
	if !o.Sandbox.Write {
		if sandbox.Type != "readOnly" {
			return notEnforced
		}
		return nil
	}
	if sandbox.Type != "workspaceWrite" || !sandbox.ExcludeSlashTmp || !sandbox.ExcludeTmpdirEnvVar {
		return notEnforced
	}
	return nil
}
