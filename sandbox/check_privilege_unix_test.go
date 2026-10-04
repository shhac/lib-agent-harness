//go:build darwin || linux

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedLinuxPrivilegeExpression(t *testing.T) {
	// Extract the actual generated command rather than copy the awk predicate.
	root := t.TempDir()
	script := linuxWorkbenchCanary(workbenchLayout{Work: root, Home: root, Tmp: root}, root, root, filepath.Join(root, "socket"), "fixture-netns", workbenchLinuxWitness{}, "", "")
	var expression string
	for _, line := range strings.Split(script, "\n") {
		if strings.Contains(line, "/proc/self/status && echo privilege-ok") {
			if expression != "" {
				t.Fatal("multiple privilege expressions")
			}
			expression = line
		}
	}
	if expression == "" {
		t.Fatal("generated privilege expression missing")
	}
	status := "CapEff: 0000\nCapPrm: 0000\nCapInh: 0000\nCapAmb: 0000\nNoNewPrivs: 1\n"
	cases := map[string]string{"confined": status}
	for _, field := range []string{"CapEff", "CapPrm", "CapInh", "CapAmb", "NoNewPrivs"} {
		old, next := field+": 0000", field+": 0001"
		if field == "NoNewPrivs" {
			old, next = field+": 1", field+": 0"
		}
		cases[field] = strings.ReplaceAll(status, old, next)
	}
	cases["missing"] = strings.ReplaceAll(status, "CapEff: 0000\n", "")
	cases["duplicate"] = status + "CapEff: 0000\n"
	cases["malformed"] = strings.ReplaceAll(status, "CapEff: 0000", "CapEff: invalid")
	cases["invalid-no-new-privs"] = strings.ReplaceAll(status, "NoNewPrivs: 1", "NoNewPrivs: 2")
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "status")
			if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			command := strings.ReplaceAll(expression, "/proc/self/status", workbenchShellQuote(file))
			out, err := exec.Command("/bin/sh", "-c", command).CombinedOutput()
			want := "privilege"
			if name == "confined" {
				want = "privilege-ok"
			}
			if err != nil || strings.TrimSpace(string(out)) != want {
				t.Fatalf("generated expression: %q %v, want %s", out, err, want)
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		// A missing file cannot be read even by root, unlike chmod(000).
		missing := filepath.Join(t.TempDir(), "absent-status")
		command := strings.ReplaceAll(expression, "/proc/self/status", workbenchShellQuote(missing))
		out, err := exec.Command("/bin/sh", "-c", command).Output()
		if err != nil || strings.TrimSpace(string(out)) != "privilege" {
			t.Fatalf("unreadable expression: %q %v", out, err)
		}
	})
}
