package tomltest

import "testing"

// The oracle has to refuse what TOML refuses, or a round trip through it proves
// nothing about what Codex will accept.
func TestBasicStringFollowsTheGrammar(t *testing.T) {
	for encoded, want := range map[string]string{
		`""`:                   "",
		`"plain"`:              "plain",
		`"a\"b\\c"`:            `a"b\c`,
		`"\b\t\n\f\r"`:         "\b\t\n\f\r",
		`"\u0001\u007F\u00e9"`: "\x01\x7fé",
		`"\U0001F600"`:         "\U0001F600",
		"\"raw\ttab\"":         "raw\ttab",
	} {
		got, err := BasicString(encoded)
		if err != nil || got != want {
			t.Errorf("BasicString(%s) = %q, %v; want %q", encoded, got, err, want)
		}
	}
	for _, encoded := range []string{
		`plain`, `"unterminated`, `"a"b"`, `"\u1"`, `"\u12"`, `"\x41"`, `"\a"`, `"\v"`, `"\"`,
		`"\uD800"`, `"\U00110000"`, `"\u+041"`, "\"raw\nnewline\"", "\"raw\x7fdel\"", "\"\xff\"",
	} {
		if got, err := BasicString(encoded); err == nil {
			t.Errorf("BasicString(%q) accepted invalid TOML as %q", encoded, got)
		}
	}
}
