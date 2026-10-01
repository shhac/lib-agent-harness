package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A versioned template is shared by commands and the disposable canary.
const workbenchSeatbeltVersion = "seatbelt-workbench-v3"

type workbenchLayout struct {
	Work, Home, Tmp string
	Read, System    []string
	Write, Loopback bool
}

func seatbeltProfile(l workbenchLayout) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("; Shells and their children execute within this sandbox.\n(allow process-exec)\n(allow process-fork)\n(allow signal (target same-sandbox))\n")
	b.WriteString("; Hardware and OS version queries needed by runtimes, excluding process arguments.\n(allow sysctl-read (sysctl-name-regex #\"^(hw[.]|kern[.]os|kern[.]max|machdep[.]cpu[.])\") (sysctl-name \"kern.argmax\"))\n; Minimal command-line runtime services, excluding keychains.\n(allow mach-lookup (global-name \"com.apple.system.logger\") (global-name \"com.apple.system.notification_center\"))\n")
	for _, p := range []string{"/private", "/private/etc", "/etc", "/dev"} {
		fmt.Fprintf(&b, "; Resolve public runtime configuration without directory listings.\n(allow file-read-metadata (literal %s))\n", strconv.Quote(p))
	}
	paths := append([]string{}, l.System...)
	paths = append(paths, l.Read...)
	paths = append(paths, l.Work, l.Home, l.Tmp)
	for _, p := range paths {
		b.WriteString("; Read the pinned runtime, explicit caller read set, workspace or private scratch.\n")
		if p == "/System" {
			// The APFS data volume exposes owner homes and private transcripts
			// below /System. Only explicit workspace/read/scratch grants may
			// authorize data-volume files, through either spelling.
			b.WriteString("(allow file-read* (require-all (subpath \"/System\") (require-not (subpath \"/System/Volumes/Data\"))))\n")
		} else {
			fmt.Fprintf(&b, "(allow file-read* (subpath %s))\n", strconv.Quote(p))
		}
		for parent := filepath.Dir(p); ; parent = filepath.Dir(parent) {
			fmt.Fprintf(&b, "(allow file-read-metadata (literal %s))\n", strconv.Quote(parent))
			if parent == filepath.Dir(parent) {
				break
			}
		}
	}
	for _, name := range []string{"passwd", "group", "nsswitch.conf", "hosts", "localtime", "ld.so.cache", "ld.so.conf", "ld.so.conf.d", "ssl/certs", "ca-certificates", "alternatives", "os-release"} {
		fmt.Fprintf(&b, "; Public runtime configuration.\n(allow file-read* (subpath %s))\n", strconv.Quote("/private/etc/"+name))
	}
	for _, p := range []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/fd"} {
		fmt.Fprintf(&b, "; Minimal device runtime.\n(allow file-read* (subpath %s))\n", strconv.Quote(p))
	}
	b.WriteString("; Discard output without exposing other devices.\n(allow file-write* (literal \"/dev/null\"))\n")
	writes := []string{l.Home, l.Tmp}
	if l.Write {
		writes = append(writes, l.Work)
	}
	for _, p := range writes {
		fmt.Fprintf(&b, "; Only private scratch and the opted-in workspace are writable.\n(allow file-write* (subpath %s))\n", strconv.Quote(p))
	}
	b.WriteString("; Repository metadata cannot be written, unlinked, moved or linked, including case aliases.\n(deny file-write* (regex #\"(^|/)[.][gG][iI][tT][ .]*(/|$)\"))\n(deny file-link (regex #\"(^|/)[.][gG][iI][tT][ .]*(/|$)\"))\n")
	b.WriteString("; Background commands cannot inspect or disturb atomic file-tool temporaries.\n(deny file-read-data (regex #\"(^|/)[.]harness-workbench-[0-9a-f]{32}-[0-9a-f]{16}[.]tmp$\"))\n(deny file-write* (regex #\"(^|/)[.]harness-workbench-[0-9a-f]{32}-[0-9a-f]{16}[.]tmp$\"))\n(deny file-link (regex #\"(^|/)[.]harness-workbench-[0-9a-f]{32}-[0-9a-f]{16}[.]tmp$\"))\n")
	if l.Loopback {
		b.WriteString("; Local development servers only, never Unix-domain or off-machine sockets.\n(allow network-bind (local ip \"localhost:*\"))\n(allow network-inbound (local ip \"localhost:*\"))\n(allow network-outbound (remote ip \"localhost:*\"))\n")
	}
	return b.String()
}

func workbenchSystem(ctx context.Context) ([]string, error) {
	dirs := workbenchSystemDirs()
	cmd := exec.CommandContext(ctx, "/usr/bin/xcode-select", "-p")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	xcode, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, err
	}
	dirs = append(dirs, xcode)
	return dirs, nil
}

func normalizeWorkbenchSystem(o Options) (Options, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout)
	defer cancel()
	system, err := workbenchSystem(ctx)
	if err != nil {
		return o, workbenchCapability(CapabilitySandboxUnavailable)
	}
	for _, dir := range system {
		if workbenchSystemContains(dir, o.RuntimeHome) {
			return o, refuse(o, "runtime_home", RefusedRuntimeHome, "runtime home must be outside the command system read set")
		}
	}
	o.Workbench.system = system
	return o, nil
}

func workbenchSandboxArgs(l workbenchLayout, command string) []string {
	return []string{"-p", seatbeltProfile(l), "/bin/sh", "-c", command}
}

func workbenchProbeKey(o Options, system []string) (string, error) {
	info, err := os.Stat("/usr/bin/sandbox-exec")
	if err != nil {
		return "", err
	}
	binary, err := os.ReadFile("/usr/bin/sandbox-exec")
	if err != nil {
		return "", err
	}
	binaryHash := sha256.Sum256(binary)
	templateHash := sha256.Sum256([]byte(seatbeltProfile(workbenchLayout{Work: "/workspace", Home: "/home", Tmp: "/tmp", Read: []string{"/read"}, System: workbenchSystemDirs(), Write: true, Loopback: true})))
	payload, _ := json.Marshal(struct {
		Kind, Template, Work, Runtime, Binary string
		Size                                  int64
		Modified                              time.Time
		Write, Loopback                       bool
		Read, System, Env                     []string
		Background                            bool
	}{"workbench", workbenchSeatbeltVersion + ":" + hex.EncodeToString(templateHash[:]), o.WorkDir, o.RuntimeHome, hex.EncodeToString(binaryHash[:]), info.Size(), info.ModTime(), o.Workbench.Write, o.Workbench.Commands.Loopback, o.Workbench.Commands.Read, system, o.Workbench.Commands.Env, o.Background})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
