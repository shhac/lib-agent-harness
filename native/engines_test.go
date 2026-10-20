package native

import (
	"errors"
	"path/filepath"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// Every engine that offers native runs has a complete entry, and nothing
// else has one.
func TestNativeEngineTableIsComplete(t *testing.T) {
	for _, e := range harness.Engines() {
		entry, ok := nativeEngines[e]
		offered := harness.Support(e, harness.Run, harness.Available).Usable()
		if ok != offered {
			t.Errorf("%s: in the table %v, offers runs %v", e, ok, offered)
		}
		if ok && (entry.transcoder == nil || entry.args == nil || entry.optionsSet == nil) {
			t.Errorf("%s: incomplete entry", e)
		}
	}
}

// A home that a CLI cannot be moved to is refused, and which code says so
// is part of the contract: Antigravity's is checked before its options and
// keeps its own code, fx's is checked with its options and reads as
// CodeHomeUnsupported, and Version reports either engine's own code.
func TestFixedHomeRefusalCodes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	other := filepath.Join(home, "elsewhere")
	for _, c := range []struct {
		engine       harness.Engine
		run, version string
	}{
		{harness.Antigravity, "antigravity_home_unsupported", "antigravity_home_unsupported"},
		{harness.Fx, CodeHomeUnsupported, "fx_home_unsupported"},
	} {
		config := Config{Provider: harness.Provider{Engine: c.engine, CLI: harness.CLI{Home: other}}}
		if err := validate(config, Request{Prompt: "hi", WorkDir: home}); err == nil || err.Code != c.run {
			t.Errorf("%s run: %v", c.engine, err)
		}
		_, err := Version(t.Context(), config)
		var failure *RunError
		if !errors.As(err, &failure) || failure.Code != c.version {
			t.Errorf("%s version: %v", c.engine, err)
		}
	}
}
