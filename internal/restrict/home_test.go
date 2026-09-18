package restrict

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

// A home with no configuration, or with settings the launch overrides anyway,
// is usable. Refusing those would make the check useless in practice.
func TestInspectCodexHomeAcceptsOrdinarySettings(t *testing.T) {
	if err := InspectCodexHome(t.TempDir()); err != nil {
		t.Errorf("a home with no config.toml was refused: %v", err)
	}
	if err := InspectCodexHome(""); err != nil {
		t.Errorf("an unset home was refused: %v", err)
	}
	ordinary := "model = \"gpt-5.6\"\napproval_policy = \"never\"\n\n[shell_environment_policy]\ninherit = \"core\"\n\n# [mcp_servers.commented]\n"
	if err := InspectCodexHome(writeConfig(t, ordinary)); err != nil {
		t.Errorf("ordinary settings were refused: %v", err)
	}
}

// An inherited MCP server really does start — verified against the installed
// CLI — and no override clears it, so a home that declares one is refused.
func TestInspectCodexHomeRefusesToolBearingConfiguration(t *testing.T) {
	for name, body := range map[string]string{
		"table header":  "[mcp_servers.stray]\ncommand = \"/bin/sh\"\n",
		"array table":   "[[hooks.pre]]\ncommand = \"x\"\n",
		"dotted assign": "mcp_servers.stray.command = \"/bin/sh\"\n",
		"quoted key":    "[\"mcp_servers\".stray]\ncommand = \"x\"\n",
		"plugins":       "[plugins]\nenabled = true\n",
		"skills":        "[skills.local]\npath = \"/tmp\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			err := InspectCodexHome(writeConfig(t, body))
			var inherited *InheritedConfig
			if !errors.As(err, &inherited) {
				t.Fatalf("tool-bearing configuration was accepted: %v", err)
			}
			if inherited.Key == "" {
				t.Error("refusal did not name the offending key")
			}
		})
	}
}

// Unreadable is refused, not assumed harmless.
func TestInspectCodexHomeRefusesWhatItCannotRead(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "config.toml"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := InspectCodexHome(home); err == nil {
		t.Fatal("an unreadable home was accepted")
	}
}
