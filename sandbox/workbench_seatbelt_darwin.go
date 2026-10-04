package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A versioned template is shared by commands and the disposable canary.
const workbenchSeatbeltVersion = "seatbelt-workbench-v12"

func seatbeltProfile(l workbenchLayout) string {
	var b strings.Builder
	// Data-read and execution permissions always share the same selectors.
	allowReadable := func(selector string) {
		fmt.Fprintf(&b, "(allow file-read* %s)\n(allow process-exec %s)\n", selector, selector)
	}
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("; Shells and their children execute within this sandbox.\n(allow process-fork)\n(allow signal (target same-sandbox))\n")
	b.WriteString("; Hardware and OS version queries needed by runtimes, excluding process arguments.\n(allow sysctl-read (sysctl-name-regex #\"^(hw[.]|kern[.]os|kern[.]max|machdep[.]cpu[.])\") (sysctl-name \"kern.argmax\"))\n; uname(3), which Node reads as it loads its os module.\n(allow sysctl-read (sysctl-name \"kern.version\") (sysctl-name \"kern.hostname\"))\n; Minimal command-line runtime services, excluding keychains.\n(allow mach-lookup (global-name \"com.apple.system.logger\") (global-name \"com.apple.system.notification_center\"))\n")
	for _, p := range []string{"/private", "/private/etc", "/private/var", "/etc", "/var", "/dev"} {
		fmt.Fprintf(&b, "; Resolve public runtime configuration without directory listings.\n(allow file-read-metadata (literal %s))\n", strconv.Quote(p))
	}
	// macOS 27 aborts a shell that cannot read the root directory's own
	// entries, which name only the top-level folders; nothing below them is
	// listed. /bin/sh reads which shell it runs from /private/var/select.
	b.WriteString("; The root directory's own entries, which name only top-level folders: shells need them to start.\n(allow file-read-data (literal \"/\"))\n")
	b.WriteString("; Which shell /bin/sh runs.\n")
	allowReadable("(subpath \"/private/var/select\")")
	paths := l.readPaths()
	for _, p := range paths {
		b.WriteString("; Read the pinned runtime, explicit caller read set, workspace or private scratch.\n")
		if p == "/System" {
			// The APFS data volume exposes owner homes and private transcripts
			// below /System. Only explicit workspace/read/scratch grants may
			// authorize data-volume files, through either spelling.
			allowReadable("(require-all (subpath \"/System\") (require-not (subpath \"/System/Volumes/Data\")))")
		} else {
			allowReadable(fmt.Sprintf("(subpath %s)", strconv.Quote(p)))
		}
		for parent := filepath.Dir(p); ; parent = filepath.Dir(parent) {
			fmt.Fprintf(&b, "(allow file-read-metadata (literal %s))\n", strconv.Quote(parent))
			if parent == filepath.Dir(parent) {
				break
			}
		}
	}
	for _, p := range workbenchConfigReadPaths() {
		b.WriteString("; Public runtime configuration.\n")
		allowReadable(fmt.Sprintf("(subpath %s)", strconv.Quote(p)))
	}
	for _, p := range workbenchDeviceReadPaths() {
		b.WriteString("; Minimal device runtime.\n")
		allowReadable(fmt.Sprintf("(subpath %s)", strconv.Quote(p)))
	}
	b.WriteString("; Discard output without exposing other devices.\n(allow file-write* (literal \"/dev/null\"))\n")
	writes := []string{l.Home, l.Tmp}
	if l.Write {
		writes = append(writes, l.Work)
	}
	for _, p := range writes {
		fmt.Fprintf(&b, "; Only private scratch and the opted-in workspace are writable.\n(allow file-write* (subpath %s))\n", strconv.Quote(p))
	}
	fmt.Fprintf(&b, "; Private scratch may host disposable repositories; preserve metadata elsewhere.\n(deny file-write* (require-all (regex #\"(^|/)[.][gG][iI][tT][ .]*(/|$)\") (require-not (subpath %s))))\n(deny file-link (require-all (regex #\"(^|/)[.][gG][iI][tT][ .]*(/|$)\") (require-not (subpath %s))))\n", strconv.Quote(l.Tmp), strconv.Quote(l.Tmp))
	// Seatbelt's regular expressions have no counted repetition, so each
	// hex digit of the temporary's name is spelled out.
	temporary := workbenchTemporaryPattern
	b.WriteString("; Background commands cannot inspect or disturb atomic file-tool temporaries.\n")
	for _, op := range []string{"file-read-data", "process-exec", "file-write*", "file-link"} {
		fmt.Fprintf(&b, "(deny %s (regex #\"%s\"))\n", op, temporary)
	}
	if l.Loopback {
		b.WriteString("; Binds and inbound on all local interfaces; outbound on-machine only, never Unix-domain sockets.\n(allow network-bind (local ip \"localhost:*\"))\n(allow network-inbound (local ip \"localhost:*\"))\n(allow network-outbound (remote ip \"localhost:*\"))\n")
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
	dirs = append(dirs, developerBundle(xcode))
	// Xcode's tools refuse to run until they read the owner's license acceptance.
	if _, err := os.Stat(xcodeLicense); err == nil {
		dirs = append(dirs, xcodeLicense)
	}
	return dirs, nil
}

const xcodeLicense = "/Library/Preferences/com.apple.dt.Xcode.plist"

// developerBundle widens a selected Xcode's developer folder to its app
// bundle: xcrun stats the bundle's Info.plist and Xcode's tools load
// SharedFrameworks beside Contents/Developer.
func developerBundle(dir string) string {
	for p := dir; p != filepath.Dir(p); p = filepath.Dir(p) {
		if strings.HasSuffix(p, ".app") {
			return p
		}
	}
	return dir
}

func normalizeWorkbenchSystem(o Options, standalone bool) (Options, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sandboxProbeTimeout)
	defer cancel()
	system, err := workbenchSystem(ctx)
	if err != nil {
		return o, workbenchCapability(CapabilitySandboxUnavailable)
	}
	for _, dir := range system {
		if workbenchSystemContains(dir, o.RuntimeHome) {
			return o, refusal("runtime_home", RefusedRuntimeHome, "runtime home must be outside the command system read set")
		}
	}
	o.system = system
	return o, nil
}

func workbenchSandboxArgs(l workbenchLayout, command string) []string {
	return []string{"-p", seatbeltProfile(l), "/bin/sh", "-c", command}
}

func workbenchProbeKey(o Options, system []string) (string, error) {
	key, _, err := workbenchProbeEvidence(o, system)
	return key, err
}

// Derive informational executable identity from the same read used by the key;
// it introduces no additional executable check or failure after the old proof.
func workbenchProbeEvidence(o Options, system []string) (string, string, error) {
	info, err := os.Stat("/usr/bin/sandbox-exec")
	if err != nil {
		return "", "", err
	}
	binary, err := os.ReadFile("/usr/bin/sandbox-exec")
	if err != nil {
		return "", "", err
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
	}{"workbench", workbenchSeatbeltVersion + ":" + workbenchPathVersion + ":" + hex.EncodeToString(templateHash[:]), o.WorkDir, o.RuntimeHome, hex.EncodeToString(binaryHash[:]), info.Size(), info.ModTime(), o.Write, o.Loopback, o.Read, system, o.Env, o.Background})
	sum := sha256.Sum256(payload)
	identityPayload, _ := json.Marshal([]any{"/usr/bin/sandbox-exec", info.Size(), info.ModTime(), info.Mode(), binaryHash})
	identity := sha256.Sum256(identityPayload)
	return hex.EncodeToString(sum[:]), hex.EncodeToString(identity[:]), nil
}

// These are the same public runtime grants emitted above, not a tool list.
func workbenchConfigReadPaths() []string {
	var paths []string
	for _, name := range []string{"passwd", "group", "nsswitch.conf", "hosts", "localtime", "ld.so.cache", "ld.so.conf", "ld.so.conf.d", "ssl/certs", "ca-certificates", "alternatives", "os-release"} {
		paths = append(paths, "/private/etc/"+name)
	}
	return paths
}
func workbenchDeviceReadPaths() []string {
	return []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/fd"}
}
func workbenchPublicReadPaths() []string {
	return append(append(workbenchConfigReadPaths(), workbenchDeviceReadPaths()...), "/private/var/select")
}

var workbenchTemporaryPattern = `(^|/)[.]harness-workbench-` + strings.Repeat("[0-9a-f]", 32) + "-" + strings.Repeat("[0-9a-f]", 16) + `[.]tmp$`
var workbenchTemporaryReadDenied = regexp.MustCompile(workbenchTemporaryPattern)

func workbenchReadDirectoryDenied(path string) bool {
	return workbenchTemporaryReadDenied.MatchString(path)
}
