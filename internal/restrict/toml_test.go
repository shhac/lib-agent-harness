package restrict

import (
	"errors"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness/internal/tomltest"
)

func TestTOMLStringEscapesWhatTOMLRequires(t *testing.T) {
	for value, want := range map[string]string{
		`plain`:                `"plain"`,
		`with "quotes"`:        `"with \"quotes\""`,
		`back\slash`:           `"back\\slash"`,
		"tab\there":            `"tab\there"`,
		"line\nbreak":          `"line\nbreak"`,
		"\x01":                 `"\u0001"`,
		"del\x7f":              `"del\u007F"`,
		`/Users/p/a b/bin`:     `"/Users/p/a b/bin"`,
		`C:\Users\p\bin.exe`:   `"C:\\Users\\p\\bin.exe"`,
		`unicode — em dash ok`: `"unicode — em dash ok"`,
	} {
		got, err := TOMLString(value)
		if err != nil {
			t.Fatalf("TOMLString(%q): %v", value, err)
		}
		if got != want {
			t.Errorf("TOMLString(%q) = %s, want %s", value, got, want)
		}
	}
	if _, err := TOMLString(string([]byte{0xff, 0xfe})); !errors.Is(err, ErrUnencodable) {
		t.Error("invalid UTF-8 was encoded rather than rejected")
	}
}

// What Codex reads back has to be the value that was encoded. The decoder is a
// strict reading of the TOML grammar written apart from the encoder, so a
// malformed escape fails here instead of matching its own expectation.
func TestTOMLStringParsesBackToTheSameValue(t *testing.T) {
	for _, value := range []string{
		"", "plain", `with "quotes"`, `back\slash`, "C:\\Users\\p", "tab\there", "line\nbreak\r\n",
		"\x00\x01\x08\x0b\x0c\x1b\x1f\x7f", "unicode — em dash", "astral \U0001F600", "html <b>&amp;</b>",
		"separator \u2028 \u2029", `\u0041 is not an escape here`,
	} {
		encoded, err := TOMLString(value)
		if err != nil {
			t.Fatalf("TOMLString(%q): %v", value, err)
		}
		decoded, err := tomltest.BasicString(encoded)
		if err != nil {
			t.Fatalf("TOMLString(%q) = %s, which is not a TOML basic string: %v", value, encoded, err)
		}
		if decoded != value {
			t.Errorf("TOMLString(%q) = %s, which reads back as %q", value, encoded, decoded)
		}
	}
}

func TestTOMLStringArrayEncodesEveryElement(t *testing.T) {
	got, err := TOMLStringArray([]string{"tool-bridge", `quote"d`})
	if err != nil {
		t.Fatal(err)
	}
	if got != `["tool-bridge","quote\"d"]` {
		t.Errorf("unexpected array: %s", got)
	}
	if got, err = TOMLStringArray(nil); err != nil || got != "[]" {
		t.Errorf("empty array = %q %v", got, err)
	}
}

// The overrides have to be TOML. A JSON object is not: its keys are quoted and
// its separator is a colon, which is what the previous encoding emitted.
func TestCodexMCPServerEmitsDottedTOMLOverrides(t *testing.T) {
	got, err := CodexMCPServer("workspace", "/opt/bin/agent assistant", []string{"worker", "tool-bridge"}, map[string]string{
		"AGENT_HARNESS_TOOL_SOCKET": "/tmp/ah/t.sock",
		"AGENT_HARNESS_TOOL_LOCK":   `/tmp/ah/"odd".lock`,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`mcp_servers.workspace.command="/opt/bin/agent assistant"`,
		`mcp_servers.workspace.args=["worker","tool-bridge"]`,
		`mcp_servers.workspace.env.AGENT_HARNESS_TOOL_LOCK="/tmp/ah/\"odd\".lock"`,
		`mcp_servers.workspace.env.AGENT_HARNESS_TOOL_SOCKET="/tmp/ah/t.sock"`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d overrides, want %d: %v", len(got), len(want), got)
	}
	for i, override := range got {
		if override != want[i] {
			t.Errorf("override %d = %s, want %s", i, override, want[i])
		}
		key, value, found := strings.Cut(override, "=")
		if !found || strings.Contains(key, `"`) || strings.Contains(key, ":") {
			t.Errorf("override %d is not a dotted TOML key: %s", i, override)
		}
		if strings.HasPrefix(value, "{") && strings.Contains(value, `":`) {
			t.Errorf("override %d embeds JSON rather than TOML: %s", i, override)
		}
	}
}

func TestCodexMCPServerRejectsUnencodableIdentifiers(t *testing.T) {
	if _, err := CodexMCPServer("work space", "/bin/true", nil, nil); !errors.Is(err, ErrUnencodable) {
		t.Error("a server name needing quoting was accepted as a bare key")
	}
	if _, err := CodexMCPServer("workspace", "/bin/true", nil, map[string]string{"BAD KEY": "x"}); !errors.Is(err, ErrUnencodable) {
		t.Error("an environment name needing quoting was accepted as a bare key")
	}
}
