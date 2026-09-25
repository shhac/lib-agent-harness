//go:build !windows

package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The installed CLI's own answers, from Claude Code 2.1.282 run with a
// disposable home against a provider that refused inference: the transcript
// folder each working directory produced.
func TestClaudeProjectKeyMatchesTheInstalledCLI(t *testing.T) {
	for path, want := range map[string]string{
		"/private/tmp/claude-501/-Users-paul-projects-personal-crew-assistant/0d697b0a-547e-4541-ba89-c60b5dfe0096/scratchpad/probe-wd/a.b_c d":                                                           "-private-tmp-claude-501--Users-paul-projects-personal-crew-assistant-0d697b0a-547e-4541-ba89-c60b5dfe0096-scratchpad-probe-wd-a-b-c-d",
		"/private/tmp/claude-501/-Users-paul-projects-personal-crew-assistant/0d697b0a-547e-4541-ba89-c60b5dfe0096/scratchpad/probe-wd/" + strings.Repeat("long", 20) + "/" + strings.Repeat("deeper", 8): "-private-tmp-claude-501--Users-paul-projects-personal-crew-assistant-0d697b0a-547e-4541-ba89-c60b5dfe0096-scratchpad-probe-wd-longlonglonglonglonglonglonglonglonglonglonglonglonglonglonglonglonglonglo-bfij1k",
	} {
		if got := claudeProjectSlug(path); got != want {
			t.Errorf("claudeProjectSlug(%q)\n got %s\nwant %s", path, got, want)
		}
	}
	// A non-ASCII character outside the basic plane is two UTF-16 units, and
	// the CLI's pattern replaces each of them.
	if got := claudeProjectSlug("/a/\U0001F600"); got != "-a---" {
		t.Errorf("astral character became %q", got)
	}
}

// The CLI runs in the resolved working directory, so a symlinked one keys its
// transcripts by the target. An inherited project folder name replaces the
// key, as it does in the CLI, when a config directory is set.
func TestClaudeProjectKeyFollowsTheCLIsInputs(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	o := mustNormalize(t, Options{Engine: Claude, WorkDir: link, Home: t.TempDir()})
	if got, want := claudeProjectKey(o), claudeProjectSlug(resolved); got != want {
		t.Fatalf("symlinked working directory keyed as %s, want %s", got, want)
	}
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "chosen_name")
	if got := claudeProjectKey(o); got != "chosen_name" {
		t.Fatalf("an inherited project folder name was ignored: %s", got)
	}
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "../escape")
	if got := claudeProjectKey(o); got != claudeProjectSlug(resolved) {
		t.Fatalf("an invalid project folder name was used: %s", got)
	}
}

func TestOpenWithoutAReferenceStarts(t *testing.T) {
	o, log := persistentOptions(t, Claude)
	ctx := probeContext(t)
	s, opened, err := Open(ctx, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, ctx, s)
	if opened != (Opened{}) {
		t.Fatalf("a start without a reference reported %+v", opened)
	}
	if args := logged(t, log, "args:"); len(args) != 1 || strings.Contains(args[0], `"--resume"`) {
		t.Fatalf("want one fresh launch, got %v", args)
	}
}

func TestOpenResumesAStoredConversation(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			o, _ := persistentOptions(t, engine)
			ctx := probeContext(t)
			s, _, err := Open(ctx, o, nil)
			if err != nil {
				t.Fatal(err)
			}
			runTurn(t, ctx, s, "hello")
			ref := s.Ref()
			release(t, ctx, s)
			resumed, opened, err := Open(ctx, o, &ref)
			if err != nil {
				t.Fatal(err)
			}
			defer release(t, ctx, resumed)
			if opened != (Opened{Resumed: true}) || resumed.Ref() != ref {
				t.Fatalf("the stored conversation was not resumed: %+v %+v", opened, resumed.Ref())
			}
		})
	}
}

// An ordinary session has no private directory to reclaim; Open still resumes
// it, and still notices when its transcript is gone.
func TestOpenResumesAnOrdinaryClaudeSession(t *testing.T) {
	binary, log := fakeHarness(t, fakeClean, fakePersistEnv+"=1")
	o := Options{Engine: Claude, Binary: binary, WorkDir: t.TempDir(), Home: t.TempDir()}
	ctx := probeContext(t)
	s, _, err := Open(ctx, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	runTurn(t, ctx, s, "hello")
	ref := s.Ref()
	s.Close()
	resumed, opened, err := Open(ctx, o, &ref)
	if err != nil || !opened.Resumed {
		t.Fatalf("an ordinary session did not resume: %+v %v", opened, err)
	}
	resumed.Close()
	if err = os.RemoveAll(filepath.Join(o.Home, "projects")); err != nil {
		t.Fatal(err)
	}
	fresh, opened, err := Open(ctx, o, &ref)
	if err != nil || opened.Fresh != FreshUnavailable {
		t.Fatalf("a missing transcript did not start fresh: %+v %v", opened, err)
	}
	fresh.Close()
	if resumes := strings.Count(strings.Join(logged(t, log, "args:"), "\n"), `"--resume"`); resumes != 1 {
		t.Fatalf("a missing transcript was resumed anyway: %d resumes", resumes)
	}
}

func TestOpenStartsFreshForAnIncompatibleReference(t *testing.T) {
	o, _ := persistentOptions(t, Claude)
	ctx := probeContext(t)
	for name, ref := range map[string]Ref{
		"another model":  reference(mustNormalize(t, withModel(o, "other")), newID()),
		"another engine": {Engine: Codex, ID: "thread", Home: o.Home, WorkDir: o.WorkDir},
	} {
		t.Run(name, func(t *testing.T) {
			s, opened, err := Open(ctx, o, &ref)
			if err != nil {
				t.Fatal(err)
			}
			defer release(t, ctx, s)
			if opened != (Opened{Fresh: FreshIncompatible}) || s.Ref().ID == ref.ID {
				t.Fatalf("an incompatible reference was not replaced: %+v %+v", opened, s.Ref())
			}
		})
	}
}

func withModel(o Options, model string) Options { o.Model = model; return o }

// A conversation the harness no longer has starts fresh. Claude's is checked
// before launching; Codex's is found missing when thread/resume is refused.
func TestOpenStartsFreshWhenTheConversationIsGone(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			o, log := persistentOptions(t, engine)
			ctx := probeContext(t)
			s, _, err := Open(ctx, o, nil)
			if err != nil {
				t.Fatal(err)
			}
			runTurn(t, ctx, s, "hello")
			ref := s.Ref()
			release(t, ctx, s)
			gone := filepath.Join(o.Home, "projects")
			if engine == Codex {
				gone = filepath.Join(o.RuntimeHome, "fake-threads")
			}
			if err = os.RemoveAll(gone); err != nil {
				t.Fatal(err)
			}
			fresh, opened, err := Open(ctx, o, &ref)
			if err != nil {
				t.Fatal(err)
			}
			defer release(t, ctx, fresh)
			if opened != (Opened{Fresh: FreshUnavailable}) {
				t.Fatalf("a missing conversation reported %+v", opened)
			}
			launches := strings.Join(logged(t, log, "args:"), "\n")
			if engine == Claude {
				if strings.Contains(launches, `"--resume"`) {
					t.Fatal("a missing transcript was launched with --resume")
				}
				if fresh.Ref().ID == ref.ID {
					t.Fatal("the fresh conversation reused the missing one's id")
				}
			} else if got := logged(t, log, "resume:"); len(got) != 1 {
				t.Fatalf("want one refused thread/resume, got %v", got)
			}
			runTurn(t, ctx, fresh, "again")
		})
	}
}

// A transcript can be there and still not be a conversation the CLI will
// reopen. The resumed harness exits during startup, and Open starts fresh.
func TestOpenStartsFreshWhenAResumedClaudeExits(t *testing.T) {
	o, log := persistentOptions(t, Claude)
	ctx := probeContext(t)
	id := newID()
	project := filepath.Join(o.Home, "projects", claudeProjectKey(mustNormalize(t, o)))
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	putSynthetic(t, project, id+".jsonl", `{"type":"summary"}`+"\n")
	ref := reference(mustNormalize(t, o), id)
	s, opened, err := Open(ctx, o, &ref)
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, ctx, s)
	if opened != (Opened{Fresh: FreshUnavailable}) {
		t.Fatalf("an exiting resume reported %+v", opened)
	}
	launches := logged(t, log, "args:")
	if len(launches) != 2 || !strings.Contains(launches[0], `"--resume"`) || strings.Contains(launches[1], `"--resume"`) {
		t.Fatalf("want a resume then a fresh start, got %v", launches)
	}
}

// Open never starts a harness over one it cannot confirm gone.
func TestOpenHoldsAnUnconfirmedLaunch(t *testing.T) {
	o, log := persistentOptions(t, Claude)
	if err := recordLaunch(o.Restriction.Tools.Dir, launchRecord{Engine: string(Claude), Launch: "elsewhere", Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s, _, err := Open(probeContext(t), o, nil)
	if s != nil {
		s.Close()
		t.Fatal("a session was opened over an unconfirmed launch")
	}
	if !errors.Is(err, ErrUnreclaimed) || !errors.Is(err, ErrUncertainLaunch) {
		t.Fatalf("want the uncertain launch held, got %v", err)
	}
	if n := invocations(t, log, "session"); n != 0 {
		t.Fatalf("a harness was launched %d times", n)
	}
}

// A session that is running holds its lease, and Open refuses rather than
// reclaiming — and so killing — the harness that session is driving.
func TestOpenLeavesALiveSessionAlone(t *testing.T) {
	o, log := persistentOptions(t, Claude)
	ctx := probeContext(t)
	live, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, ctx, live)
	ref := live.Ref()
	s, _, err := Open(ctx, o, &ref)
	if s != nil {
		s.Close()
		t.Fatal("a second session was opened over a live one")
	}
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("want the held lease reported, got %v", err)
	}
	if n := invocations(t, log, "session"); n != 1 {
		t.Fatalf("want only the live harness launched, got %d", n)
	}
	runTurn(t, ctx, live, "still here")
}

// A harness left running by a process that died is not started over. Without a
// live bridge naming it, it cannot be identified as this launch, so it is held
// rather than signalled or replaced.
func TestOpenHoldsAnOrphanedHarness(t *testing.T) {
	o, log := persistentOptions(t, Claude)
	ctx := probeContext(t)
	orphan, err := Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer orphan.Close()
	runTurn(t, ctx, orphan, "hello")
	ref := orphan.Ref()
	record, err := readLaunchRecord(o.Restriction.Tools.Dir)
	if err != nil || record == nil {
		t.Fatalf("no launch record: %v", err)
	}
	// Stand in for the owning process dying: its lease goes, its harness does
	// not, and Close is never reached.
	orphan.mu.Lock()
	host := orphan.tools
	orphan.mu.Unlock()
	host.mu.Lock()
	lease := host.lease
	host.lease = nil
	host.mu.Unlock()
	_ = lease.Close()
	s, _, err := Open(ctx, o, &ref)
	if s != nil {
		s.Close()
		t.Fatal("a session was opened over an orphaned harness")
	}
	if !errors.Is(err, ErrUnreclaimed) {
		t.Fatalf("want the orphan held, got %v", err)
	}
	if alive, _ := groupAlive(record.Group); !alive {
		t.Fatal("an unidentified harness was signalled")
	}
	if n := invocations(t, log, "session"); n != 1 {
		t.Fatalf("want only the orphan launched, got %d", n)
	}
}
