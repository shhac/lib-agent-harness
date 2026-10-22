package conformance_test

import (
	"context"
	"flag"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/cassette"
)

var record = flag.String("record", "", "engines to record against the installed, signed-in CLI, comma separated (spends the operator's own login)")

func init() { cassette.Main() }

// engine is a CLI whose wire is recorded.
type engine struct {
	name       string // the CLI's name, and its directory under testdata
	engine     harness.Engine
	redactions cassette.Redactions
}

func (e engine) recording() bool { return slices.Contains(strings.Split(*record, ","), e.name) }

// scenario runs body against every recorded version of the engine's CLI, or,
// recording, against the installed CLI, whose recordings it replaces and then
// replays, so a placeholder or redaction that changes what the library reads
// fails while the CLI is at hand. body asserts the same things either way,
// and never the model's wording.
func scenario(t *testing.T, e engine, name string, body func(t *testing.T, p harness.Provider, tape *cassette.Tape)) {
	t.Helper()
	if e.recording() {
		real, err := exec.LookPath(e.name)
		if err != nil {
			t.Fatalf("recording %s needs it installed: %v", e.name, err)
		}
		version := installedVersion(t, real)
		t.Logf("recording %s %s against the operator's own login", e.name, version)
		dir := filepath.Join("testdata", e.name, version, name)
		tape := cassette.Start(t, cassette.Config{CLI: e.name, Dir: dir, Record: true, Real: real, Version: version, Redactions: e.redactions})
		body(t, provider(e, tape), tape)
		if !t.Failed() {
			replay(t, e, dir, version, body)
		}
		return
	}
	dirs, _ := filepath.Glob(filepath.Join("testdata", e.name, "*", name))
	if len(dirs) == 0 {
		t.Fatalf("nothing recorded for %s; record it with: go test ./conformance -run '%s' -record=%s", name, t.Name(), e.name)
	}
	for _, dir := range dirs {
		replay(t, e, dir, filepath.Base(filepath.Dir(dir)), body)
	}
}

func replay(t *testing.T, e engine, dir, version string, body func(t *testing.T, p harness.Provider, tape *cassette.Tape)) {
	t.Run(version, func(t *testing.T) {
		tape := cassette.Start(t, cassette.Config{CLI: e.name, Dir: dir, Version: version})
		body(t, provider(e, tape), tape)
	})
}

func provider(e engine, tape *cassette.Tape) harness.Provider {
	return harness.Provider{Engine: e.engine, CLI: harness.CLI{Binary: tape.Binary()}}
}

// installedVersion is the last word of `<cli> --version`, which names the
// directory a recording is kept in.
func installedVersion(t *testing.T, real string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, real, "--version").Output()
	fields := strings.Fields(string(out))
	if err != nil || len(fields) == 0 {
		t.Fatalf("%s --version: %v %q", real, err, out)
	}
	return fields[len(fields)-1]
}

func testContext(t *testing.T) context.Context {
	timeout := 30 * time.Second
	if *record != "" {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}
