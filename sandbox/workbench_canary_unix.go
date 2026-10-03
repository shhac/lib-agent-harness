//go:build darwin || linux

package sandbox

import (
	"path/filepath"
	"strings"
)

type workbenchLinuxWitness struct{ Network, Localhost, Socket string }

func workbenchCanaryLines(output string) (map[string]bool, string) {
	lines := map[string]bool{}
	completed := strings.TrimSpace(output)
	last := completed
	if i := strings.LastIndexByte(completed, '\n'); i >= 0 {
		last = strings.TrimSpace(completed[i+1:])
	}
	for _, line := range strings.Split(output, "\n") {
		lines[strings.TrimSpace(line)] = true
	}
	return lines, last
}

func workbenchCanaryEscaped(lines map[string]bool, extra ...string) bool {
	for _, escape := range []string{"sibling", "gitdir", "gitmove", "gitlink", "githardlink", "home", "outside", "runtime", "readset-write", "link", "socket", "network", "localhost"} {
		if lines[escape] {
			return true
		}
	}
	for _, escape := range extra {
		if lines[escape] {
			return true
		}
	}
	return false
}

func linuxStructuralWitness(namespace, socket string) string {
	q := workbenchShellQuote
	// Every unreadable or failed component is a failure, never partial proof.
	return "n=$(readlink /proc/self/ns/net) && [ -n \"$n\" ] && [ \"$n\" != " + q(namespace) + " ] && awk 'BEGIN { n=0; bad=0 } NR>2 { if ($1 != \"lo:\") bad=1; n++ } END { exit (bad || n != 1 || NR < 3) }' /proc/net/dev && ( " + linuxSocketAbsent(socket) + " )"
}

// test -e alone confuses ENOENT with EACCES. Inspect the path top-down:
// absence counts only after a readable, searchable parent has been proved.
func linuxSocketAbsent(socket string) string {
	q := workbenchShellQuote
	check := "[ ! -e " + q(socket) + " ] && [ ! -L " + q(socket) + " ]"
	for p := filepath.Dir(socket); p != "/"; p = filepath.Dir(p) {
		name := q(p)
		check = "if [ -e " + name + " ] || [ -L " + name + " ]; then [ -d " + name + " ] && [ -r " + name + " ] && [ -x " + name + " ] && ( " + check + " ); else true; fi"
	}
	return "[ -r / ] && [ -x / ] && ( " + check + " )"
}

func linuxWorkbenchCanary(l workbenchLayout, hidden, read, socket, namespace string, w workbenchLinuxWitness, network, local string, runtimeMarker ...string) string {
	q := workbenchShellQuote
	s := "try() { name=$1; shift; if ( \"$@\" ) >/dev/null 2>&1; then echo \"$name\"; fi; }\n"
	try := func(label, cmd string) { s += "try " + label + " /bin/sh -c " + q(cmd) + "\n" }
	try("inside", "echo x > inside")
	try("nested", "mkdir -p deep/nested && echo x > deep/nested/file")
	try("tmp", "echo x > "+q(filepath.Join(l.Tmp, "tmp-write")))
	try("tmpdir", "echo x > \"$TMPDIR/tmpdir-write\"")
	try("sibling", "echo x > "+q(filepath.Join(hidden, "sibling-write")))
	try("gitdir", "echo x >> .git/config")
	try("gitmove", "mv .git git-moved && echo x >> git-moved/config; result=$?; if [ -d git-moved ]; then mv git-moved .git; fi; exit $result")
	try("gitlink", "ln -s .git git-link && echo x >> git-link/config")
	try("githardlink", "ln .git/config git-hard && echo x >> git-hard")
	for _, label := range []string{"home", "outside"} {
		try(label, "cat "+q(filepath.Join(hidden, label)))
	}
	runtimePath := filepath.Join(hidden, "runtime")
	if len(runtimeMarker) > 0 {
		runtimePath = runtimeMarker[0]
	}
	try("runtime", "cat "+q(runtimePath))
	try("outside-etc", "test -e /etc/shadow")
	try("system", "/bin/sh -c true && ls /usr/bin && cat /etc/passwd")
	try("readset", "cat "+q(filepath.Join(read, "marker")))
	try("readset-write", "echo x > "+q(filepath.Join(read, "write")))
	try("link", "ln "+q(filepath.Join(hidden, "home"))+" secret-link")
	try("completion-fd-leaked", "printf forged >&3")
	s += "awk 'BEGIN { n=0; bad=0 } /^Cap(Eff|Prm|Inh|Amb):/ { n++; if ($2 !~ /^0+$/) bad=1 } /^NoNewPrivs:/ { n++; if ($2 != 1) bad=1 } END { exit (bad || n != 5) }' /proc/self/status && echo privilege-ok || echo privilege\n"
	structural := linuxStructuralWitness(namespace, socket)
	for _, entry := range []struct{ label, witness, command string }{{"socket", w.Socket, "/usr/bin/nc -U -w 2 " + q(socket) + " </dev/null"}, {"network", w.Network, network}, {"localhost", w.Localhost, local}} {
		s += "echo witness-" + entry.label + "-" + entry.witness + "\n"
		if entry.witness == "connect" {
			try(entry.label, entry.command)
		} else {
			s += "( " + structural + " ) && echo " + entry.label + "-structural-ok || echo " + entry.label + "\n"
		}
	}
	// Mount attacks are attempted individually and followed by a write. The raw
	// syscall CI helper supplies coverage on hosts without util-linux.
	for _, attack := range []struct{ tool, command string }{{"umount", "umount .git"}, {"umount", "umount -l .git"}, {"mount", "mount -o remount,rw,bind .git"}, {"mount", "mkdir -p /tmp/x && mount --bind . /tmp/x"}} {
		destination := ".git/config"
		if strings.Contains(attack.command, "/tmp/x") {
			destination = "/tmp/x/.git/config"
		}
		s += "if command -v " + attack.tool + " >/dev/null; then\n"
		try("overlay", attack.command+"; attack_status=$?; echo x >> "+destination+"; write_status=$?; [ \"$attack_status\" = 0 ] || [ \"$write_status\" = 0 ]")
		s += "if command -v unshare >/dev/null; then\n"
		try("overlay", "unshare -Urm /bin/sh -c "+q(attack.command+"; echo x >> "+destination+"; exit 0"))
		s += "else echo overlay-unshare-not-attempted; fi\nelse echo overlay-" + attack.tool + "-not-attempted; fi\n"
	}
	if l.Loopback {
		s += "nc -l 127.0.0.1 35791 >/dev/null 2>&1 &\nown_listener=$!\nsleep 1\nnc -z -w 2 127.0.0.1 35791 >/dev/null 2>&1 && echo own-loopback\nkill \"$own_listener\" >/dev/null 2>&1; wait \"$own_listener\" 2>/dev/null\n"
	}
	return s
}

func judgeLinuxWorkbench(output string, write, reached, background bool, loopback bool) error {
	lines, last := workbenchCanaryLines(output)
	if workbenchCanaryEscaped(lines, "outside-etc", "privilege", "overlay", "completion-fd-leaked") {
		return workbenchCapability(CapabilitySandboxNotEnforced)
	}
	if reached || (!write && (lines["inside"] || lines["nested"])) {
		return workbenchCapability(CapabilitySandboxNotEnforced)
	}
	required := []string{"tmp", "tmpdir", "system", "readset", "privilege-ok"}
	if loopback {
		required = append(required, "own-loopback")
	}
	if write {
		required = append(required, "inside", "nested")
	}
	if background {
		required = append(required, "background")
	}
	for _, label := range []string{"network", "localhost", "socket"} {
		if lines["witness-"+label+"-structural"] {
			required = append(required, label+"-structural-ok")
		} else if !lines["witness-"+label+"-connect"] {
			return workbenchCapability(CapabilitySandboxUnavailable)
		}
	}
	for _, label := range required {
		if !lines[label] {
			return workbenchCapability(CapabilitySandboxUnavailable)
		}
	}
	if last != canaryRan || !lines[canaryRan] {
		return workbenchCapability(CapabilitySandboxUnavailable)
	}
	return nil
}
