//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func testSkill(t *testing.T, name string, scripts bool) harness.Skill {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "---\nname: " + name + "\ndescription: Synthetic skill " + name + ".\n---\nRun scripts/go.sh.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "go.sh"), []byte("#!/bin/sh\necho ran \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return harness.Skill{Name: name, Dir: dir, Scripts: scripts}
}

func skillRoot(t *testing.T, s harness.Skill) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	var refused *UnsupportedError
	if !errors.As(err, &refused) {
		t.Fatalf("expected a refusal, got %v", err)
	}
	return refused.Code
}

func ordinaryOptions(t *testing.T, engine harness.Engine) Options {
	o := Options{Provider: harness.Provider{Engine: engine, CLI: harness.CLI{Home: t.TempDir()}}, WorkDir: t.TempDir()}
	if engine == harness.Grok {
		o.Policy.GrokPermission = GrokDenyWhenAsked
	}
	return o
}

// Every engine and mode resolves to the delivery its installed CLI was
// verified to support, and Composed moves each one off native delivery.
func TestSkillDeliveryPerEngineAndMode(t *testing.T) {
	skill := testSkill(t, "demo", false)
	for _, tc := range []struct {
		name     string
		options  func(t *testing.T) Options
		auto     skillDelivery
		composed skillDelivery
	}{
		{"claude ordinary", func(t *testing.T) Options { return ordinaryOptions(t, harness.Claude) }, nativeSkills, instructionSkills},
		{"claude without the Skill tool", func(t *testing.T) Options {
			o := ordinaryOptions(t, harness.Claude)
			o.Policy.ClaudeTools = []string{"Read", "Bash"}
			return o
		}, instructionSkills, instructionSkills},
		{"claude sandboxed", func(t *testing.T) Options { return sandboxOptions(t, harness.Claude, "/usr/bin/true", false) }, instructionSkills, instructionSkills},
		{"claude restricted", func(t *testing.T) Options { return restrictedOptions(t, harness.Claude) }, hostedSkills, hostedSkills},
		{"codex ordinary", func(t *testing.T) Options { return ordinaryOptions(t, harness.Codex) }, instructionSkills, instructionSkills},
		{"codex sandboxed", func(t *testing.T) Options { return sandboxOptions(t, harness.Codex, "/usr/bin/true", false) }, nativeSkills, instructionSkills},
		{"codex restricted", func(t *testing.T) Options { return restrictedOptions(t, harness.Codex) }, hostedSkills, hostedSkills},
		{"grok", func(t *testing.T) Options { return ordinaryOptions(t, harness.Grok) }, nativeSkills, instructionSkills},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for delivery, want := range map[harness.SkillDelivery]skillDelivery{harness.SkillDeliveryAuto: tc.auto, harness.SkillDeliveryComposed: tc.composed} {
				o := tc.options(t)
				o.Skills = harness.Skills{Delivery: delivery, Provided: []harness.Skill{skill}}
				o = mustNormalize(t, o)
				if o.skills == nil || o.skills.delivery != want {
					t.Fatalf("%q delivery: %+v, want %d", delivery, o.skills, want)
				}
			}
		})
	}
}

func TestOrdinaryClaudeSessionLoadsAPrivatePlugin(t *testing.T) {
	skill := testSkill(t, "demo", true)
	o := ordinaryOptions(t, harness.Claude)
	o.Skills.Provided = []harness.Skill{skill}
	o = mustNormalize(t, o)
	remove, err := prepareSkillFiles(o)
	if err != nil {
		t.Fatal(err)
	}
	args := commandArgs(o, "id", false, nil)
	dir, found := argValue(args, "--plugin-dir")
	if !found || dir != o.skills.pluginDir || inside(o.WorkDir, dir) {
		t.Fatalf("plugin %q in %q", dir, args)
	}
	if target, err := filepath.EvalSymlinks(filepath.Join(dir, "skills", "demo")); err != nil || target != skillRoot(t, skill) {
		t.Fatalf("plugin skill resolves to %q (%v)", target, err)
	}
	if _, found := argValue(args, "--append-system-prompt"); found {
		t.Fatalf("native delivery needs no index: %q", args)
	}
	remove()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("plugin not removed: %v", err)
	}
}

func TestComposedClaudeSessionsCanReadTheirSkills(t *testing.T) {
	skill := testSkill(t, "demo", false)
	root := skillRoot(t, skill)
	t.Run("ordinary", func(t *testing.T) {
		o := ordinaryOptions(t, harness.Claude)
		o.Instructions = Instructions{Mode: Replace, Text: "You review code."}
		o.Skills = harness.Skills{Delivery: harness.SkillDeliveryComposed, Provided: []harness.Skill{skill}}
		args := commandArgs(mustNormalize(t, o), "id", false, nil)
		if dir, _ := argValue(args, "--add-dir"); dir != root {
			t.Fatalf("skill directory not readable: %q", args)
		}
		prompt, _ := argValue(args, "--system-prompt")
		if !strings.HasPrefix(prompt, "You review code.\n\n") || !strings.Contains(prompt, "SKILL.md: "+filepath.Join(root, "SKILL.md")) || !strings.Contains(prompt, "Do not run its scripts.") {
			t.Fatalf("index: %q", prompt)
		}
	})
	t.Run("sandboxed", func(t *testing.T) {
		o := sandboxOptions(t, harness.Claude, "/usr/bin/true", false)
		o.Skills.Provided = []harness.Skill{skill}
		o = mustNormalize(t, o)
		args := commandArgs(o, "id", false, &launch{extra: sandboxArgs(o)})
		if _, found := argValue(args, "--plugin-dir"); found {
			t.Fatalf("a sandboxed session's --disable-slash-commands removes plugin skills: %q", args)
		}
		if _, found := argValue(args, "--add-dir"); found {
			t.Fatalf("sandbox reads belong in its settings: %q", args)
		}
		var settings struct {
			Sandbox struct {
				Filesystem struct {
					AllowRead []string `json:"allowRead"`
				} `json:"filesystem"`
			} `json:"sandbox"`
			Permissions struct {
				Allow []string `json:"allow"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal([]byte(claudeSandboxSettings(o)), &settings); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(settings.Sandbox.Filesystem.AllowRead, root) || !slices.Contains(settings.Permissions.Allow, "Read(/"+root+"/**)") {
			t.Fatalf("settings: %+v", settings)
		}
		if prompt, _ := argValue(args, "--append-system-prompt"); !strings.Contains(prompt, "- demo: Synthetic skill demo.") {
			t.Fatalf("index: %q", prompt)
		}
	})
	t.Run("without read tools", func(t *testing.T) {
		o := ordinaryOptions(t, harness.Claude)
		o.Policy.ClaudeTools = []string{}
		o.Skills.Provided = []harness.Skill{skill}
		if _, err := normalize(o); refusalCode(t, err) != RefusedSkills {
			t.Fatal(err)
		}
	})
}

func TestCodexSessionsIndexOrLinkTheirSkills(t *testing.T) {
	skill := testSkill(t, "demo", true)
	root := skillRoot(t, skill)
	t.Run("ordinary", func(t *testing.T) {
		o := ordinaryOptions(t, harness.Codex)
		o.Instructions = Instructions{Mode: Append, Text: "Be brief."}
		o.Skills.Provided = []harness.Skill{skill}
		o = mustNormalize(t, o)
		text, _ := codexThreadParams(o, o.WorkDir, false, "")["developerInstructions"].(string)
		if !strings.HasPrefix(text, "Be brief.\n\n") || !strings.Contains(text, "scripts may be run from "+root) {
			t.Fatalf("developer instructions: %q", text)
		}
		if err := syncRuntimeSkills(o); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("sandboxed", func(t *testing.T) {
		o := sandboxOptions(t, harness.Codex, "/usr/bin/true", false)
		skillsDir := filepath.Join(o.RuntimeHome, "skills")
		for _, dir := range []string{filepath.Join(skillsDir, ".system", "bundled")} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		stale := filepath.Join(skillsDir, "stale")
		if err := os.Symlink(t.TempDir(), stale); err != nil {
			t.Fatal(err)
		}
		o.Skills.Provided = []harness.Skill{skill}
		o = mustNormalize(t, o)
		if _, ok := codexThreadParams(o, o.WorkDir, false, "")["developerInstructions"]; ok {
			t.Fatal("native delivery needs no index")
		}
		if err := syncRuntimeSkills(o); err != nil {
			t.Fatal(err)
		}
		if target, err := filepath.EvalSymlinks(filepath.Join(skillsDir, "demo")); err != nil || target != root {
			t.Fatalf("runtime home skill resolves to %q (%v)", target, err)
		}
		if _, err := os.Lstat(stale); !os.IsNotExist(err) {
			t.Fatal("an earlier launch's link survived")
		}
		if _, err := os.Stat(filepath.Join(skillsDir, ".system", "bundled")); err != nil {
			t.Fatal("the harness's own skills were touched")
		}
		// A later launch without skills leaves none behind.
		o.skills = nil
		if err := syncRuntimeSkills(o); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(skillsDir, "demo")); !os.IsNotExist(err) {
			t.Fatal("a provided skill outlived its request")
		}
	})
	t.Run("a runtime home entry is never replaced", func(t *testing.T) {
		o := sandboxOptions(t, harness.Codex, "/usr/bin/true", false)
		if err := os.MkdirAll(filepath.Join(o.RuntimeHome, "skills", "demo"), 0o700); err != nil {
			t.Fatal(err)
		}
		o.Skills.Provided = []harness.Skill{skill}
		if err := syncRuntimeSkills(mustNormalize(t, o)); refusalCode(t, err) != RefusedSkillFiles {
			t.Fatal(err)
		}
	})
}

func TestGrokSessionLoadsAPluginOrItsRules(t *testing.T) {
	skill := testSkill(t, "demo", false)
	o := ordinaryOptions(t, harness.Grok)
	o.Skills.Provided = []harness.Skill{skill}
	o = mustNormalize(t, o)
	remove, err := prepareSkillFiles(o)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	args := grokArgs(o)
	if dir, _ := argValue(args, "--plugin-dir"); dir == "" || dir != o.skills.pluginDir || args[len(args)-1] != "stdio" {
		t.Fatalf("args %q", args)
	}
	if _, ok := grokSessionParams(o, false, "")["_meta"]; ok {
		t.Fatal("native delivery needs no rules")
	}

	composed := ordinaryOptions(t, harness.Grok)
	composed.Skills = harness.Skills{Delivery: harness.SkillDeliveryComposed, Provided: []harness.Skill{skill}}
	composed = mustNormalize(t, composed)
	meta, _ := grokSessionParams(composed, true, "s")["_meta"].(map[string]any)
	if rules, _ := meta["rules"].(string); !strings.Contains(rules, "- demo: Synthetic skill demo.") {
		t.Fatalf("rules %q", rules)
	}
	if _, found := argValue(grokArgs(composed), "--plugin-dir"); found {
		t.Fatal("composed delivery loaded a plugin")
	}
}

func TestRestrictedSessionHostsTheSkillTools(t *testing.T) {
	skill, reader := testSkill(t, "demo", true), testSkill(t, "reader", false)
	o := restrictedOptions(t, harness.Codex)
	o.Instructions = Instructions{Mode: Append, Text: "Scoped task."}
	o.Skills.Provided = []harness.Skill{skill, reader}
	if _, err := normalize(o); refusalCode(t, err) != "work_dir_required" {
		t.Fatalf("scripts without a working directory: %v", err)
	}
	var ran SkillCommand
	o.SkillRun = SkillRunOptions{WorkDir: o.WorkDir, Exec: func(_ context.Context, c SkillCommand) (SkillOutput, error) {
		ran = c
		return SkillOutput{ExitCode: 3, Stdout: "out"}, nil
	}}
	callerTools := len(o.Restriction.Tools.Tools)
	n := mustNormalize(t, o)
	if len(o.Restriction.Tools.Tools) != callerTools {
		t.Fatal("normalizing edited the caller's tool host")
	}
	names := toolNames(n.Restriction.Tools.Tools)
	if !slices.Equal(names, []string{"read_file", "finish", LoadSkillTool, RunSkillScriptTool}) {
		t.Fatalf("hosted tools %q", names)
	}
	if !slices.Contains(n.Restriction.Tools.Qualified(), "mcp__agent_workspace__load_skill") {
		t.Fatal("the skill tools are not part of the proven surface")
	}
	text, _ := codexThreadParams(n, n.WorkDir, false, "")["developerInstructions"].(string)
	if !strings.HasPrefix(text, "Scoped task.\n\n") || !strings.Contains(text, "- demo (scripts): Synthetic skill demo.") || strings.Contains(text, skillRoot(t, skill)) {
		t.Fatalf("hosted index: %q", text)
	}

	handler := n.Restriction.Tools.Handler
	call := func(name, arguments string) ToolResult {
		t.Helper()
		result, err := handler.CallTool(context.Background(), ToolCall{Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if r := call(LoadSkillTool, `{"skill":"demo"}`); r.IsError || !strings.Contains(r.Content, "Run scripts/go.sh.") {
		t.Fatalf("load: %+v", r)
	}
	if r := call(LoadSkillTool, `{"skill":"demo","file":"../../etc/passwd"}`); !r.IsError || r.Content != "load_skill error: file_outside_skill" {
		t.Fatalf("escape: %+v", r)
	}
	if r := call(RunSkillScriptTool, `{"skill":"reader","script":"scripts/go.sh"}`); !r.IsError || !strings.Contains(r.Content, "scripts_not_permitted") {
		t.Fatalf("unpermitted script: %+v", r)
	}
	r := call(RunSkillScriptTool, `{"skill":"demo","script":"scripts/go.sh","args":["a"]}`)
	if r.IsError || r.Content != `{"exit_code":3,"timed_out":false,"stdout":"out","stdout_truncated":false,"stderr":"","stderr_truncated":false}` {
		t.Fatalf("script: %+v", r)
	}
	if ran.Skill != "demo" || ran.Script != filepath.Join(skillRoot(t, skill), "scripts", "go.sh") || !slices.Equal(ran.Args, []string{"a"}) || ran.WorkDir != o.WorkDir {
		t.Fatalf("command %+v", ran)
	}
	if r := call("read_file", `{}`); r.Content != `read_file:{}` {
		t.Fatalf("the caller's tools must still reach the caller: %+v", r)
	}

	reserved := restrictedOptions(t, harness.Claude)
	reserved.Restriction.Tools.Tools = append(reserved.Restriction.Tools.Tools, ToolDefinition{Name: LoadSkillTool, Schema: map[string]any{"type": "object"}})
	reserved.Skills.Provided = []harness.Skill{reader}
	if _, err := normalize(reserved); refusalCode(t, err) != RefusedSkillToolReserved {
		t.Fatal(err)
	}
}

// A restricted Claude session keeps its proven flags; the hosted tools are
// allowed by name like any other hosted tool.
func TestRestrictedClaudeAllowsTheSkillTools(t *testing.T) {
	o := restrictedOptions(t, harness.Claude)
	o.Skills.Provided = []harness.Skill{testSkill(t, "demo", false)}
	o = mustNormalize(t, o)
	host, err := newToolHost(o.Restriction.Tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.close()
	args := commandArgs(o, "id", false, &launch{host: host, extra: claudeRestrictedArgs(host)})
	allowed, _ := argValue(args, "--allowedTools")
	if !strings.Contains(allowed, "mcp__agent_workspace__load_skill") || strings.Contains(allowed, "run_skill_script") {
		t.Fatalf("allowed %q", allowed)
	}
	for _, flag := range []string{"--plugin-dir", "--add-dir"} {
		if _, found := argValue(args, flag); found {
			t.Fatalf("restricted launch carries %s: %q", flag, args)
		}
	}
}

func TestSkillToolsForACallersOwnHost(t *testing.T) {
	skill := testSkill(t, "demo", true)
	tools, handler, err := SkillTools(harness.Skills{Provided: []harness.Skill{skill}}, SkillRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if names := toolNames(tools); !slices.Equal(names, []string{LoadSkillTool, RunSkillScriptTool}) {
		t.Fatalf("tools %q", names)
	}
	result, err := handler.CallTool(context.Background(), ToolCall{Name: RunSkillScriptTool, Arguments: json.RawMessage(`{"skill":"demo","script":"scripts/go.sh"}`)})
	if err != nil || !result.IsError || result.Content != "run_skill_script error: work_dir_required" {
		t.Fatalf("%+v %v", result, err)
	}
	if _, err = handler.CallTool(context.Background(), ToolCall{Name: "other"}); !errors.Is(err, ErrToolUnknown) {
		t.Fatalf("other tool: %v", err)
	}
	work := t.TempDir()
	_, handler, _ = SkillTools(harness.Skills{Provided: []harness.Skill{skill}}, SkillRunOptions{WorkDir: work})
	result, err = handler.CallTool(context.Background(), ToolCall{Name: RunSkillScriptTool, Arguments: json.RawMessage(`{"skill":"demo","script":"scripts/go.sh","args":["x"]}`)})
	if err != nil || result.IsError || !strings.Contains(result.Content, `"stdout":"ran x\n"`) {
		t.Fatalf("%+v %v", result, err)
	}
	if _, _, err = SkillTools(harness.Skills{Provided: []harness.Skill{{Name: "demo", Dir: t.TempDir()}}}, SkillRunOptions{}); refusalCode(t, err) != "skill_manifest_missing" {
		t.Fatal(err)
	}
}

func TestGlobalSkillRequestsPerMode(t *testing.T) {
	for _, tc := range []struct {
		name             string
		options          func(t *testing.T) Options
		include, exclude bool
	}{
		{"claude ordinary", func(t *testing.T) Options { return ordinaryOptions(t, harness.Claude) }, true, false},
		{"claude without the Skill tool", func(t *testing.T) Options {
			o := ordinaryOptions(t, harness.Claude)
			o.Policy.ClaudeTools = []string{"Read"}
			return o
		}, false, false},
		{"claude sandboxed", func(t *testing.T) Options { return sandboxOptions(t, harness.Claude, "/usr/bin/true", false) }, false, true},
		{"claude restricted", func(t *testing.T) Options { return restrictedOptions(t, harness.Claude) }, false, true},
		{"codex ordinary", func(t *testing.T) Options { return ordinaryOptions(t, harness.Codex) }, true, false},
		{"codex sandboxed", func(t *testing.T) Options { return sandboxOptions(t, harness.Codex, "/usr/bin/true", false) }, false, false},
		{"codex restricted", func(t *testing.T) Options { return restrictedOptions(t, harness.Codex) }, false, true},
		{"grok", func(t *testing.T) Options { return ordinaryOptions(t, harness.Grok) }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for global, accepted := range map[harness.GlobalSkills]bool{harness.GlobalSkillsDefault: true, harness.GlobalSkillsInclude: tc.include, harness.GlobalSkillsExclude: tc.exclude} {
				o := tc.options(t)
				o.Skills.Global = global
				_, err := normalize(o)
				if accepted && err != nil {
					t.Errorf("%q refused: %v", global, err)
				}
				if !accepted && refusalCode(t, err) != RefusedGlobalSkills {
					t.Errorf("%q: %v", global, err)
				}
			}
		})
	}
}

func TestSkillRequestValuesAreChecked(t *testing.T) {
	skill := testSkill(t, "demo", false)
	for name, tc := range map[string]struct {
		mutate func(*Options)
		code   string
	}{
		"global":          {func(o *Options) { o.Skills.Global = "all" }, RefusedGlobalSkillsInvalid},
		"delivery":        {func(o *Options) { o.Skills.Delivery = "native" }, RefusedSkillDeliveryInvalid},
		"unusable skill":  {func(o *Options) { o.Skills.Provided = []harness.Skill{{Name: "demo", Dir: "relative"}} }, "skill_dir_invalid"},
		"run unconsumed":  {func(o *Options) { o.SkillRun.WorkDir = o.WorkDir }, RefusedConflict},
		"run for natives": {func(o *Options) { o.Skills.Provided, o.SkillRun.WorkDir = []harness.Skill{skill}, o.WorkDir }, RefusedConflict},
	} {
		t.Run(name, func(t *testing.T) {
			o := ordinaryOptions(t, harness.Claude)
			tc.mutate(&o)
			if _, err := normalize(o); refusalCode(t, err) != tc.code {
				t.Fatal(err)
			}
		})
	}
}

// A skill request is part of what a Ref names, but only what changes the
// agent's surface: which skills, whether they may run scripts, and how
// installed skills and delivery were asked for. Moving a skill's directory or
// editing its description resumes the same conversation.
func TestSkillRequestIsPartOfTheReference(t *testing.T) {
	skill := testSkill(t, "demo", false)
	base := Options{Provider: harness.Provider{Engine: harness.Claude, CLI: harness.CLI{Home: "/h"}}, Model: "haiku", WorkDir: "/w"}
	digest := func(mutate func(*Options)) string {
		o := base
		mutate(&o)
		return reference(mustNormalize(t, o), "s1").ConfigHash
	}
	plain := digest(func(*Options) {})
	if plain != "814563750823203c3103008245f016ff59bcf55a8bf7d265dea1d746a292ec7c" {
		t.Fatalf("a session without skills changed digest: %s", plain)
	}
	with := digest(func(o *Options) { o.Skills.Provided = []harness.Skill{skill} })
	moved := testSkill(t, "demo", false)
	if err := os.WriteFile(filepath.Join(moved.Dir, "SKILL.md"), []byte("---\nname: demo\ndescription: Edited.\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]string{
		"none":    plain,
		"scripts": digest(func(o *Options) { s := skill; s.Scripts = true; o.Skills.Provided = []harness.Skill{s} }),
		"composed": digest(func(o *Options) {
			o.Skills = harness.Skills{Delivery: harness.SkillDeliveryComposed, Provided: []harness.Skill{skill}}
		}),
		"installed": digest(func(o *Options) { o.Skills.Global = harness.GlobalSkillsInclude }),
	} {
		if other == with {
			t.Errorf("%s shares the digest", name)
		}
	}
	if digest(func(o *Options) { o.Skills.Provided = []harness.Skill{moved} }) != with {
		t.Error("a moved or edited skill orphaned the session")
	}
}

// The mounted skill tools pass the same launch proof as the caller's own: the
// probe's harness must carry, or the channel must serve, every hosted tool.
func TestRestrictedSkillToolsPassTheLaunchProof(t *testing.T) {
	for engine, scenario := range map[harness.Engine]string{harness.Claude: fakeClean, harness.Codex: fakeListed} {
		t.Run(string(engine), func(t *testing.T) {
			o, log := probedOptions(t, engine, scenario)
			o.Skills.Provided = []harness.Skill{testSkill(t, "demo", true)}
			o.SkillRun.WorkDir = o.WorkDir
			if err := VerifyRestriction(probeContext(t), o); err != nil {
				t.Fatal(err)
			}
			if invocations(t, log, "probe") != 1 {
				t.Fatal("the skill tools were not probed")
			}
		})
	}
}

// An ordinary Claude session launches with its plugin, and the plugin goes
// when the session does.
func TestClaudeSessionPluginLivesAsLongAsTheSession(t *testing.T) {
	binary, log := fakeHarness(t, fakeClean)
	o := ordinaryOptions(t, harness.Claude)
	o.Provider.CLI.Binary = binary
	o.Skills.Provided = []harness.Skill{testSkill(t, "demo", false)}
	s, err := Start(probeContext(t), o)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, line := range strings.Split(string(raw), "\n") {
		if encoded, found := strings.CutPrefix(line, "args:"); found {
			if err := json.Unmarshal([]byte(encoded), &args); err != nil {
				t.Fatal(err)
			}
		}
	}
	dir, found := argValue(args, "--plugin-dir")
	if !found {
		t.Fatalf("launched without its plugin: %q", args)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "demo", "SKILL.md")); err != nil {
		t.Fatalf("plugin unreadable while the session runs: %v", err)
	}
	s.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("plugin outlived the session: %v", err)
	}
}
