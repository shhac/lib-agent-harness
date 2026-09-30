package session

import (
	"errors"
	"reflect"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestCodexBrowserArgsAndReference(t *testing.T) {
	base := Options{Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Home: t.TempDir()}}, WorkDir: t.TempDir()}
	o, err := normalize(base)
	if err != nil {
		t.Fatal(err)
	}
	plain := reference(o, "thread-1")
	if args := commandArgs(o, plain.ID, false, nil); !reflect.DeepEqual(args, []string{"app-server", "--listen", "stdio://"}) {
		t.Fatalf("ordinary launch changed: %q", args)
	}
	base.Browser = true
	o, err = normalize(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, resume := range []bool{false, true} {
		args := commandArgs(o, plain.ID, resume, nil)
		want := []string{"app-server", "--listen", "stdio://", "-c", "features.browser_use=true", "-c", "features.browser_use_external=true"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("resume %t: %q", resume, args)
		}
	}
	if reference(o, plain.ID).ConfigHash == plain.ConfigHash {
		t.Fatal("browser opt-in did not change the resume contract")
	}
	for _, restricted := range []bool{false, true} {
		limited := base
		if restricted {
			limited.Restriction = &Restriction{}
		} else {
			limited.Sandbox = &Sandbox{}
		}
		_, err := normalize(limited)
		var refusal *UnsupportedError
		if !errors.As(err, &refusal) || refusal.Code != RefusedConflict {
			t.Fatalf("restricted %t: %v", restricted, err)
		}
	}
}
