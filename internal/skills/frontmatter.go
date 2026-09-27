package skills

import (
	"encoding/json"
	"strings"
)

// frontMatter reads the YAML front matter that opens a SKILL.md. It is a
// deliberately small, strict reader of the subset skills use: top-level
// "key: value" lines, where name and description are a plain, single- or
// double-quoted scalar, a plain scalar continued on indented lines, or a
// literal or folded block scalar. Other keys are checked for shape and
// otherwise ignored, so a skill may carry fields such as license or
// allowed-tools. Anything outside that subset (a duplicate key, flow
// collections, anchors, tags, a document without a closing marker) is
// refused rather than guessed at.
func frontMatter(text string) (map[string]string, bool) {
	text = strings.TrimPrefix(text, "\xef\xbb\xbf")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	if len(lines) == 0 || lines[0] != "---" {
		return nil, false
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, false
	}
	r := reader{seen: map[string]bool{}, values: map[string]string{}}
	for _, line := range lines[1:end] {
		if !r.line(line) {
			return nil, false
		}
	}
	if !r.finish() {
		return nil, false
	}
	return r.values, true
}

type reader struct {
	seen   map[string]bool
	values map[string]string
	// key is the scalar field being read, when it may continue on further
	// lines; closed means it may not.
	key    string
	closed bool
	parts  []string
	// other is true while the current key is one this reader ignores.
	other bool
}

func (r *reader) line(line string) bool {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		return true
	case line[0] == '#':
		return true
	case line[0] == ' ' || line[0] == '\t' || line[0] == '-':
		return r.continuation(trimmed)
	}
	if !r.finish() {
		return false
	}
	key, rest, found := strings.Cut(line, ":")
	if !found || !validKey(key) || r.seen[key] || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return false
	}
	r.seen[key] = true
	value := strings.TrimSpace(rest)
	if key != "name" && key != "description" {
		r.other = true
		return true
	}
	return r.start(key, value)
}

func (r *reader) continuation(trimmed string) bool {
	switch {
	case r.other:
		return true
	case r.key == "description" && !r.closed:
		r.parts = append(r.parts, trimmed)
		return true
	}
	return false
}

func (r *reader) start(key, value string) bool {
	r.key, r.closed, r.parts = key, false, nil
	switch value {
	case "":
		return true
	case "|", "|-", "|+", ">", ">-", ">+":
		return key == "description"
	}
	switch value[0] {
	case '"':
		var decoded string
		if json.Unmarshal([]byte(value), &decoded) != nil {
			return false
		}
		r.parts, r.closed = []string{decoded}, true
		return true
	case '\'':
		decoded, ok := singleQuoted(value)
		if !ok {
			return false
		}
		r.parts, r.closed = []string{decoded}, true
		return true
	case '[', '{', '&', '*', '!', '%', '@', '`', '|', '>':
		return false
	}
	if comment := strings.Index(value, " #"); comment >= 0 {
		value = strings.TrimSpace(value[:comment])
	}
	if strings.Contains(value, ": ") || strings.HasSuffix(value, ":") {
		return false
	}
	r.parts = []string{value}
	if key == "name" {
		r.closed = true
	}
	return true
}

// finish records the scalar being read, if any.
func (r *reader) finish() bool {
	defer func() { r.key, r.closed, r.parts, r.other = "", false, nil, false }()
	if r.key == "" {
		return true
	}
	if r.key == "name" && len(r.parts) != 1 {
		return false
	}
	r.values[r.key] = strings.Join(r.parts, " ")
	return true
}

func singleQuoted(value string) (string, bool) {
	if len(value) < 2 || value[len(value)-1] != '\'' {
		return "", false
	}
	inner := value[1 : len(value)-1]
	if strings.Contains(strings.ReplaceAll(inner, "''", ""), "'") {
		return "", false
	}
	return strings.ReplaceAll(inner, "''", "'"), true
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
