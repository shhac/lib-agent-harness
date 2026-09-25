//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// persistentOptions is a restricted configuration whose fake harness keeps its
// conversations the way the installed CLI does.
func persistentOptions(t *testing.T, engine Engine) (Options, string) {
	t.Helper()
	scenario := fakeClean
	if engine == Codex {
		scenario = fakeListed
	}
	return probedOptions(t, engine, scenario, fakePersistEnv+"=1")
}

// logged returns what the fake harness recorded under a prefix, in order.
func logged(t *testing.T, log, prefix string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if value, found := strings.CutPrefix(line, prefix); found {
			out = append(out, value)
		}
	}
	return out
}

// runTurn drives one turn to completion and returns its result.
func runTurn(t *testing.T, ctx context.Context, s *Session, text string) Result {
	t.Helper()
	turn, err := s.StartTurn(ctx, Input{text})
	if err != nil {
		t.Fatal(err)
	}
	for range turn.Events() {
	}
	result, err := turn.Wait(ctx)
	if err != nil || result.Status != "completed" {
		t.Fatalf("turn did not complete: %+v %v", result, err)
	}
	return result
}

func release(t *testing.T, ctx context.Context, s *Session) {
	t.Helper()
	if out, err := s.Release(ctx); err != nil || !out.Confirmed {
		t.Fatalf("release did not confirm the harness gone: %+v %v", out, err)
	}
}

// A restricted conversation resumed after its process is gone comes back as
// the same conversation, and restricted again: the tool channel is rebuilt, the
// harness is relaunched with the restricted arguments, and Claude's startup
// frame is cross-checked as it was the first time.
func TestRestrictedResumeReestablishesTheRestriction(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			o, log := persistentOptions(t, engine)
			ctx := probeContext(t)
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			runTurn(t, ctx, s, "hello")
			ref := s.Ref()
			release(t, ctx, s)

			resumed, err := Resume(ctx, o, ref)
			if err != nil {
				t.Fatalf("a restricted conversation did not resume: %v", err)
			}
			defer release(t, ctx, resumed)
			if resumed.Ref() != ref {
				t.Fatalf("resume changed the reference: %+v != %+v", resumed.Ref(), ref)
			}
			record, err := readLaunchRecord(o.Restriction.Tools.Dir)
			if err != nil || record == nil || !record.identified() {
				t.Fatalf("the resumed harness was not recorded: %+v %v", record, err)
			}
			launches := logged(t, log, "args:")
			if len(launches) != 2 {
				t.Fatalf("want two credentialed launches, got %d", len(launches))
			}
			args := launches[1]
			required := []string{`"--tools="`, `"--allowedTools=mcp__agent_workspace__read_file,mcp__agent_workspace__finish"`, `"--strict-mcp-config"`, `"--disable-slash-commands"`, `"--resume","` + ref.ID + `"`}
			if engine == Codex {
				required = []string{`model_catalog_json=`, `features.shell_tool=false`, `mcp_servers.agent_workspace.command=`}
				if got := logged(t, log, "resume:"); len(got) != 1 || got[0] != ref.ID {
					t.Fatalf("thread/resume did not name the stored thread: %v", got)
				}
			}
			for _, want := range required {
				if !strings.Contains(args, want) {
					t.Errorf("resumed launch is missing %s in %s", want, args)
				}
			}
			if engine == Claude {
				if got := resumed.Capabilities().RestrictTools.Availability; got != Native {
					t.Errorf("the resumed harness's surface was not cross-checked: %q", got)
				}
			}
			runTurn(t, ctx, resumed, "again")
		})
	}
}

// A conversation the harness no longer has is a failed resume, and a settled
// one: the harness is gone and nothing is left reserving the assignment.
func TestResumingAMissingRestrictedConversationFails(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			o, _ := persistentOptions(t, engine)
			ctx := probeContext(t)
			id := "fake-thread"
			if engine == Claude {
				id = newID()
			}
			s, err := Resume(ctx, o, reference(mustNormalize(t, o), id))
			if s != nil {
				s.Close()
				t.Fatal("a missing conversation resumed")
			}
			if engine == Codex && !errors.Is(err, ErrRejected) {
				t.Fatalf("want a rejected thread/resume, got %v", err)
			}
			var exit *ProcessError
			if engine == Claude && (!errors.As(err, &exit) || exit.Code != ProcessExited) {
				t.Fatalf("want the harness to have exited, got %v", err)
			}
			if out, err := Reclaim(ctx, o.Restriction.Tools.Dir); err != nil || !out.Confirmed || out.Found {
				t.Fatalf("a failed resume left the assignment reserved: %+v %v", out, err)
			}
		})
	}
}

// A release that edits its tools still resumes its stored conversations, and
// the resumed harness is restricted to the new surface, not the stored one.
func TestRestrictedResumeSurvivesAChangedToolSurface(t *testing.T) {
	for _, engine := range []Engine{Claude, Codex} {
		t.Run(string(engine), func(t *testing.T) {
			o, log := persistentOptions(t, engine)
			ctx := probeContext(t)
			s, err := Start(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			runTurn(t, ctx, s, "hello")
			ref := s.Ref()
			release(t, ctx, s)

			tools := freezeTools(o.Restriction.Tools.Tools)
			tools[0].Description = "read a file, described differently"
			tools = append(tools, ToolDefinition{Name: "list_files", Schema: map[string]any{"type": "object"}})
			o.Restriction = &Restriction{Tools: o.Restriction.Tools}
			o.Restriction.Tools.Tools = tools
			resumed, err := Resume(ctx, o, ref)
			if err != nil {
				t.Fatalf("a changed tool surface orphaned the conversation: %v", err)
			}
			defer release(t, ctx, resumed)
			launches := logged(t, log, "args:")
			if engine == Claude {
				if !strings.Contains(launches[len(launches)-1], "mcp__agent_workspace__list_files") {
					t.Fatalf("the resumed harness was not given the new surface: %s", launches[len(launches)-1])
				}
				if got := resumed.Capabilities().RestrictTools.Availability; got != Native {
					t.Errorf("the new surface was not cross-checked: %q", got)
				}
			}
			runTurn(t, ctx, resumed, "again")
		})
	}
}
