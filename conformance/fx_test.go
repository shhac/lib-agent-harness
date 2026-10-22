package conformance_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/account"
	"github.com/shhac/lib-agent-harness/catalog"
	"github.com/shhac/lib-agent-harness/internal/cassette"
	"github.com/shhac/lib-agent-harness/native"
	"github.com/shhac/lib-agent-harness/session"
)

// fx's status lists the operator's MCP servers and team, and its credits a
// balance; none of them belong in a recording.
var fx = engine{name: "fx", engine: harness.Fx, redactions: cassette.Redactions{
	"team":                 `"example-team"`,
	"servers":              `[]`,
	"configuration_issues": `[]`,
	"balance":              `"10.00"`,
	"used":                 `"0.00"`,
}}

// fxModel is the model every fx scenario runs, at low effort: the cheapest
// the recording login offers.
const fxModel = "gpt-5.6-luna"

const fxWritePrompt = "Create a file named probe.txt in the working directory containing the text hi, then say done."

func TestFxCatalog(t *testing.T) {
	scenario(t, fx, "catalog", func(t *testing.T, p harness.Provider, _ *cassette.Tape) {
		models, err := catalog.Discover(testContext(t), p)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(models, func(m catalog.Model) bool { return m.ID == fxModel }) {
			t.Fatalf("%s is not in the catalog: %+v", fxModel, models)
		}
	})
}

func TestFxAccount(t *testing.T) {
	scenario(t, fx, "account", func(t *testing.T, p harness.Provider, _ *cassette.Tape) {
		report, err := account.Inspect(testContext(t), p)
		if err != nil {
			t.Fatal(err)
		}
		a := report.Account
		if a.LoggedIn == nil || !*a.LoggedIn || a.AuthMethod == "" {
			t.Fatalf("account: %+v", a)
		}
	})
}

func fxRun(t *testing.T, p harness.Provider, tape *cassette.Tape, prompt string) (string, native.Result, error) {
	work := t.TempDir()
	tape.Placeholder("WORKDIR", work)
	c := native.Config{Provider: p, Model: fxModel, Effort: "low", Fx: native.FxOptions{PermissionMode: "ask"}}
	result, err := native.Run(testContext(t), c, native.Request{Prompt: prompt, WorkDir: work}, nil)
	return work, result, err
}

func TestFxNativeRun(t *testing.T) {
	scenario(t, fx, "native-run", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		_, result, err := fxRun(t, p, tape, "Reply with exactly: ok")
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Report) == 0 || result.SessionID == "" {
			t.Fatalf("result: %+v", result)
		}
		if u := result.Usage; !u.Known || u.Input <= 0 || u.Output <= 0 {
			t.Fatalf("usage: %+v", u)
		}
	})
}

// An asking run has no one to ask, so fx ends it at its first write and
// exits 1. The run reports the exit first, and fx's reason as the failure.
func TestFxNativeRunThatWouldWrite(t *testing.T) {
	scenario(t, fx, "native-write", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		work, result, err := fxRun(t, p, tape, fxWritePrompt)
		var runErr *native.RunError
		if !errors.As(err, &runErr) || runErr.Code != native.CodeProcessExited || runErr.ExitCode == nil || *runErr.ExitCode != 1 {
			t.Fatalf("err %v, result %+v", err, result)
		}
		if !strings.Contains(result.Failure, "NonInteractivePermissionRequired") {
			t.Fatalf("failure: %q", result.Failure)
		}
		if tape.Recording() {
			if _, err := os.Stat(filepath.Join(work, "probe.txt")); err == nil {
				t.Fatal("the asking run wrote the file")
			}
		}
	})
}

func fxSession(t *testing.T, p harness.Provider, tape *cassette.Tape, policy string) session.Options {
	work := t.TempDir()
	tape.Placeholder("WORKDIR", work)
	return session.Options{Provider: p, Model: fxModel, Effort: "low", WorkDir: work, Policy: session.Policy{FxPermission: policy}}
}

// turn runs one turn to its end and returns its result and event kinds.
func turn(t *testing.T, s *session.Session, text string) (session.Result, []session.Event) {
	t.Helper()
	ctx := testContext(t)
	tr, err := s.StartTurn(ctx, session.Input{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	var events []session.Event
	for e := range tr.Events() {
		events = append(events, e)
	}
	result, err := tr.Wait(ctx)
	if err != nil {
		t.Fatalf("turn: %v (%+v)", err, result)
	}
	return result, events
}

func kinds(events []session.Event) []string {
	var k []string
	for _, e := range events {
		k = append(k, e.Kind)
	}
	return k
}

// settled checks what every completed fx turn reports: its own usage, final,
// with fx's cache split, and the context as fx measured it.
func settled(t *testing.T, r session.Result) {
	t.Helper()
	if r.Status != "completed" {
		t.Fatalf("status %q", r.Status)
	}
	u := r.Usage
	if !u.Known || !u.Final || u.Input <= 0 || u.Output <= 0 || !u.CacheKnown {
		t.Fatalf("usage: %+v", u)
	}
	if c := r.Context; c.Quality != harness.Measured || c.UsedTokens == nil || c.CapacityTokens == nil {
		t.Fatalf("context: %+v", c)
	}
}

func TestFxSessionTwoTurns(t *testing.T) {
	scenario(t, fx, "session-two-turns", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		s, err := session.Start(testContext(t), fxSession(t, p, tape, ""))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if ref := s.Ref(); ref.Engine != harness.Fx || ref.ID == "" {
			t.Fatalf("reference: %+v", ref)
		}
		first, events := turn(t, s, "Reply with exactly: ok")
		settled(t, first)
		if k := kinds(events); !slices.Contains(k, "text_delta") || k[len(k)-1] != "status" {
			t.Fatalf("events: %v", k)
		}
		second, _ := turn(t, s, "Reply with exactly: still ok")
		settled(t, second)
	})
}

// Denied, the write fails and the turn carries on to its end.
func TestFxSessionDeniesAWrite(t *testing.T) {
	scenario(t, fx, "session-deny", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		o := fxSession(t, p, tape, session.FxDenyWhenAsked)
		s, err := session.Start(testContext(t), o)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		result, events := turn(t, s, fxWritePrompt)
		settled(t, result)
		if !slices.ContainsFunc(events, func(e session.Event) bool { return e.Kind == "tool_completed" && e.Status == "failed" }) {
			t.Fatalf("no tool failed: %v", kinds(events))
		}
		if tape.Recording() {
			if _, err := os.Stat(filepath.Join(o.WorkDir, "probe.txt")); err == nil {
				t.Fatal("the denied write happened")
			}
		}
	})
}

func TestFxSessionAllowsAWrite(t *testing.T) {
	scenario(t, fx, "session-allow", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		o := fxSession(t, p, tape, session.FxAllowWhenAsked)
		s, err := session.Start(testContext(t), o)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		result, events := turn(t, s, fxWritePrompt)
		settled(t, result)
		if !slices.ContainsFunc(events, func(e session.Event) bool { return e.Kind == "tool_completed" && e.Status == "completed" }) {
			t.Fatalf("no tool completed: %v", kinds(events))
		}
		if tape.Recording() {
			if b, err := os.ReadFile(filepath.Join(o.WorkDir, "probe.txt")); err != nil || strings.TrimSpace(string(b)) != "hi" {
				t.Fatalf("the allowed write: %q %v", b, err)
			}
		}
	})
}

func TestFxSessionResumes(t *testing.T) {
	scenario(t, fx, "session-resume", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		o := fxSession(t, p, tape, "")
		s, err := session.Start(testContext(t), o)
		if err != nil {
			t.Fatal(err)
		}
		first, _ := turn(t, s, "Remember the word heliotrope. Reply with exactly: ok")
		settled(t, first)
		ref := s.Ref()
		s.Close()

		resumed, err := session.Resume(testContext(t), o, ref)
		if err != nil {
			t.Fatal(err)
		}
		defer resumed.Close()
		second, _ := turn(t, resumed, "Which word did I ask you to remember? Reply with the word only.")
		settled(t, second)
		if !strings.Contains(strings.ToLower(second.Text), "heliotrope") {
			t.Fatalf("the resumed conversation lost its history: %q", second.Text)
		}
	})
}

func TestFxResumeOfAnUnknownConversation(t *testing.T) {
	scenario(t, fx, "resume-unknown", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		o := fxSession(t, p, tape, "")
		s, err := session.Start(testContext(t), o)
		if err != nil {
			t.Fatal(err)
		}
		ref := s.Ref()
		s.Close()
		ref.ID = "conformance-unknown-conversation"
		if resumed, err := session.Resume(testContext(t), o, ref); err == nil {
			resumed.Close()
			t.Fatal("resumed a conversation fx does not hold")
		} else if !strings.Contains(err.Error(), "unavailable to resume") {
			t.Fatalf("resume: %v", err)
		}
	})
}

func TestFxRefusesAnUnknownModel(t *testing.T) {
	scenario(t, fx, "unknown-model", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		o := fxSession(t, p, tape, "")
		o.Model = "conformance-no-such-model"
		s, err := session.Start(testContext(t), o)
		if err == nil {
			s.Close()
			t.Fatal("started with a model fx does not offer")
		}
		t.Logf("refused: %v", err)
	})
}
