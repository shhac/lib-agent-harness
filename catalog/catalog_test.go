package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
)

// requireFailure checks a discovery failure's typed facts and that nothing
// secret, from a provider, a child or a credential, reached any of its forms.
func requireFailure(t *testing.T, err error, engine harness.Engine, family harness.Family, code string) *Error {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("want *Error %s, got %T %v", code, err, err)
	}
	facts, ok := harness.ErrorFacts(err)
	if !ok || facts.Operation != harness.Models || facts.Engine != engine || facts.Family != family || facts.Code != code {
		t.Fatalf("want %s/%s/%s; got %+v", engine, family, code, facts)
	}
	encoded, _ := json.Marshal(facts)
	if strings.Contains(string(encoded)+fmt.Sprintf("%#v", failure)+err.Error(), "secret") {
		t.Fatalf("failure retained provider text or credential: %v", err)
	}
	return failure
}

// fakeRunner stands in for the subprocess and records what would have run.
func fakeRunner(output string, record *invocation, written *strings.Builder) cliRunner {
	return func(ctx context.Context, command invocation, talk exchange) error {
		if record != nil {
			*record = command
		}
		var sink io.Writer = io.Discard
		if written != nil {
			sink = written
		}
		return talk(strings.NewReader(output), sink)
	}
}

func testDiscoverer(run cliRunner) discoverer {
	return discoverer{timeout: defaultTimeout, run: run}
}

func TestDiscoveryRefusesUnsupportedEnginesBeforeAnyWork(t *testing.T) {
	d := testDiscoverer(func(context.Context, invocation, exchange) error {
		t.Fatal("an unsupported engine reached the transport")
		return nil
	})
	_, err := d.discover(context.Background(), harness.Provider{Engine: "secret-engine"})
	requireFailure(t, err, "", harness.FailureCapability, "unsupported_engine")
}

// A provider that sets the half its engine does not read is refused rather
// than half-read, before any catalog subprocess starts.
func TestDiscoveryRefusesAMalformedProvider(t *testing.T) {
	d := testDiscoverer(func(context.Context, invocation, exchange) error {
		t.Fatal("malformed provider reached the catalog transport")
		return nil
	})
	p := cliProvider(harness.Codex, "chosen-codex", "")
	p.API.BaseURL = "https://gateway.invalid/v1"
	_, err := d.discover(context.Background(), p)
	requireFailure(t, err, harness.Codex, harness.FailurePreflight, "api_config_for_cli_engine")
}

func TestDiscoveryBoundsTimeAndSanitizesChildErrors(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok} {
		called := false
		d := testDiscoverer(func(ctx context.Context, command invocation, _ exchange) error {
			called = true
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > defaultTimeout {
				t.Fatal("missing discovery deadline")
			}
			return errors.New("secret-token from subprocess")
		})
		_, err := d.discover(context.Background(), cliProvider(engine, testBinary(t), t.TempDir()))
		if !called {
			t.Fatalf("%s never ran", engine)
		}
		requireFailure(t, err, engine, harness.FailureProcess, "process_failed")
	}
}

func TestDiscoveryReportsAnExitStatus(t *testing.T) {
	exit := exec.Command(testBinary(t), "-test.run=^$", "-test.count=1", "-secret-unknown-flag").Run()
	d := testDiscoverer(func(context.Context, invocation, exchange) error { return exit })
	_, err := d.discover(context.Background(), cliProvider(harness.Grok, testBinary(t), t.TempDir()))
	failure := requireFailure(t, err, harness.Grok, harness.FailureProcess, "process_exited")
	if failure.ExitCode == nil || *failure.ExitCode == 0 {
		t.Fatalf("%+v", failure)
	}
}

func TestDiscoveryPreservesCancellation(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok} {
		ctx, cancel := context.WithCancel(context.Background())
		d := testDiscoverer(func(context.Context, invocation, exchange) error {
			cancel()
			return errors.New("synthetic transport error")
		})
		_, err := d.discover(ctx, cliProvider(engine, testBinary(t), t.TempDir()))
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s lost cancellation: %v", engine, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := testDiscoverer(func(context.Context, invocation, exchange) error {
		t.Fatal("a cancelled discovery started")
		return nil
	})
	if _, err := d.discover(ctx, cliProvider(harness.Grok, "", "")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// A caller's own deadline is the caller's: it comes back as ctx.Err(), while
// discovery's internal bound is a typed timeout.
func TestDiscoveryDistinguishesTheCallersDeadline(t *testing.T) {
	block := func(ctx context.Context, _ invocation, _ exchange) error { <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	d := discoverer{timeout: time.Minute, run: block}
	if _, err := d.discover(ctx, cliProvider(harness.Codex, testBinary(t), t.TempDir())); !errors.Is(err, context.DeadlineExceeded) || errors.As(err, new(*Error)) {
		t.Fatalf("caller deadline: %v", err)
	}
	d = discoverer{timeout: 50 * time.Millisecond, run: block}
	_, err := d.discover(context.Background(), cliProvider(harness.Codex, testBinary(t), t.TempDir()))
	requireFailure(t, err, harness.Codex, harness.FailureProcess, "deadline_exceeded")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("a typed timeout must still match context.DeadlineExceeded")
	}
}

func TestDiscoveryRefusesAMissingExecutable(t *testing.T) {
	for _, engine := range []harness.Engine{harness.Codex, harness.Claude, harness.Grok} {
		_, err := Discover(context.Background(), cliProvider(engine, "/secret/missing/"+string(engine), t.TempDir()))
		requireFailure(t, err, engine, harness.FailurePreflight, "executable_not_found")
	}
}

func TestDiscoveryRefusesARelativeHome(t *testing.T) {
	for engine, code := range map[harness.Engine]string{harness.Codex: "codex_home_invalid", harness.Claude: "claude_home_invalid", harness.Grok: "grok_home_invalid"} {
		_, err := Discover(context.Background(), cliProvider(engine, testBinary(t), "secret-relative"))
		requireFailure(t, err, engine, harness.FailurePreflight, code)
	}
}

func TestCatalogSizeIsBounded(t *testing.T) {
	catalog := newCatalogBuilder()
	for i := range maxModels {
		if err := catalog.add(Model{ID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalog.add(Model{ID: fmt.Sprint(maxModels - 1)}); err != nil {
		t.Fatal("a duplicate counted against the limit")
	}
	if err := catalog.add(Model{ID: "one-too-many"}); err == nil {
		t.Fatal("catalog grew past its limit")
	}
}

func TestOutputIsBoundedWithoutTruncating(t *testing.T) {
	reader := &boundedReader{r: strings.NewReader(strings.Repeat("x", 10)), n: 4}
	data, err := io.ReadAll(reader)
	if !errors.Is(err, errOutputLimit) || len(data) != 4 {
		t.Fatalf("%q %v", data, err)
	}
	exact := &boundedReader{r: strings.NewReader("abcd"), n: 4}
	if data, err := io.ReadAll(exact); err != nil || string(data) != "abcd" {
		t.Fatalf("%q %v", data, err)
	}
}
