package cassette

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"
)

// replay plays the recording of this launch: the Nth launch in a scenario
// plays NNN.jsonl, whose arguments must be this launch's.
func replay(c control) int {
	p := c.launchPlaceholders()
	cursor, path, err := claim(c.Claims, "%03d.jsonl", 1)
	if err != nil {
		return report(c, "claim a launch: %v", err)
	}
	defer cursor.Close()
	path = filepath.Join(c.Dir, filepath.Base(path))
	if _, err := os.Stat(path); err != nil {
		return report(c, "launch %s %q was not recorded", filepath.Base(path), p.argv())
	}
	x, err := readExchange(path)
	if err != nil {
		return report(c, "%v", err)
	}
	if argv := p.argv(); !slices.Equal(x.header.Argv, argv) {
		return report(c, "%s: launched with arguments %q, recorded with %q", filepath.Base(path), argv, x.header.Argv)
	}
	pl := player{c: c, p: p, subs: p.forms(), x: x, in: bufio.NewReader(os.Stdin), cursor: cursor}
	return pl.play()
}

// report records a divergence where the test will find it, and on standard
// error, which the library may surface.
func report(c control, format string, args ...any) int {
	msg := fmt.Sprintf(format, args...)
	if f, _, err := claim(c.Reports, "%03d.txt", 1); err == nil {
		_, _ = f.WriteString(msg + "\n")
		_ = f.Close()
	}
	fmt.Fprintln(os.Stderr, "cassette:", msg)
	return diverged
}

type player struct {
	c      control
	p      placeholders
	subs   []substitution
	x      exchange
	in     *bufio.Reader
	cursor *os.File // how many entries have been played, for the test to check
}

// played notes that the first n entries have been played. It is written at
// once: the library stops a CLI by killing it, so a replay cut short by the
// library sending less than was recorded leaves only this to show it.
func (pl *player) played(n int) {
	_, _ = pl.cursor.WriteAt([]byte(fmt.Sprintf("%08d", n)), 0)
}

func (pl *player) play() int {
	for i, e := range pl.x.entries {
		pl.played(i)
		switch {
		case e.Out != nil:
			pl.write(os.Stdout, pl.p.fill(string(e.Out), true), e.EOL)
		case e.OutText != nil:
			pl.write(os.Stdout, pl.p.fill(*e.OutText, false), e.EOL)
		case e.Err != nil:
			pl.write(os.Stderr, pl.p.fill(*e.Err, false), e.EOL)
		case e.In != nil || e.InText != nil:
			line, err := pl.in.ReadBytes('\n')
			if len(line) == 0 && err != nil {
				return pl.diverge(i, "expected %s, but the library closed standard input", want(e))
			}
			if problem := pl.match(e, line); problem != "" {
				return pl.diverge(i, "%s", problem)
			}
		case e.EOF:
			rest, _ := io.ReadAll(pl.in)
			if len(bytes.TrimSpace(rest)) > 0 {
				return pl.diverge(i, "expected standard input to close, got %q", bytes.TrimSpace(rest))
			}
		case e.Exit != nil:
			pl.played(i + 1)
			return *e.Exit
		}
	}
	pl.played(len(pl.x.entries))
	// The recorded CLI was killed rather than exiting; wait to be killed too.
	if pl.x.header.Stdin == "pipe" {
		_, _ = io.Copy(io.Discard, pl.in)
	}
	time.Sleep(time.Minute)
	return 1
}

func (pl *player) write(w *os.File, line string, eol *bool) {
	if eol == nil || *eol {
		line += "\n"
	}
	_, _ = w.WriteString(line)
}

func (pl *player) diverge(i int, format string, args ...any) int {
	where := fmt.Sprintf("%s line %d: ", filepath.Base(pl.x.path), i+2)
	return report(pl.c, where+format, args...)
}

func want(e entry) string {
	if e.In != nil {
		return string(e.In)
	}
	return fmt.Sprintf("%q", *e.InText)
}

// match compares a line the library sent with the recorded one, as JSON
// values rather than text, so only a change in what was said differs.
func (pl *player) match(e entry, line []byte) string {
	got := lineEntry(true, []byte(nameWith(pl.subs, string(line))))
	if e.InText != nil {
		if got.InText == nil || *got.InText != *e.InText {
			return fmt.Sprintf("expected %q\n  got %s", *e.InText, bytes.TrimSpace(line))
		}
		return ""
	}
	if got.In == nil || !reflect.DeepEqual(decode(e.In), decode(got.In)) {
		return fmt.Sprintf("expected %s\n  got %s", e.In, bytes.TrimSpace(got.In))
	}
	return ""
}

func decode(raw json.RawMessage) any {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	_ = d.Decode(&v)
	return v
}
