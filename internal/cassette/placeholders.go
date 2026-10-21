package cassette

import (
	"path/filepath"
	"slices"
	"strings"
)

// placeholders swaps per-run values, such as a test's temporary directory,
// for stable names, so a recording made in one directory replays in another.
// A recording holds @@NAME@@ where a run held the value.
type placeholders map[string]string

type substitution struct{ value, name string }

// forms are every spelling of each value a line might hold, longest first so
// a directory inside another is named for itself: as written, with symlinks
// resolved (macOS reports /var as /private/var), and escaped inside a JSON
// string (Windows paths double their backslashes).
func (p placeholders) forms() []substitution {
	var subs []substitution
	for name, value := range p {
		if value == "" {
			continue
		}
		values := []string{value}
		if resolved, err := filepath.EvalSymlinks(value); err == nil && resolved != value {
			values = append(values, resolved)
		}
		for _, v := range values {
			subs = append(subs, substitution{v, "@@" + name + "@@"})
			if escaped := jsonEscape(v); escaped != v {
				subs = append(subs, substitution{escaped, "@@" + name + "@@"})
			}
		}
	}
	slices.SortFunc(subs, func(a, b substitution) int {
		if d := len(b.value) - len(a.value); d != 0 {
			return d
		}
		if d := strings.Compare(a.value, b.value); d != 0 {
			return d
		}
		return strings.Compare(a.name, b.name)
	})
	return slices.CompactFunc(subs, func(a, b substitution) bool { return a.value == b.value })
}

// holds reports whether a value already has a name, in any spelling.
func (p placeholders) holds(value string) bool {
	return slices.ContainsFunc(placeholders{"": value}.forms(), func(s substitution) bool {
		return slices.ContainsFunc(p.forms(), func(t substitution) bool { return t.value == s.value })
	})
}

// name replaces every value with its placeholder.
func (p placeholders) name(s string) string { return nameWith(p.forms(), s) }

func nameWith(subs []substitution, s string) string {
	for _, sub := range subs {
		s = strings.ReplaceAll(s, sub.value, sub.name)
	}
	return s
}

// fill replaces every placeholder with its value, escaped for a JSON string
// when the line is JSON.
func (p placeholders) fill(s string, inJSON bool) string {
	for name, value := range p {
		if inJSON {
			value = jsonEscape(value)
		}
		s = strings.ReplaceAll(s, "@@"+name+"@@", value)
	}
	return s
}

func jsonEscape(s string) string {
	b, _ := encodeLine(s)
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(string(b), "\n"), `"`), `"`)
}
