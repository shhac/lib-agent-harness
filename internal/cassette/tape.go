package cassette

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Config is one scenario's cassette.
type Config struct {
	// CLI is the CLI's name, which the linked binary is given.
	CLI string
	// Dir holds the scenario's exchange files.
	Dir string
	// Record runs Real and replaces Dir's recordings; otherwise Dir is
	// replayed.
	Record bool
	// Real is the installed CLI to record, as an absolute path.
	Real string
	// Version is the installed CLI's version, kept in each recording.
	Version string
	// Redactions apply to everything recorded.
	Redactions Redactions
}

// Tape is one scenario's cassette, linked in as the CLI.
type Tape struct {
	t       testing.TB
	c       control
	binary  string
	control string
}

// Start links this test binary in as the CLI, recording or replaying as cfg
// says, and checks the outcome when the test ends: no divergence, every
// recorded launch made, and nothing in a new recording that could be a
// credential or the user's own details.
func Start(t testing.TB, cfg Config) *Tape {
	t.Helper()
	// The CLI runs in its own working directory, so a relative Dir would
	// name somewhere else there.
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Dir = dir
	// Not t.TempDir: Windows refuses to remove a binary a process killed
	// without being waited for may still map, so removal is best effort.
	bin, err := os.MkdirTemp("", "cassette-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for range 20 {
			if os.RemoveAll(bin) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	name := cfg.CLI
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	tp := &Tape{t: t, binary: filepath.Join(bin, name), control: filepath.Join(bin, controlName)}
	if err := linkSelf(tp.binary); err != nil {
		t.Fatalf("cassette: link the test binary in as %s: %v", cfg.CLI, err)
	}
	tp.c = control{Mode: "replay", CLI: cfg.CLI, Version: cfg.Version, Dir: cfg.Dir, Claims: t.TempDir(), Reports: t.TempDir(), Placeholders: placeholders{}, Redactions: cfg.Redactions}
	if cfg.Record {
		tp.c.Mode, tp.c.Real = "record", cfg.Real
		if err := os.RemoveAll(cfg.Dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
	} else if files, _ := filepath.Glob(filepath.Join(cfg.Dir, "*.jsonl")); len(files) == 0 {
		t.Fatalf("cassette: nothing recorded in %s", cfg.Dir)
	}
	tp.writeControl()
	t.Cleanup(tp.finish)
	return tp
}

// Binary is the path to launch as the CLI.
func (tp *Tape) Binary() string { return tp.binary }

// Recording reports whether the real CLI runs behind the tape, so effects
// outside the wire, such as a file a tool wrote, happen and can be checked.
func (tp *Tape) Recording() bool { return tp.c.Mode == "record" }

// Placeholder names a value that differs between runs, such as a temporary
// directory, so recordings hold the name and replays the run's own value.
// Set it before the launch that uses it.
func (tp *Tape) Placeholder(name, value string) {
	tp.c.Placeholders[name] = value
	tp.writeControl()
}

func (tp *Tape) writeControl() {
	b, err := json.Marshal(tp.c)
	if err == nil {
		err = os.WriteFile(tp.control, b, 0o600)
	}
	if err != nil {
		tp.t.Fatalf("cassette: write control: %v", err)
	}
}

func (tp *Tape) finish() {
	t := tp.t
	reports, _ := filepath.Glob(filepath.Join(tp.c.Reports, "*.txt"))
	slices.Sort(reports)
	for _, r := range reports {
		b, _ := os.ReadFile(r)
		t.Errorf("cassette: replay of %s diverged from its recording:\n%s(if the change is meant, re-record with -record=%s)", tp.c.Dir, b, tp.c.CLI)
	}
	if tp.c.Mode == "record" {
		leaks, err := Scan(tp.c.Dir)
		if err != nil {
			t.Errorf("cassette: scan %s: %v", tp.c.Dir, err)
		}
		if len(leaks) > 0 {
			_ = os.RemoveAll(tp.c.Dir)
			t.Errorf("cassette: removed the recording, which held what may be private:\n  %s\n(add a Redaction or a Placeholder for it)", strings.Join(leaks, "\n  "))
		}
		return
	}
	files, _ := filepath.Glob(filepath.Join(tp.c.Dir, "*.jsonl"))
	slices.Sort(files)
	for _, f := range files {
		x, err := readExchange(f)
		if err != nil {
			t.Errorf("cassette: %v", err)
			continue
		}
		cursor, err := os.ReadFile(filepath.Join(tp.c.Claims, filepath.Base(f)))
		if err != nil {
			t.Errorf("cassette: recorded launch %s %q was never made", filepath.Base(f), x.header.Argv)
			continue
		}
		played, _ := strconv.Atoi(string(cursor))
		if want := x.required(); played < want && len(reports) == 0 {
			t.Errorf("cassette: launch %s stopped before line %d; the library no longer sent:\n  %s", filepath.Base(f), played+2, describe(x.entries[played]))
		}
	}
}

// linkSelf puts this test binary at path, by a hard link where the file
// system allows one and by a copy where it does not.
func linkSelf(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if os.Link(self, path) == nil {
		return nil
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}
