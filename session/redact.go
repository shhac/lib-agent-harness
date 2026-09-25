package session

// Output hygiene: what harness and tool text is reduced to before it reaches a
// caller's diagnostic record or a model.

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// sanitize prepares captured harness output for a caller's private diagnostic
// record: control sequences removed, credential-shaped runs redacted, bounded.
// It is not a classification and must not be shown as one.
func sanitize(raw []byte, limit int) string {
	var out strings.Builder
	skipEscape := false
	for _, r := range string(raw) {
		if skipEscape {
			if unicode.IsLetter(r) {
				skipEscape = false
			}
			continue
		}
		switch {
		case r == 0x1b:
			skipEscape = true
		case r == '\n' || r == '\t':
			out.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
		default:
			out.WriteRune(r)
		}
	}
	return bound(redactSecrets(strings.Join(strings.Fields(out.String()), " ")), limit)
}

// redactSecrets removes tokens that look like credentials. It is deliberately
// blunt: a long opaque run in diagnostic output is worth losing.
func redactSecrets(text string) string {
	fields := strings.Fields(text)
	for i, field := range fields {
		trimmed := strings.Trim(field, `"'.,;:()[]{}`)
		if len(trimmed) >= 20 && opaque(trimmed) {
			fields[i] = strings.Replace(field, trimmed, "[redacted]", 1)
			continue
		}
		for _, prefix := range []string{"sk-", "sk_", "Bearer", "token=", "key=", "secret="} {
			if strings.HasPrefix(trimmed, prefix) && len(trimmed) > len(prefix) {
				fields[i] = "[redacted]"
				break
			}
		}
	}
	return strings.Join(fields, " ")
}

// opaque reports a run with no word structure: mixed classes, no separators and
// no vowel-bearing shape, which is what a credential looks like and ordinary
// prose does not.
func opaque(field string) bool {
	letters, digits, other := 0, 0, 0
	for _, r := range field {
		switch {
		case unicode.IsLetter(r):
			letters++
		case unicode.IsDigit(r):
			digits++
		default:
			other++
		}
	}
	if other > 2 || letters == 0 {
		return false
	}
	return digits > 0 || (letters > 24 && strings.ToLower(field) != field)
}

// bound truncates on a rune boundary and says so. Silent truncation would let a
// tool result look complete when it is not.
func bound(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end] + "\n[truncated: " + strconv.Itoa(len(text)-end) + " further bytes omitted]"
}
