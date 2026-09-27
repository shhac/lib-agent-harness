package native

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/tomltest"
)

func writeTestSkill(t *testing.T, name string) harness.Skill {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "---\nname: " + name + "\ndescription: Synthetic skill " + name + ".\n---\nRun scripts/go.sh.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return harness.Skill{Name: name, Dir: dir}
}

// capture runs c and returns the argument vector the CLI would have been
// given, with inspect called while the invocation's files still exist.
func capture(t *testing.T, c Config, r Request, inspect func(args []string)) []string {
	t.Helper()
	var got []string
	c.RunCommand = func(_ context.Context, args []string, _ string, _, _ io.Writer) error {
		got = slices.Clone(args)
		if inspect != nil {
			inspect(args)
		}
		return nil
	}
	_, err := Run(context.Background(), c, r, nil)
	var runErr *RunError
	if err != nil && (!errors.As(err, &runErr) || runErr.Code != CodeNoTerminalResult) {
		t.Fatalf("run: %v", err)
	}
	if got == nil {
		t.Fatal("the run never executed")
	}
	return got
}

func argValue(args []string, flag string) (string, bool) {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1], true
		}
		if value, ok := strings.CutPrefix(arg, flag+"="); ok {
			return value, true
		}
	}
	return "", false
}

func TestClaudeRunLoadsProvidedSkillsAsAPrivatePlugin(t *testing.T) {
	skill := writeTestSkill(t, "demo")
	root, _ := filepath.EvalSymlinks(skill.Dir)
	work := t.TempDir()
	var pluginDir string
	c := Config{Provider: provider(harness.Claude), Skills: harness.Skills{Provided: []harness.Skill{skill}}}
	args := capture(t, c, Request{Prompt: "p", WorkDir: work}, func(args []string) {
		dir, ok := argValue(args, "--plugin-dir")
		if !ok {
			t.Fatalf("no plugin: %q", args)
		}
		pluginDir = dir
		if within(resolved(work), resolved(dir)) {
			t.Fatal("the plugin lies inside the workspace")
		}
		manifest, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json"))
		if err != nil || !strings.Contains(string(manifest), `"agent-harness-skills"`) {
			t.Fatalf("manifest %q %v", manifest, err)
		}
		target, err := filepath.EvalSymlinks(filepath.Join(dir, "skills", "demo"))
		if err != nil || target != root {
			t.Fatalf("skill entry resolves to %q (%v), want %q", target, err, root)
		}
	})
	if i := slices.Index(args, "--"); i < 0 || slices.Index(args, "--plugin-dir") > i {
		t.Fatalf("plugin flag must precede the positional terminator: %q", args)
	}
	if _, found := argValue(args, "--append-system-prompt"); found {
		t.Fatalf("native delivery needs no index: %q", args)
	}
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Fatalf("the plugin outlived its run: %v", err)
	}
}

func TestComposedSkillsAreIndexedInTheRunsInstructions(t *testing.T) {
	skill := writeTestSkill(t, "demo")
	skill.Scripts = true
	root, _ := filepath.EvalSymlinks(skill.Dir)
	manifest := filepath.Join(root, "SKILL.md")
	for _, tc := range []struct {
		engine   harness.Engine
		delivery harness.SkillDelivery
		read     func(t *testing.T, args []string) string
	}{
		{harness.Claude, harness.SkillDeliveryComposed, func(t *testing.T, args []string) string {
			if dir, _ := argValue(args, "--add-dir"); dir != root {
				t.Fatalf("skill directory not readable: %q", args)
			}
			if _, found := argValue(args, "--plugin-dir"); found {
				t.Fatalf("composed delivery loaded a plugin: %q", args)
			}
			text, _ := argValue(args, "--append-system-prompt")
			return text
		}},
		{harness.Codex, "", func(t *testing.T, args []string) string {
			for i := 0; i+1 < len(args); i++ {
				if args[i] != "-c" {
					continue
				}
				if key, value, err := tomltest.Override(args[i+1]); err == nil && key == "developer_instructions" {
					return value
				}
			}
			return ""
		}},
		{harness.Grok, "", func(t *testing.T, args []string) string {
			text, _ := argValue(args, "--rules")
			return text
		}},
	} {
		t.Run(string(tc.engine), func(t *testing.T) {
			c := Config{Provider: provider(tc.engine), Skills: harness.Skills{Delivery: tc.delivery, Provided: []harness.Skill{skill}}}
			args := capture(t, c, Request{Prompt: "p", WorkDir: t.TempDir(), AppendInstructions: "Be brief."}, nil)
			text := tc.read(t, args)
			if !strings.HasPrefix(text, "Be brief.\n\n") {
				t.Fatalf("caller instructions must lead: %q", text)
			}
			for _, want := range []string{"- demo: Synthetic skill demo.", "SKILL.md: " + manifest, "scripts may be run from " + root} {
				if !strings.Contains(text, want) {
					t.Fatalf("index lacks %q:\n%s", want, text)
				}
			}
		})
	}
}

func TestSkillRequestsANativeRunCannotHonourAreRefused(t *testing.T) {
	skill := writeTestSkill(t, "demo")
	provided := []harness.Skill{skill}
	for name, tc := range map[string]struct {
		c    Config
		code string
	}{
		"unknown global":           {Config{Provider: provider(harness.Codex), Skills: harness.Skills{Global: "some"}}, CodeGlobalSkillsInvalid},
		"unknown delivery":         {Config{Provider: provider(harness.Codex), Skills: harness.Skills{Delivery: "native"}}, CodeSkillDeliveryInvalid},
		"exclude codex":            {Config{Provider: provider(harness.Codex), Skills: harness.Skills{Global: harness.GlobalSkillsExclude}}, CodeGlobalSkillsUnsupported},
		"exclude claude":           {Config{Provider: provider(harness.Claude), Skills: harness.Skills{Global: harness.GlobalSkillsExclude, Provided: provided}}, CodeGlobalSkillsUnsupported},
		"exclude grok":             {Config{Provider: provider(harness.Grok), Skills: harness.Skills{Global: harness.GlobalSkillsExclude}}, CodeGlobalSkillsUnsupported},
		"include without skills":   {Config{Provider: provider(harness.Claude), Args: []string{"--disable-slash-commands"}, Skills: harness.Skills{Global: harness.GlobalSkillsInclude}}, CodeSkillsConflict},
		"include without settings": {Config{Provider: provider(harness.Claude), Args: []string{"--setting-sources=project"}, Skills: harness.Skills{Global: harness.GlobalSkillsInclude}}, CodeSkillsConflict},
		"plugin without Skill":     {Config{Provider: provider(harness.Claude), Args: []string{"--tools=Read"}, Skills: harness.Skills{Provided: provided}}, CodeSkillsConflict},
		"plugin in safe mode":      {Config{Provider: provider(harness.Claude), Args: []string{"--safe-mode"}, Skills: harness.Skills{Provided: provided}}, CodeSkillsConflict},
	} {
		t.Run(name, func(t *testing.T) {
			if facts := refusal(t, tc.c, Request{Prompt: "p"}); facts.Code != tc.code {
				t.Fatalf("code %q, want %q", facts.Code, tc.code)
			}
		})
	}
	// Composed delivery reads skills with the agent's own tools, so a Claude
	// run that removes the Skill tool can still take it.
	composed := Config{Provider: provider(harness.Claude), Args: []string{"--tools=Read"}, Skills: harness.Skills{Delivery: harness.SkillDeliveryComposed, Provided: provided}}
	if err := validate(composed, Request{Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	// Default and Include are each engine's own behaviour.
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok} {
		if err := validate(Config{Provider: provider(engine), Skills: harness.Skills{Global: harness.GlobalSkillsInclude}}, Request{Prompt: "p"}); err != nil {
			t.Errorf("%s include: %v", engine, err)
		}
	}
}

func TestAnUnusableSkillIsRefusedBeforeLaunch(t *testing.T) {
	missing := harness.Skill{Name: "demo", Dir: t.TempDir()}
	c := Config{Provider: provider(harness.Codex), Skills: harness.Skills{Provided: []harness.Skill{missing}}, RunCommand: neverRun(t)}
	_, err := Run(context.Background(), c, Request{Prompt: "p"}, nil)
	facts, ok := harness.ErrorFacts(err)
	if !ok || facts.Family != harness.FailurePreflight || facts.Code != "skill_manifest_missing" {
		t.Fatalf("%+v %v", facts, err)
	}
	if strings.Contains(err.Error(), missing.Dir) {
		t.Fatalf("error names a path: %v", err)
	}
}
