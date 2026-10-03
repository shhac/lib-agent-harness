//go:build darwin || linux

package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

func TestBwrapTrialClassifiesOnlyExitStatus(t *testing.T) {
	testenv.RequireProcessGroup(t)
	for _, tc := range []struct {
		version                string
		versionExit, trialExit int
		code                   string
	}{
		{"", 0, 0, CapabilitySandboxToolMissing},
		{"bubblewrap 0.7.0", 0, 0, CapabilitySandboxToolOutdated},
		{"garbage", 0, 0, CapabilitySandboxToolOutdated},
		{"bubblewrap 0.8.0", 1, 0, CapabilitySandboxToolOutdated},
		{"bubblewrap 0.8.0", 0, 1, CapabilitySandboxNamespacesUnavailable},
		{"bubblewrap 0.8.0", 0, 0, ""},
		{"bubblewrap 0.10.0", 0, 0, ""},
		{"bubblewrap 1.0", 0, 0, ""},
	} {
		t.Run(tc.version+tc.code, func(t *testing.T) {
			path := t.TempDir()
			t.Setenv("PATH", path)
			if tc.version != "" {
				script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '" + tc.version + "'; exit " + strconv.Itoa(tc.versionExit) + "; fi\necho SECRET-STDERR >&2\nexit " + strconv.Itoa(tc.trialExit) + "\n"
				if e := os.WriteFile(filepath.Join(path, "bwrap"), []byte(script), 0700); e != nil {
					t.Fatal(e)
				}
			}
			_, _, e := checkBwrap(context.Background(), linuxTestLayout(t))
			if tc.code == "" {
				if e != nil {
					t.Fatal(e)
				}
				return
			}
			var cap *ProofError
			if !errors.As(e, &cap) || cap.Code != tc.code || cap.HarnessFacts().Phase != "before_launch" || len(cap.Tools) != 1 || cap.Tools[0] != "bwrap" {
				t.Fatalf("%v", e)
			}
			if strings.Contains(e.Error(), "SECRET-STDERR") {
				t.Fatal("stderr leaked")
			}
		})
	}
}

func linuxTestLayout(t *testing.T) workbenchLayout {
	t.Helper()
	root := t.TempDir()
	l := workbenchLayout{Work: filepath.Join(root, "work"), Home: filepath.Join(root, "home"), Tmp: filepath.Join(root, "tmp"), System: bwrapSystemDirs()}
	for _, p := range []string{l.Work, l.Home, l.Tmp} {
		if e := os.Mkdir(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	return l
}
func TestBwrapVersion(t *testing.T) {
	for _, s := range []string{"bubblewrap 0.8.0", "bubblewrap 0.10.0\n", "bubblewrap 1.0", "bubblewrap 1.0.0"} {
		if _, ok := supportedBwrapVersion(s); !ok {
			t.Errorf("refused %s", s)
		}
	}
	for _, s := range []string{"bubblewrap 0.7.9", "garbage", "bubblewrap 0.8.0-extra", "bubblewrap -1.8", "bubblewrap 0", "bubblewrap 0.8.0 more", "bubblewrap 0.8.0.1", "bubblewrap +1.0", "bubblewrap 9999999999999999999999.0"} {
		if _, ok := supportedBwrapVersion(s); ok {
			t.Errorf("accepted %s", s)
		}
	}
}

func TestWorkbenchLinuxSocketAbsenceRequiresReadableParents(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "socket")
	check := func(path string, want bool) {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", linuxSocketAbsent(path))
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		e := cmd.Run()
		if (e == nil) != want {
			t.Fatalf("socket absence %s: %v", path, e)
		}
	}
	check(socket, true)
	check(filepath.Join(root, "absent", "socket"), true)
	if e := os.WriteFile(socket, []byte("marker"), 0600); e != nil {
		t.Fatal(e)
	}
	check(socket, false)
	if e := os.Remove(socket); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Join(root, "absent"), socket); e != nil {
		t.Fatal(e)
	}
	check(socket, false)
	locked := filepath.Join(root, "locked")
	if e := os.Mkdir(locked, 0000); e != nil {
		t.Fatal(e)
	}
	defer os.Chmod(locked, 0700)
	if os.Geteuid() != 0 {
		check(filepath.Join(locked, "socket"), false)
	}
}

func TestBwrapFingerprintDetectsSameMetadataReplacement(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "bwrap")
	if e := os.WriteFile(binary, []byte("first"), 0700); e != nil {
		t.Fatal(e)
	}
	info, e := os.Stat(binary)
	if e != nil {
		t.Fatal(e)
	}
	before, e := workbenchBinaryFingerprint(binary)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(binary, []byte("other"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Chtimes(binary, info.ModTime(), info.ModTime()); e != nil {
		t.Fatal(e)
	}
	after, e := workbenchBinaryFingerprint(binary)
	if e != nil || before == after {
		t.Fatalf("replacement reused evidence: %v", e)
	}
}

func TestBwrapMergedSystemAndMissingOverlay(t *testing.T) {
	l := linuxTestLayout(t)
	system := t.TempDir()
	usr := filepath.Join(system, "usr")
	if e := os.MkdirAll(filepath.Join(usr, "bin"), 0700); e != nil {
		t.Fatal(e)
	}
	bin := filepath.Join(system, "bin")
	if e := os.Symlink("usr/bin", bin); e != nil {
		t.Fatal(e)
	}
	l.System = []string{usr, bin, filepath.Join(system, "missing")}
	args, e := bwrapArgs(l)
	if e != nil {
		t.Fatal(e)
	}
	simulateBwrap(t, args)
	found := false
	for i, a := range args {
		if a == "--symlink" && args[i+1] == "usr/bin" && args[i+2] == bin {
			found = true
		}
	}
	if !found {
		t.Fatal("merged system symlink not recreated")
	}
	if bwrapSystemContains(filepath.Join(system, "missing"), l.Work) {
		t.Fatal("absent system entry includes everything")
	}
	l.Write = true
	if _, e = bwrapArgs(l); e == nil {
		t.Fatal("writable workspace with no metadata overlay accepted")
	}
	if e = os.Symlink(l.Home, filepath.Join(l.Work, ".git")); e != nil {
		t.Fatal(e)
	}
	if _, e = bwrapArgs(l); e == nil {
		t.Fatal("symlink metadata accepted")
	}
}

func simulateBwrap(t *testing.T, args []string) {
	t.Helper()
	tree := map[string]bool{"/": true}
	phase := 1
	lastDir := ""
	lastBind := ""
	var binds []string
	for i := 0; i < len(args); i++ {
		flag := args[i]
		switch flag {
		case "--cap-drop":
			i++
		case "--tmpfs", "--dev", "--proc":
			if phase != 1 {
				t.Fatal("covering mount after directory or bind")
			}
			i++
			tree[args[i]] = true
		case "--perms":
			i++
		case "--dir":
			if phase > 2 {
				t.Fatal("directory after bind")
			}
			phase = 2
			i++
			p := args[i]
			for _, bind := range binds {
				if lexicallyWithin(bind, p) {
					t.Fatalf("directory %s inside active bind %s", p, bind)
				}
			}
			if !tree[filepath.Dir(p)] {
				t.Fatalf("missing parent of %s", p)
			}
			if lastDir != "" {
				paths := []string{lastDir, p}
				shallowPaths(paths)
				if paths[0] != lastDir {
					t.Fatal("directories out of order")
				}
			}
			lastDir = p
			tree[p] = true
		case "--ro-bind", "--bind", "--symlink":
			phase = 3
			source, dest := args[i+1], args[i+2]
			i += 2
			if !tree[filepath.Dir(dest)] {
				t.Fatalf("missing parent of bind %s", dest)
			}
			info, e := os.Lstat(source)
			directory := flag != "--symlink" && (e != nil || info.IsDir())
			if directory && !tree[dest] {
				t.Fatalf("missing bind destination %s", dest)
			}
			if flag == "--symlink" {
				tree[dest] = true
			}
			if strings.HasSuffix(dest, "/.git") && i != len(args)-1 {
				t.Fatal("git overlay is not last")
			}
			lastBind = dest
			binds = append(binds, dest)
		default:
			if !strings.HasPrefix(flag, "--") {
				t.Fatalf("unexpected value %s", flag)
			}
		}
	}
	if lastBind == "" {
		t.Fatal("no binds")
	}
	for _, flag := range []string{"--unshare-user", "--disable-userns", "--cap-drop", "ALL", "--unshare-net", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup", "--die-with-parent", "--new-session"} {
		found := false
		for _, a := range args {
			found = found || a == flag
		}
		if !found {
			t.Errorf("missing %s", flag)
		}
	}
}

func TestBwrapArgsPhases(t *testing.T) {
	for _, tc := range []struct {
		work, home, tmp string
		read            []string
	}{
		{"home/owner/work", "home/owner/app/sessions/id/workbench/home", "home/owner/app/sessions/id/workbench/tmp", nil},
		{"home/owner/work", "var/tmp/app/sessions/id/workbench/home", "var/tmp/app/sessions/id/workbench/tmp", nil},
		{"var/tmp/work", "home/owner/app/sessions/id/workbench/home", "home/owner/app/sessions/id/workbench/tmp", nil},
		{"tmp/work", "var/tmp/app/sessions/id/workbench/home", "var/tmp/app/sessions/id/workbench/tmp", nil},
		{"tmp/work", "tmp/app/home", "tmp/app/tmp", []string{"tmp/work/read"}},
		{"tmp/read/work", "var/tmp/app/home", "var/tmp/app/tmp", []string{"tmp/read"}},
		{"var/tmp/work", "var/tmp/app/home", "var/tmp/app/tmp", []string{"/usr/bin"}},
		{"home/owner/work", "var/tmp/app/home", "var/tmp/app/tmp", []string{"tmp/read", "var/tmp/toolchain"}},
	} {
		t.Run(tc.work+tc.home, func(t *testing.T) {
			root := t.TempDir()
			if e := os.MkdirAll(filepath.Join(root, "home", "owner"), 0700); e != nil {
				t.Fatal(e)
			}
			fixture := func(p string) string {
				if p == "/usr/bin" {
					return p
				}
				return filepath.Join(root, p)
			}
			tc.work, tc.home, tc.tmp = fixture(tc.work), fixture(tc.home), fixture(tc.tmp)
			for i, p := range tc.read {
				tc.read[i] = fixture(p)
			}
			for _, p := range append([]string{tc.work, tc.home, tc.tmp, filepath.Join(tc.work, ".git")}, tc.read...) {
				if p != "/usr/bin" {
					if e := os.MkdirAll(p, 0700); e != nil {
						t.Fatal(e)
					}
				}
			}
			if len(tc.read) == 2 {
				alias := filepath.Join(root, "read-alias")
				if e := os.Symlink(tc.read[0], alias); e != nil {
					t.Fatal(e)
				}
				resolved, e := filepath.EvalSymlinks(alias)
				if e != nil {
					t.Fatal(e)
				}
				tc.read[0] = resolved // Normalization supplies resolved Read paths.
			}
			args, e := bwrapArgs(workbenchLayout{Work: tc.work, Home: tc.home, Tmp: tc.tmp, Read: tc.read, System: bwrapSystemDirs(), Write: true})
			if e != nil {
				t.Fatal(e)
			}
			simulateBwrap(t, args)
			if args[len(args)-1] != filepath.Join(tc.work, ".git") {
				t.Fatal("missing fixture overlay")
			}
			for i, a := range args {
				if a == "--ro-bind" && args[i+1] == "/usr/bin" {
					t.Fatal("redundant system bind")
				}
			}
		})
	}
	l := linuxTestLayout(t)
	if e := os.Mkdir(filepath.Join(l.Work, ".git"), 0700); e != nil {
		t.Fatal(e)
	}
	args, e := bwrapArgs(l)
	if e != nil {
		t.Fatal(e)
	}
	simulateBwrap(t, args)
	if args[len(args)-1] != filepath.Join(l.Work, ".git") {
		t.Fatal("missing overlay")
	}
	for _, p := range []string{"/usr/work", "/bin/work", "/lib64/work", "/etc/work"} {
		l.Work = p
		if _, e := bwrapArgs(l); e == nil {
			t.Fatalf("accepted %s", p)
		}
	}
}

func TestLinuxCommandSocketReadPaths(t *testing.T) {
	for _, path := range []string{"/", "/run", "/run/user/1000", "/tmp"} {
		if !linuxCommandSocketRead(path) {
			t.Errorf("accepted %s", path)
		}
	}
	for _, path := range []string{"/run-other", "/tmp/project", "/workspace", "/usr/bin"} {
		if linuxCommandSocketRead(path) {
			t.Errorf("refused %s", path)
		}
	}
}

func TestWorkbenchLinuxStatusProtocol(t *testing.T) {
	for _, tc := range []struct {
		data             string
		started, settled bool
		code             int
	}{
		{`{"child-pid":123}` + "\n" + `{"exit-code":7}`, true, true, 7},
		{`{"child-pid":123}`, true, false, -1}, {`{"exit-code":0}`, false, false, -1}, {"garbage", false, false, -1},
	} {
		started, code, settled := readBwrapStatus(strings.NewReader(tc.data), nil)
		if started != tc.started || settled != tc.settled || code != tc.code {
			t.Fatalf("%s: %t %d %t", tc.data, started, code, settled)
		}
	}
}

func TestWorkbenchLinuxCanaryJudge(t *testing.T) {
	good := "inside\nnested\ntmp\ntmpdir\nsystem\nreadset\nprivilege-ok\nwitness-network-connect\nwitness-localhost-connect\nwitness-socket-structural\nsocket-structural-ok\ncanary-ran\n"
	if e := judgeLinuxWorkbench(good, true, false, false, false); e != nil {
		t.Fatal(e)
	}
	for _, label := range []string{"sibling", "gitdir", "gitmove", "gitlink", "githardlink", "home", "outside", "runtime", "outside-etc", "readset-write", "link", "socket", "network", "localhost", "privilege", "overlay"} {
		var cap *ProofError
		e := judgeLinuxWorkbench(label+"\n"+good, true, false, false, false)
		if !errors.As(e, &cap) || cap.Code != CapabilitySandboxNotEnforced {
			t.Fatalf("%s %v", label, e)
		}
	}
	for _, label := range []string{"privilege-ok", "socket-structural-ok", "canary-ran", "readset"} {
		if e := judgeLinuxWorkbench(strings.ReplaceAll(good, label+"\n", ""), true, false, false, false); e == nil {
			t.Fatalf("missing %s accepted", label)
		}
	}
	if judgeLinuxWorkbench(good, true, true, false, false) == nil || judgeLinuxWorkbench(good, false, false, false, false) == nil || judgeLinuxWorkbench(good, true, false, true, false) == nil {
		t.Fatal("observations ignored")
	}
}
