package conformance_test

import (
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/account"
	"github.com/shhac/lib-agent-harness/catalog"
	"github.com/shhac/lib-agent-harness/internal/cassette"
	"github.com/shhac/lib-agent-harness/native"
	"github.com/shhac/lib-agent-harness/session"
)

// agy's account documents carry the Google account's address and project.
var agy = engine{name: "agy", engine: harness.Antigravity, redactions: cassette.Redactions{
	"email":   `"user@example.com"`,
	"project": `"example-project"`,
}}

// The agy scenarios use the login's default model, so they record whatever
// the login offers rather than a model ID that may have moved.

func TestAntigravityCatalog(t *testing.T) {
	scenario(t, agy, "catalog", func(t *testing.T, p harness.Provider, _ *cassette.Tape) {
		models, err := catalog.Discover(testContext(t), p)
		if err != nil {
			t.Fatal(err)
		}
		if len(models) == 0 {
			t.Fatal("no models")
		}
	})
}

func TestAntigravityAccount(t *testing.T) {
	scenario(t, agy, "account", func(t *testing.T, p harness.Provider, _ *cassette.Tape) {
		report, err := account.Inspect(testContext(t), p)
		if err != nil {
			t.Fatal(err)
		}
		if report.Account.LoggedIn == nil {
			t.Fatalf("login not reported: %+v", report.Account)
		}
	})
}

func TestAntigravityNativeRun(t *testing.T) {
	scenario(t, agy, "native-run", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		work := t.TempDir()
		tape.Placeholder("WORKDIR", work)
		result, err := native.Run(testContext(t), native.Config{Provider: p}, native.Request{Prompt: "Reply with exactly: ok", WorkDir: work}, nil)
		if err != nil {
			t.Fatalf("%v (%+v)", err, result)
		}
		if len(result.Report) == 0 || result.SessionID == "" {
			t.Fatalf("result: %+v", result)
		}
		if u := result.Usage; !u.Known || u.Input <= 0 || u.Output <= 0 {
			t.Fatalf("usage: %+v", u)
		}
	})
}

func agySession(t *testing.T, p harness.Provider, tape *cassette.Tape) session.Options {
	work := t.TempDir()
	tape.Placeholder("WORKDIR", work)
	return session.Options{Provider: p, WorkDir: work}
}

func TestAntigravitySessionTurn(t *testing.T) {
	scenario(t, agy, "session-turn", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		s, err := session.Start(testContext(t), agySession(t, p, tape))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		result, events := turn(t, s, "Reply with exactly: ok")
		if result.Status != "completed" {
			t.Fatalf("status %q, events %v", result.Status, kinds(events))
		}
		if u := result.Usage; !u.Known || !u.Final || u.Input <= 0 || u.Output <= 0 {
			t.Fatalf("usage: %+v", u)
		}
	})
}

// agy answers a resume of a conversation it does not hold by starting a new
// one, which the session reports rather than carrying on in.
func TestAntigravityResumeOfAnUnknownConversation(t *testing.T) {
	scenario(t, agy, "resume-unknown", func(t *testing.T, p harness.Provider, tape *cassette.Tape) {
		o := agySession(t, p, tape)
		s, err := session.Start(testContext(t), o)
		if err != nil {
			t.Fatal(err)
		}
		ref := s.Ref()
		s.Close()
		ref.ID = "00000000-0000-4000-8000-000000000000"
		if resumed, err := session.Resume(testContext(t), o, ref); err == nil {
			resumed.Close()
			t.Fatal("resumed a conversation agy does not hold")
		} else if !strings.Contains(err.Error(), "unavailable to resume") {
			t.Fatalf("resume: %v", err)
		}
	})
}
