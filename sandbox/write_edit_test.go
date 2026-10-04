package sandbox

import (
	"strings"
	"testing"
)

func TestApplyEdit(t *testing.T) {
	big := strings.Repeat("b", 1<<20-2) + "a"
	for name, tc := range map[string]struct {
		content, old, new string
		all               bool
		want, code        string
	}{
		"single":             {"one two", "two", "2", false, "one 2", ""},
		"missing":            {"one two", "three", "3", false, "", "match_not_found"},
		"ambiguous":          {"a a", "a", "b", false, "", "match_not_unique"},
		"all":                {"a a a", "a", "b", true, "b b b", ""},
		"all with one match": {"a b", "a", "c", true, "c b", ""},
		"delete":             {"keep drop", " drop", "", false, "keep", ""},
		"at the limit":       {big, "a", "aa", false, big + "a", ""},
		"past the limit":     {big, "a", "aaa", false, "", wbTooLarge},
		"all past the limit": {"ab", "a", strings.Repeat("x", 1<<20), true, "", wbTooLarge},
	} {
		got, code := applyEdit(tc.content, tc.old, tc.new, tc.all)
		if got != tc.want || code != tc.code {
			t.Errorf("%s: got %d bytes, code %q; want %d bytes, code %q", name, len(got), code, len(tc.want), tc.code)
		}
	}
}
