package native

import (
	"reflect"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestCodexBrowserStartAndResume(t *testing.T) {
	for _, resume := range []string{"", "thread-1"} {
		c := Config{Provider: provider(harness.Codex), Codex: CodexOptions{Sandbox: "read-only"}}
		r := Request{Prompt: "inspect the form", ResumeSession: resume}
		plain := codexArgs(c, r, nil)
		c.Browser = true
		if err := validate(c, r); err != nil {
			t.Fatal(err)
		}
		args := codexArgs(c, r, nil)
		want := append([]string{}, plain[:len(plain)-2]...)
		if resume != "" {
			want = append([]string{}, plain[:len(plain)-3]...)
		}
		want = append(want, "-c", "features.browser_use=true", "-c", "features.browser_use_external=true", "--")
		if resume != "" {
			want = append(want, resume)
		}
		want = append(want, r.Prompt)
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("resume %q: %q, want %q", resume, args, want)
		}
	}
}

func TestCodexBrowserOverridesOnlyManagedWhenRequested(t *testing.T) {
	for _, args := range [][]string{
		{"-c", "features.browser_use=false"},
		{"-cfeatures.browser_use_external=false"},
		{"--config=features.browser_use_external=false"},
		{"--config", `"features" . 'browser_use' = false`},
		{"-c", "features={browser_use=false}"},
		{"--disable", "browser_use"},
		{"--enable=computer_use,browser_use_external"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c := Config{Provider: provider(harness.Codex), Args: args}
			if err := validate(c, Request{}); err != nil {
				t.Fatalf("ordinary overrides changed: %v", err)
			}
			c.Browser = true
			if err := validate(c, Request{}); err == nil || err.Code != CodeManagedFlagInArgs {
				t.Fatalf("conflicting browser override: %v", err)
			}
		})
	}
	c := Config{Provider: provider(harness.Codex), Browser: true, Args: []string{"--disable=computer_use", "-c", "features.multi_agent=false"}}
	if err := validate(c, Request{}); err != nil {
		t.Fatalf("unrelated feature configuration: %v", err)
	}
}
