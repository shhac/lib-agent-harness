package cassette

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// echoCLI is the "real" CLI the tests record: this test binary, run with
// echoEnv set, answering each JSON-RPC request with its own params and
// exiting 3 when its input closes.
const echoEnv = "LIB_HARNESS_CASSETTE_ECHO"

func init() {
	Main()
	if os.Getenv(echoEnv) == "1" {
		os.Exit(echoCLI())
	}
}

func echoCLI() int {
	in := bufio.NewScanner(os.Stdin)
	fmt.Fprintln(os.Stderr, "echo ready in", mustGetwd())
	fmt.Println(`{"jsonrpc":"2.0","method":"hello","params":{"cwd":` + quote(mustGetwd()) + `}}`)
	for in.Scan() {
		var m map[string]json.RawMessage
		if json.Unmarshal(in.Bytes(), &m) != nil {
			fmt.Println("not json: " + in.Text())
			continue
		}
		fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", m["id"], m["params"])
	}
	return 3
}

func mustGetwd() string { d, _ := os.Getwd(); return d }

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

// session runs the linked binary as a library would: it sends two requests,
// numbered from first, and one text line, then closes its input.
func session(t *testing.T, binary, dir string, first int) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(binary, "acp", "--cwd", dir)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"new","params":{"cwd":%s}}`+"\n"+
			`{"jsonrpc":"2.0","method":"note","params":{"x":1}}`+"\n"+
			`{"jsonrpc":"2.0","id":%d,"method":"prompt","params":{"text":"hi"}}`+"\n"+
			"plain words\n", first, quote(dir), first+1))
	var out, errs strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errs
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return out.String(), errs.String(), cmd.ProcessState.ExitCode()
}

func recordEcho(t *testing.T, recordings string, redactions Redactions) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(echoEnv, "1")
	tape := Start(t, Config{CLI: "echo", Dir: recordings, Record: true, Real: self, Version: "1.0", Redactions: redactions})
	work := t.TempDir()
	tape.Placeholder("WORKDIR", work)
	out, _, code := session(t, tape.Binary(), work, 1)
	if code != 3 || !strings.Contains(out, `"id":2,"result":{"text":"hi"}`) {
		t.Fatalf("recorded run: exit %d, stdout %s", code, out)
	}
}

func TestReplayAnswersAsTheRecordedCLIDid(t *testing.T) {
	recordings := t.TempDir()
	t.Run("record", func(t *testing.T) { recordEcho(t, recordings, nil) })

	data, err := os.ReadFile(filepath.Join(recordings, "001.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"argv":["acp","--cwd","@@WORKDIR@@"]`, `"stdin":"pipe"`, `{"in":{"jsonrpc":"2.0","id":1,"method":"new","params":{"cwd":"@@WORKDIR@@"}}}`, `{"in_text":"plain words"}`, `{"err":"echo ready in @@WORKDIR@@"}`, `{"eof":true}`, `{"exit":3}`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("recording lacks %s:\n%s", want, data)
		}
	}

	t.Setenv(echoEnv, "")
	tape := Start(t, Config{CLI: "echo", Dir: recordings})
	work := t.TempDir()
	tape.Placeholder("WORKDIR", work)
	out, errs, code := session(t, tape.Binary(), work, 1)
	if code != 3 {
		t.Fatalf("replay exit %d, stderr %s", code, errs)
	}
	for _, want := range []string{`{"jsonrpc":"2.0","method":"hello","params":{"cwd":` + quote(work) + `}}`, `"id":1,"result":{"cwd":` + quote(work) + `}`, `"id":2,"result":{"text":"hi"}`, "not json: plain words"} {
		if !strings.Contains(out, want) {
			t.Errorf("replayed stdout lacks %s:\n%s", want, out)
		}
	}
	if !strings.Contains(errs, "echo ready in "+work) {
		t.Errorf("replayed stderr: %s", errs)
	}
}

// fakeTB collects what Tape's checks report, so a test can expect one.
type fakeTB struct {
	testing.TB
	errors []string
}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func TestReplayReportsWhatTheLibrarySentDifferently(t *testing.T) {
	recordings := t.TempDir()
	t.Run("record", func(t *testing.T) { recordEcho(t, recordings, nil) })
	t.Setenv(echoEnv, "")

	fake := &fakeTB{TB: t}
	tape := Start(fake, Config{CLI: "echo", Dir: recordings})
	work := t.TempDir()
	// The working directory is not named, so the request carries a path the
	// recording does not hold.
	_, _, code := session(t, tape.Binary(), work, 1)
	if code != diverged {
		t.Fatalf("exit %d, want %d", code, diverged)
	}
	tape.finish()
	if len(fake.errors) != 1 || !strings.Contains(fake.errors[0], `launched with arguments ["acp" "--cwd" "@@CWD@@"], recorded with ["acp" "--cwd" "@@WORKDIR@@"]`) || !strings.Contains(fake.errors[0], "re-record with -record=echo") {
		t.Fatalf("errors: %q", fake.errors)
	}
}

func TestReplayComparesRequestsAsJSONValues(t *testing.T) {
	pl := player{}
	recorded := entry{In: json.RawMessage(`{"jsonrpc":"2.0","id":"5","method":"m","params":{"a":1,"b":[true,null]}}`)}
	if problem := pl.match(recorded, []byte(`{"params":{"b":[true,null],"a":1},"method":"m","id":"5","jsonrpc":"2.0"}`+"\n")); problem != "" {
		t.Fatal(problem)
	}
	for _, changed := range []string{
		`{"jsonrpc":"2.0","id":"5","method":"m","params":{"a":2,"b":[true,null]}}`,
		`{"jsonrpc":"2.0","id":"6","method":"m","params":{"a":1,"b":[true,null]}}`,
		`{"jsonrpc":"2.0","id":5,"method":"m","params":{"a":1,"b":[true,null]}}`,
	} {
		if problem := pl.match(recorded, []byte(changed)); problem == "" {
			t.Errorf("%s matched the recording", changed)
		}
	}
}

// A library that stops sending what it sent when the recording was made is
// caught even though it then kills the CLI, as a session's Close does.
func TestReplayReportsWhatTheLibraryNoLongerSends(t *testing.T) {
	recordings := t.TempDir()
	t.Run("record", func(t *testing.T) { recordEcho(t, recordings, nil) })
	t.Setenv(echoEnv, "")
	fake := &fakeTB{TB: t}
	tape := Start(fake, Config{CLI: "echo", Dir: recordings})
	work := t.TempDir()
	tape.Placeholder("WORKDIR", work)
	cmd := exec.Command(tape.Binary(), "acp", "--cwd", work)
	cmd.Dir = work
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(stdin, `{"jsonrpc":"2.0","id":1,"method":"new","params":{"cwd":%s}}`+"\n", quote(work))
	// Kill the CLI once the replay waits for the second line, as Close would.
	cursor := filepath.Join(tape.c.Claims, "001.jsonl")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(cursor); string(b) == "00000001" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the replay never reached the second line")
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	tape.finish()
	if len(fake.errors) != 1 || !strings.Contains(fake.errors[0], "the library no longer sent") || !strings.Contains(fake.errors[0], `"method":"note"`) {
		t.Fatalf("errors: %q", fake.errors)
	}
}

func TestReplayReportsALaunchNeverMade(t *testing.T) {
	recordings := t.TempDir()
	t.Run("record", func(t *testing.T) { recordEcho(t, recordings, nil) })
	fake := &fakeTB{TB: t}
	tape := Start(fake, Config{CLI: "echo", Dir: recordings})
	tape.finish()
	if len(fake.errors) != 1 || !strings.Contains(fake.errors[0], "was never made") {
		t.Fatalf("errors: %q", fake.errors)
	}
}

func TestReplayNeedsPlayOnlyThroughWhatTheLibrarySent(t *testing.T) {
	zero := 0
	in, out := lineEntry(true, []byte(`{"id":"1"}`)), lineEntry(false, []byte(`{"id":"1"}`))
	text := lineEntry(true, []byte("plain"))
	for _, c := range []struct {
		entries []entry
		want    int
	}{
		{[]entry{out, {EOF: true}, {Exit: &zero}}, 0},
		{[]entry{in, out, {Exit: &zero}}, 1},
		{[]entry{in, out, in, out, {EOF: true}}, 3},
		{[]entry{out, text, out}, 2},
	} {
		if got := (exchange{entries: c.entries}).required(); got != c.want {
			t.Errorf("%d entries: required %d, want %d", len(c.entries), got, c.want)
		}
	}
}

func TestRedactionsKeepEverythingElse(t *testing.T) {
	r := Redactions{"team": `"example-team"`, "balance": `"10.00"`, "used": `0`, "servers": `[]`, "size": `"none"`}
	got := string(r.apply([]byte(`{"kind":"credits", "team":"real \"team\"","balance":"24.91","used":3.5e2,"size":272000,"other":"team","mcp":{"servers":[{"name":"mine","url":"https://x"}],"n":[{"team":"t"}]}}`)))
	want := `{"kind":"credits", "team":"example-team","balance":"10.00","used":0,"size":272000,"other":"team","mcp":{"servers":[],"n":[{"team":"example-team"}]}}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestScanFindsWhatMayBePrivate(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	dir := t.TempDir()
	lines := []string{
		`{"out":{"user":"someone@company.io"}}`,
		`{"out":{"user":"someone@example.com","pkg":"react@18.3.1"}}`,
		`{"out":{"token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.x"}}`,
		`{"out":{"key":"sk-ant-0123456789abcdefghij"}}`,
		`{"out":{"path":` + quote(filepath.Join(home, "x")) + `}}`,
		`{"out":{"task":"risk-assessment-and-review-of-things"}}`,
	}
	if err := os.WriteFile(filepath.Join(dir, "001.jsonl"), []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, f := range found {
		kinds = append(kinds, f[strings.LastIndex(f, ": ")+2:])
	}
	want := []string{"an e-mail address", "a JWT", "an API key", "the user's home directory"}
	if strings.Join(kinds, "|") != strings.Join(want, "|") {
		t.Fatalf("found %q, want %q", kinds, want)
	}
	for _, f := range found {
		if strings.Contains(f, "someone") || strings.Contains(f, "sk-ant") {
			t.Fatalf("report holds the text it found: %s", f)
		}
	}
}

func TestPlaceholdersNameTheLongestValueFirst(t *testing.T) {
	p := placeholders{"WORK": "/tmp/run/work", "RUN": "/tmp/run", "WIN": `C:\Users\x`}
	if got := p.name(`/tmp/run/work/a and /tmp/run/b`); got != `@@WORK@@/a and @@RUN@@/b` {
		t.Fatal(got)
	}
	if got := p.name(`{"p":"C:\\Users\\x\\y"}`); got != `{"p":"@@WIN@@\\y"}` {
		t.Fatal(got)
	}
	if got := p.fill(`{"p":"@@WIN@@\\y"}`, true); got != `{"p":"C:\\Users\\x\\y"}` {
		t.Fatal(got)
	}
}
