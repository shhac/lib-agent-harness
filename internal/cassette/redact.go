package cassette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Redactions replace the value of a JSON key, at any depth, wherever a CLI
// writes it, with raw JSON of the same type, so a parser reads the stand-in as
// it would the real value: {"team": `"example-team"`, "servers": `[]`}. A
// value of another type is kept, since a key one document holds private can
// name a measurement in another. The rest of the frame keeps its bytes. A
// redaction is one-way; a replay plays the stand-in.
type Redactions map[string]string

func (r Redactions) apply(raw []byte) []byte {
	if len(r) == 0 {
		return raw
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil {
		return raw
	}
	delim, ok := tok.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return raw
	}
	var out []byte
	last := 0
	for d.More() {
		key := ""
		if delim == '{' {
			k, err := d.Token()
			if err != nil {
				return raw
			}
			key, _ = k.(string)
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return raw
		}
		end := int(d.InputOffset())
		start := end - len(v)
		replaced := r.apply(v)
		if standIn, hit := r[key]; hit && delim == '{' && jsonKind([]byte(standIn)) == jsonKind(v) {
			replaced = []byte(standIn)
		}
		out = append(append(out, raw[last:start]...), replaced...)
		last = end
	}
	return append(out, raw[last:]...)
}

// jsonKind is the type of the JSON value raw holds, by its first byte.
func jsonKind(raw []byte) byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0
	}
	switch c := raw[0]; c {
	case '"', '{', '[', 'n':
		return c
	case 't', 'f':
		return 't'
	}
	return '0'
}

// leakPatterns are the shapes no recording may hold, by what each is.
var leakPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"a JWT", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{"an API key", regexp.MustCompile(`\b(sk|pk|rk|xai|ghp|gho|github_pat)[-_][A-Za-z0-9_-]{16,}`)},
	{"a bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{16,}`)},
	{"an e-mail address", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`)},
}

var exampleAddress = regexp.MustCompile(`@(example\.(com|org|net)|users\.noreply\.github\.com)$`)

// Scan reports where the recordings under dir hold what may be private: a
// credential-shaped token, an e-mail address that is not an example one, or
// the user's home directory, which the recorder should have named. It names
// the kind, never the text, so the report is safe to print.
func Scan(dir string) ([]string, error) {
	home, _ := os.UserHomeDir()
	var found []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, leak := range leakPatterns {
				for _, match := range leak.re.FindAllString(line, -1) {
					if strings.Contains(match, "@") && exampleAddress.MatchString(match) {
						continue
					}
					found = append(found, fmt.Sprintf("%s:%d: %s", path, i+1, leak.kind))
				}
			}
			if home != "" && (strings.Contains(line, home) || strings.Contains(line, jsonEscape(home))) {
				found = append(found, fmt.Sprintf("%s:%d: the user's home directory", path, i+1))
			}
		}
		return nil
	})
	return found, err
}
