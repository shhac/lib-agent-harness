// Package cassette records what a real CLI and the library said to each other,
// and plays it back in place of the CLI.
//
// A test links its own binary in as the CLI, with a control file beside it.
// Run as that CLI, the binary reads the control file instead of running its
// tests: recording, it runs the real CLI and keeps every line in both
// directions; replaying, it answers with a recording and checks that what the
// library sends is what the real CLI was sent. The control file stands in for
// the environment, which some launches replace with an allowlist.
//
// One process launch is one exchange file, NNN.jsonl: a header, then one entry
// per line in the order the recorder saw it. Lines that parse as JSON are
// kept verbatim; others are kept as text.
package cassette

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// header is the first line of an exchange file.
type header struct {
	CLI      string   `json:"cli"`
	Version  string   `json:"version"`
	Argv     []string `json:"argv"`
	Stdin    string   `json:"stdin"` // "pipe", or "none" when the launch gave no pipe
	Recorded string   `json:"recorded"`
	GOOS     string   `json:"goos"`
}

// entry is one line of an exchange. Exactly one of its fields is set, apart
// from EOL, which marks a last line that ended without a newline.
type entry struct {
	In      json.RawMessage `json:"in,omitempty"`
	InText  *string         `json:"in_text,omitempty"`
	Out     json.RawMessage `json:"out,omitempty"`
	OutText *string         `json:"out_text,omitempty"`
	Err     *string         `json:"err,omitempty"`
	EOL     *bool           `json:"eol,omitempty"`
	EOF     bool            `json:"eof,omitempty"`
	Exit    *int            `json:"exit,omitempty"`
}

// lineEntry is the entry for one line read from the library (in) or written
// by the CLI (out), as JSON when it parses as JSON.
func lineEntry(in bool, line []byte) entry {
	text, eol := line, true
	if n := len(text); n > 0 && text[n-1] == '\n' {
		text = text[:n-1]
	} else {
		eol = false
	}
	text = bytes.TrimSuffix(text, []byte("\r"))
	var e entry
	if json.Valid(text) && len(bytes.TrimSpace(text)) > 0 {
		raw := json.RawMessage(bytes.Clone(text))
		if in {
			e.In = raw
		} else {
			e.Out = raw
		}
	} else {
		s := string(text)
		if in {
			e.InText = &s
		} else {
			e.OutText = &s
		}
	}
	if !eol {
		e.EOL = &eol
	}
	return e
}

// errEntry is a line the CLI wrote to its standard error, always as text.
func errEntry(line []byte) entry {
	text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
	e := entry{Err: &text}
	if !strings.HasSuffix(string(line), "\n") {
		eol := false
		e.EOL = &eol
	}
	return e
}

// encodeLine marshals one header or entry as a JSON line, keeping embedded
// frames as they were written: no HTML escaping, key order unchanged.
func encodeLine(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// exchange is one recorded launch.
type exchange struct {
	path    string
	header  header
	entries []entry
}

func readExchange(path string) (exchange, error) {
	f, err := os.Open(path)
	if err != nil {
		return exchange{}, err
	}
	defer f.Close()
	x := exchange{path: path}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 64<<20)
	first := true
	for scanner.Scan() {
		if first {
			if err := json.Unmarshal(scanner.Bytes(), &x.header); err != nil {
				return exchange{}, fmt.Errorf("%s: header: %w", path, err)
			}
			first = false
			continue
		}
		var e entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return exchange{}, fmt.Errorf("%s:%d: %w", path, len(x.entries)+2, err)
		}
		x.entries = append(x.entries, e)
	}
	if err := scanner.Err(); err != nil {
		return exchange{}, fmt.Errorf("%s: %w", path, err)
	}
	if first {
		return exchange{}, fmt.Errorf("%s: no header", path)
	}
	return x, nil
}

// required is how many entries a replay must play: all of them, except that
// the library may kill a CLI before it reads that its input closed.
func (x exchange) required() int {
	n := len(x.entries)
	for n > 0 && x.entries[n-1].EOF {
		n--
	}
	return n
}

// describe is an entry as its recording holds it.
func describe(e entry) string {
	b, _ := encodeLine(e)
	return strings.TrimSpace(string(b))
}
