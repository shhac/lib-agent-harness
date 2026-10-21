package cassette

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"
)

// controlName is the control file a linked binary finds beside itself.
const controlName = "cassette.json"

// control is what a test tells the binary it links in as the CLI.
type control struct {
	Mode         string       `json:"mode"` // "record" or "replay"
	CLI          string       `json:"cli"`
	Version      string       `json:"version"`
	Real         string       `json:"real,omitempty"` // record: the CLI to run
	Dir          string       `json:"dir"`            // the exchange files
	Claims       string       `json:"claims"`         // replay: one file per exchange played
	Reports      string       `json:"reports"`        // one file per divergence
	Placeholders placeholders `json:"placeholders"`
	Redactions   Redactions   `json:"redactions,omitempty"`
}

// diverged is the exit code of a replay that met something its recording
// does not hold.
const diverged = 97

// Main runs this binary as the CLI when a test has linked it in as one, and
// does not return then. Call it from the test package's init, before any test
// flag is read.
func Main() {
	c, ok := findControl()
	if !ok {
		return
	}
	switch c.Mode {
	case "record":
		os.Exit(record(c))
	case "replay":
		os.Exit(replay(c))
	}
	os.Exit(2)
}

func findControl() (control, bool) {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exe))
	}
	if len(os.Args) > 0 {
		candidates = append(candidates, filepath.Dir(os.Args[0]))
	}
	for _, dir := range candidates {
		data, err := os.ReadFile(filepath.Join(dir, controlName))
		if err != nil {
			continue
		}
		var c control
		if json.Unmarshal(data, &c) == nil {
			return c, true
		}
	}
	return control{}, false
}

// launchPlaceholders are the test's placeholders and this launch's own: its
// working directory and the user's home.
func (c control) launchPlaceholders() placeholders {
	p := placeholders{}
	for k, v := range c.Placeholders {
		p[k] = v
	}
	if cwd, err := os.Getwd(); err == nil && !p.holds(cwd) {
		p["CWD"] = cwd
	}
	if home, err := os.UserHomeDir(); err == nil && !p.holds(home) {
		p["USERHOME"] = home
	}
	return p
}

func (p placeholders) argv() []string {
	args := slices.Clone(os.Args[1:])
	for i, a := range args {
		args[i] = p.name(a)
	}
	return args
}

// stdinPiped reports whether the launch gave this process a pipe to read, as
// opposed to nothing (the null device).
func stdinPiped() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeNamedPipe != 0
}

// claim takes the first free name in dir, so concurrent launches in one
// scenario each get their own.
func claim(dir, format string, start int) (*os.File, string, error) {
	for i := start; i < start+1000; i++ {
		name := filepath.Join(dir, fmt.Sprintf(format, i))
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("cassette: no free exchange name")
}

// record runs the real CLI as this process was run, keeping every line each
// way. Each line is written to the exchange before it is passed on, so a
// reply is always recorded after what it answered; and each write reaches
// the file at once, since the library stops a CLI by killing its group.
func record(c control) int {
	p := c.launchPlaceholders()
	f, _, err := claim(c.Dir, "%03d.jsonl", 1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cassette:", err)
		return 2
	}
	defer f.Close()
	var mu sync.Mutex
	write := func(v any) {
		b, err := encodeLine(v)
		if err != nil {
			return
		}
		line := p.name(string(b))
		mu.Lock()
		defer mu.Unlock()
		_, _ = f.WriteString(line)
	}
	piped := stdinPiped()
	h := header{CLI: c.CLI, Version: c.Version, Argv: p.argv(), Stdin: "none", Recorded: time.Now().UTC().Format("2006-01-02"), GOOS: runtime.GOOS}
	if piped {
		h.Stdin = "pipe"
	}
	write(h)

	cmd := exec.Command(c.Real, os.Args[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 2
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 2
	}
	var stdin io.WriteCloser
	if piped {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return 2
		}
	} else {
		cmd.Stdin = os.Stdin
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "cassette: start", c.Real+":", err)
		return 2
	}
	if piped {
		go func() {
			r := bufio.NewReader(os.Stdin)
			for {
				line, err := r.ReadBytes('\n')
				if len(line) > 0 {
					write(lineEntry(true, line))
					_, _ = stdin.Write(line)
				}
				if err != nil {
					write(entry{EOF: true})
					_ = stdin.Close()
					return
				}
			}
		}()
	}
	var wg sync.WaitGroup
	tee := func(from io.Reader, to io.Writer, wrap func([]byte) entry) {
		defer wg.Done()
		r := bufio.NewReader(from)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				write(wrap(line))
				_, _ = to.Write(line)
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go tee(stdout, os.Stdout, func(line []byte) entry {
		e := lineEntry(false, line)
		if e.Out != nil {
			e.Out = c.Redactions.apply(e.Out)
		}
		return e
	})
	go tee(stderr, os.Stderr, errEntry)
	wg.Wait()
	_ = cmd.Wait()
	code := cmd.ProcessState.ExitCode()
	if code < 0 {
		return 1
	}
	write(entry{Exit: &code})
	return code
}
