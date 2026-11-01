package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
)

const edgeSeatbeltVersion = "seatbelt-edge-v5"

func init() { sandboxbridge.BuildEdge = buildEdge }

func buildEdge(c sandboxbridge.EdgeConfig) (sandboxbridge.EdgePlan, error) {
	refused := func() (sandboxbridge.EdgePlan, error) {
		return sandboxbridge.EdgePlan{}, errors.New("edge_confinement_failed")
	}
	if c.ProxyPort < 1 || c.ProxyPort > 65535 {
		return refused()
	}
	located, err := exec.LookPath(c.Binary)
	if err != nil {
		return refused()
	}
	bin, err := filepath.EvalSymlinks(located)
	if err != nil {
		return refused()
	}
	pathEnv := ""
	for _, entry := range c.Env {
		key, value, _ := strings.Cut(entry, "=")
		if key == "PATH" {
			pathEnv = value
		}
	}
	installation, err := nativecli.DiscoverEdgeInstallation(bin, pathEnv)
	if err != nil {
		return sandboxbridge.EdgePlan{}, err
	}
	if installation.NpmCodex && installation.NativeBinary == "" {
		return sandboxbridge.EdgePlan{}, nativecli.ErrEdgeNative
	}
	install, dependencies := installation.Root, installation.VendorRoots
	// Include only the actual installation, never its enclosing owner home.
	read, problem := sandboxbridge.ReadDirs(append(append(append([]string(nil), c.Read...), install), dependencies...))
	if problem != "" {
		return refused()
	}
	cliHome, _ := filepath.EvalSymlinks(c.CLIHome)
	if cliHome == "" {
		cliHome = filepath.Clean(c.CLIHome)
	}
	for _, dir := range append([]string{install}, dependencies...) {
		overlap := Nested(dir, cliHome) || Within(cliHome, dir)
		if _, err := os.Stat(cliHome); err == nil {
			overlap = overlap || Nested(cliHome, dir)
		}
		if cliHome != "." && overlap {
			return sandboxbridge.EdgePlan{}, errors.New("edge_install_credentials")
		}
		// Check installation roots, not caller Read trees: caller grants keep
		// their shared semantics and may contain unrelated credential fixtures.
		for _, name := range []string{"auth.json", ".credentials.json", "credentials.json"} {
			if _, e := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(e) {
				return sandboxbridge.EdgePlan{}, errors.New("edge_install_credentials")
			}
		}
	}
	o := Options{WorkDir: c.Work, RuntimeHome: c.Runtime, Read: read, Write: c.Write, Loopback: c.Loopback}
	// Shared normalization validates private homes, workspace separation,
	// system and caller grants without preparing writable state.
	o, err = normalize(o, true)
	if err != nil {
		return refused()
	}
	l := workbenchLayout{Work: o.WorkDir, Home: o.RuntimeHome, Tmp: filepath.Join(o.RuntimeHome, "tmp"), Read: o.Read, System: o.system, Write: o.Write, Loopback: o.Loopback}
	if c.Bridge != "" {
		bridge, e := filepath.EvalSymlinks(c.Bridge)
		if e != nil || !l.readsDirectory(filepath.Dir(bridge)) {
			return refused()
		}
	}
	profile := edgeProfile(l, c.ProxyPort, c.Socket) + edgeInterpreterProfile(installation.Interpreter) + edgePreferenceProfile(c.Preferences)
	if c.ToolState != "" {
		if Nested(l.Home, c.ToolState) || Nested(l.Work, c.ToolState) {
			return refused()
		}
		profile += fmt.Sprintf("(deny file-write* (subpath %s))\n(deny file-link (subpath %s))\n(deny file-read* (require-all (subpath %s) (require-not (literal %s))))\n(allow file-read* (literal %s))\n", strconv.Quote(c.ToolState), strconv.Quote(c.ToolState), strconv.Quote(c.ToolState), strconv.Quote(c.Secret), strconv.Quote(c.Secret))
	}
	profile += fmt.Sprintf("; Selected owner login is never a native read grant.\n(deny file-read* (subpath %s))\n", strconv.Quote(cliHome))
	var env []string
	var pathDrops []workbenchPathDrop
	for _, entry := range c.Env {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "PATH":
			filtered, dropped := filterWorkbenchPath(l, value)
			if filtered == "" {
				filtered = l.Tmp
			}
			env = append(env, "PATH="+filtered)
			pathDrops = append(pathDrops, dropped...)
		case "HOME", "TMPDIR":
		default:
			env = append(env, entry)
		}
	}
	env = append(env, "HOME="+l.Home, "TMPDIR="+l.Tmp)
	// Ephemeral transport endpoints enter the launch profile, never its key.
	socketTemplate := ""
	if c.Bridge != "" {
		socketTemplate = "/edge-tool-socket"
	}
	template := edgeProfile(l, 1, socketTemplate) + edgeInterpreterProfile(installation.Interpreter) + edgePreferenceProfile(c.Preferences)
	seatbelt, err := os.ReadFile("/usr/bin/sandbox-exec")
	if err != nil {
		return refused()
	}
	bridgeIdentity := ""
	if c.Bridge != "" {
		data, e := os.ReadFile(c.Bridge)
		if e != nil {
			return refused()
		}
		sum := sha256.Sum256(data)
		bridgeIdentity = hex.EncodeToString(sum[:])
	}
	data, _ := json.Marshal([]any{edgeSeatbeltVersion, workbenchPathVersion, template, cliHome, sha256.Sum256(seatbelt), c.Bridge, bridgeIdentity, c.ToolState, o.Read, o.system})
	sum := sha256.Sum256(data)
	args := []string{"-p", profile, "--", bin}
	if installation.Interpreter != "" {
		args = []string{"-p", profile, "--", installation.Interpreter, bin}
	}
	return sandboxbridge.EdgePlan{Binary: "/usr/bin/sandbox-exec", Args: args, Env: env, Key: hex.EncodeToString(sum[:]), PathDiagnostic: workbenchPathReport(pathDrops, 2048)}, nil
}

func edgeProfile(l workbenchLayout, port int, socket string) string {
	profile := seatbeltProfile(l)
	profile += fmt.Sprintf("; Sole provider transport, owned outside this sandbox.\n(allow network-outbound (remote ip \"localhost:%d\"))\n", port)
	for _, name := range []string{".codex", ".agents", ".claude", ".mcp.json"} {
		profile += fmt.Sprintf("(deny file-write* (subpath %s))\n(deny file-link (subpath %s))\n", strconv.Quote(filepath.Join(l.Work, name)), strconv.Quote(filepath.Join(l.Work, name)))
	}
	// Credentials and configuration remain readable but cannot be planted by
	// native shell tools or descendants. No credential refresh is exported.
	for _, name := range []string{"auth.json", "config.toml", "AGENTS.md", "CLAUDE.md", ".claude.json", ".claude", ".codex", ".agents", ".mcp.json", "login.shared", "login.lock", "claude/settings.json", "claude/settings.local.json", "claude/.credentials.json", "claude/.claude.json", "claude/CLAUDE.md", "claude/.mcp.json", "claude/agents", "claude/commands", "claude/skills", "prompts", "rules", "skills"} {
		profile += fmt.Sprintf("(deny file-write* (subpath %s))\n(deny file-link (subpath %s))\n", strconv.Quote(filepath.Join(l.Home, name)), strconv.Quote(filepath.Join(l.Home, name)))
	}
	if socket != "" {
		profile += fmt.Sprintf("(allow network-outbound (remote unix-socket (path-literal %s)))\n", strconv.Quote(socket))
		profile += fmt.Sprintf("(allow file-read-metadata (literal %s))\n", strconv.Quote(filepath.Dir(socket)))
	}
	return profile
}

func edgeInterpreterProfile(path string) string {
	if path == "" {
		return ""
	}
	p := fmt.Sprintf("(allow file-read* (literal %s))\n(allow process-exec (literal %s))\n", strconv.Quote(path), strconv.Quote(path))
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		p += fmt.Sprintf("(allow file-read-metadata (literal %s))\n", strconv.Quote(dir))
		if dir == filepath.Dir(dir) {
			break
		}
	}
	return p
}

// edgePreferenceProfile lets the CLI read only its own preference domains
// through cfprefsd. Codex refuses to start a thread when it cannot synchronize
// its managed (MDM) configuration domain.
func edgePreferenceProfile(domains []string) string {
	if len(domains) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("; The CLI's own preference domains, read through cfprefsd.\n(allow ipc-posix-shm-read* (ipc-posix-name-prefix \"apple.cfprefs.\"))\n(allow mach-lookup (global-name \"com.apple.cfprefsd.daemon\") (global-name \"com.apple.cfprefsd.agent\") (local-name \"com.apple.cfprefsd.agent\"))\n")
	for _, domain := range domains {
		fmt.Fprintf(&b, "(allow user-preference-read (preference-domain %s))\n", strconv.Quote(domain))
	}
	return b.String()
}
