package restrict

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Codex configuration overrides are TOML, not JSON. A JSON object passed to a
// `-c` override is not valid TOML — its keys are quoted and separated by colons
// — so it is rejected or, worse, misread. These helpers emit dotted-key
// overrides with TOML-encoded values, which is the same form the rest of the
// restricted settings already use.

// ErrUnencodable reports a value that cannot be represented as a TOML basic
// string. Callers supply their own paths and fixed identifiers, so this is a
// programming error rather than something to sanitize around.
var ErrUnencodable = errors.New("value cannot be encoded as TOML")

// TOMLString encodes a basic string: quotes, backslashes and control characters
// escaped, everything else literal.
func TOMLString(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", ErrUnencodable
	}
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				// TOML's \u escape takes exactly four hex digits; a shorter one is
				// a parse error rather than a shorter code point.
				fmt.Fprintf(&out, `\u%04X`, r)
				continue
			}
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.String(), nil
}

// TOMLStringArray encodes an array of basic strings.
func TOMLStringArray(values []string) (string, error) {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		encoded, err := TOMLString(value)
		if err != nil {
			return "", err
		}
		parts = append(parts, encoded)
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}

// bareKey reports whether a key needs no quoting in a dotted TOML path.
func bareKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// CodexMCPServer returns the `-c` overrides that register one stdio MCP server.
// Each leaf is its own dotted-key override rather than one nested inline table,
// so quoting stays simple and a malformed nested value cannot silently change a
// neighbouring field. Field names are Codex's: command, args, env.
func CodexMCPServer(name, command string, args []string, env map[string]string) ([]string, error) {
	if !bareKey(name) {
		return nil, ErrUnencodable
	}
	prefix := "mcp_servers." + name + "."
	encodedCommand, err := TOMLString(command)
	if err != nil {
		return nil, err
	}
	out := []string{prefix + "command=" + encodedCommand}
	encodedArgs, err := TOMLStringArray(args)
	if err != nil {
		return nil, err
	}
	out = append(out, prefix+"args="+encodedArgs)
	keys := make([]string, 0, len(env))
	for key := range env {
		if !bareKey(key) {
			return nil, ErrUnencodable
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, err := TOMLString(env[key])
		if err != nil {
			return nil, err
		}
		out = append(out, prefix+"env."+key+"="+value)
	}
	return out, nil
}
