// Package tomltest decodes TOML basic strings for tests that need to show an
// encoded Codex override means what it was meant to. It follows the TOML 1.0
// grammar strictly and independently of the encoder, so an escape the encoder
// gets wrong is a decoding error here rather than an echo of the same mistake.
// It is imported only by tests.
package tomltest

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// BasicString decodes one complete TOML basic string, quotes included.
func BasicString(encoded string) (string, error) {
	if len(encoded) < 2 || encoded[0] != '"' || encoded[len(encoded)-1] != '"' {
		return "", errors.New("not a quoted basic string")
	}
	if !utf8.ValidString(encoded) {
		return "", errors.New("basic string is not UTF-8")
	}
	body := encoded[1 : len(encoded)-1]
	var out strings.Builder
	for i := 0; i < len(body); {
		r, size := utf8.DecodeRuneInString(body[i:])
		switch {
		case r == '"':
			return "", errors.New("unescaped quotation mark")
		case (r < 0x20 && r != '\t') || r == 0x7f:
			return "", errors.New("unescaped control character")
		case r != '\\':
			out.WriteRune(r)
			i += size
			continue
		}
		if i+1 >= len(body) {
			return "", errors.New("dangling escape")
		}
		escape := body[i+1]
		i += 2
		simple := map[byte]rune{'b': '\b', 't': '\t', 'n': '\n', 'f': '\f', 'r': '\r', '"': '"', '\\': '\\'}
		if decoded, ok := simple[escape]; ok {
			out.WriteRune(decoded)
			continue
		}
		digits := map[byte]int{'u': 4, 'U': 8}[escape]
		if digits == 0 {
			return "", errors.New("unknown escape")
		}
		if i+digits > len(body) {
			return "", errors.New("short unicode escape")
		}
		code, err := strconv.ParseUint(body[i:i+digits], 16, 32)
		if err != nil || !utf8.ValidRune(rune(code)) {
			return "", errors.New("invalid unicode escape")
		}
		out.WriteRune(rune(code))
		i += digits
	}
	return out.String(), nil
}

// Override splits a `key=value` override and decodes its basic-string value.
func Override(override string) (string, string, error) {
	key, value, found := strings.Cut(override, "=")
	if !found {
		return "", "", errors.New("override has no value")
	}
	decoded, err := BasicString(value)
	return key, decoded, err
}
