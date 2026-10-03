package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"
)

// Proof is successful pre-launch evidence for one frozen configuration.
// Its fields cannot be fabricated by callers; Linux always re-proves.
type Proof struct {
	system           []string
	binary, identity string
	options          Options
	request          *Options
}

func (p Proof) SystemDirs() []string { return append([]string(nil), p.system...) }
func (p Proof) Binary() string       { return p.binary }

// Identity fingerprints the proved executable. On macOS it is informational;
// the Seatbelt cache key pins the executable. Linux rechecks it before launch.
func (p Proof) Identity() string { return p.identity }

// Prove checks normalized options with disposable canaries before credentialed work.
func Prove(ctx context.Context, o Options) (Proof, error) {
	n, err := normalize(o, true)
	if err != nil {
		return Proof{}, err
	}
	p, err := proveWorkbench(ctx, n)
	if err == nil {
		p = p.withRequest(o)
	}
	return p, err
}

func (p Proof) withRequest(o Options) Proof {
	o.Read, o.Env = slices.Clone(o.Read), slices.Clone(o.Env)
	p.request = &o
	return p
}

// Runner contains command trees for a proved configuration and caller-owned state.
// The caller owns admission, directory validation, settlement and the state lock.
type Runner struct {
	execute func(context.Context, string, string, time.Duration, func()) (CommandResult, error)
	close   func() error
	timeout time.Duration
}
type commandSandbox = Runner
type commandConfig struct {
	options      Options
	proof        Proof
	stateDir     string
	outputBudget int
	standalone   bool
}

// NewRunner prepares durable recovery state. A zero outputBudget selects the
// standalone per-stream bound; hosted callers pass their tool result budget.
// Options must match the original Prove request or its frozen normalized value.
// Preparation does not repeat option normalization or system discovery.
func NewRunner(o Options, p Proof, stateDir string, outputBudget int) (*Runner, error) {
	// Use only the configuration frozen by the proof. Matching either the
	// original request or its normalized value permits public Prove callers
	// to retain their input without rerunning system discovery after proof.
	candidate := o
	candidate.system = p.options.system
	matches := reflect.DeepEqual(candidate, p.options)
	if p.request != nil {
		matches = matches || reflect.DeepEqual(o, *p.request)
	}
	if len(p.system) == 0 || !matches {
		return nil, stateError(StateUnusable)
	}
	n := p.options
	if outputBudget != 0 && (outputBudget < MinResult || outputBudget > MaxResult) {
		return nil, refusal("commands", RefusedLimit, "command output budget must be zero or between 4096 and 65536 bytes")
	}
	state, err := filepath.EvalSymlinks(stateDir)
	if err != nil || !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir || !Nested(n.RuntimeHome, state) {
		return nil, stateError(StateUnusable)
	}
	if info, err := os.Stat(state); err != nil || !info.IsDir() || !privateStateDir(info) {
		return nil, stateError(StateUnusable)
	}
	return newCommandSandbox(commandConfig{options: n, proof: p, stateDir: state, outputBudget: outputBudget, standalone: outputBudget == 0})
}

// Execute runs a command in a validated relative workspace directory. onStart
// reports launch only. Zero timeout waits for settlement until cancellation.
func (r *Runner) Execute(ctx context.Context, command, rel string, timeout time.Duration, onStart func()) (CommandResult, error) {
	return r.execute(ctx, command, rel, timeout, onStart)
}
func (r *Runner) Close() error           { return r.close() }
func (r *Runner) Timeout() time.Duration { return r.timeout }
