package catalog

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/restrict"
	"github.com/shhac/lib-agent-harness/process"
)

const (
	outputLimit = 4 << 20
	lineLimit   = 1 << 20
)

// invocation is the one command a catalog read runs.
type invocation struct {
	bin  string
	args []string
	env  []string
}

type exchange func(io.Reader, io.Writer) error

type cliRunner func(context.Context, invocation, exchange) error

type catalogReader func(io.Reader, io.Writer) ([]Model, error)

// cliEngine is how one engine's catalog is read.
type cliEngine struct {
	args        func() []string
	environment func(home string) ([]string, string)
	read        catalogReader
}

var cliEngines = map[harness.Engine]cliEngine{
	harness.Codex: {codexArgs, nativecli.CodexEnvironment, readCodexCatalog},
	harness.Claude: {func() []string {
		return append(nativecli.ClaudeRestrictedArgs(), "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	}, nativecli.ClaudeEnvironment, readClaudeCatalog},
	// --no-leader keeps this process from serving other Grok clients.
	harness.Grok: {func() []string { return []string{"agent", "--no-leader", "stdio"} }, nativecli.GrokEnvironment, readGrokCatalog},
}

// codexArgs creates no thread, and disables tool-discovery features during
// startup.
func codexArgs() []string {
	args := []string{"app-server", "--listen", "stdio://", "-c", "analytics.enabled=false", "-c", "check_for_update_on_startup=false"}
	for _, feature := range restrict.CodexFeatures {
		args = append(args, "-c", "features."+feature+"=false")
	}
	return args
}

// cliCatalog builds only the selected engine's invocation, so discovering
// Claude's models never resolves a Codex home.
func cliCatalog(p harness.Provider) (invocation, catalogReader, error) {
	engine, ok := cliEngines[p.Engine]
	if !ok {
		return invocation{}, nil, refusal(p.Engine, "unsupported_engine")
	}
	bin, err := resolveBinary(p.CLI.Binary, p.Engine)
	if err != nil {
		return invocation{}, nil, err
	}
	env, code := engine.environment(p.CLI.Home)
	if code != "" {
		return invocation{}, nil, preflightFailure(code)
	}
	return invocation{bin: bin, args: engine.args(), env: env}, engine.read, nil
}

func resolveBinary(configured string, engine harness.Engine) (string, error) {
	bin := configured
	if bin == "" {
		bin = string(engine)
	}
	bin, err := exec.LookPath(bin)
	if errors.Is(err, os.ErrPermission) {
		return "", preflightFailure("executable_permission")
	}
	if err != nil {
		return "", preflightFailure("executable_not_found")
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return "", preflightFailure("executable_unresolved")
	}
	return bin, nil
}

// runCLI runs the invocation in an empty private directory with stderr
// discarded, and stops the whole process tree as soon as the exchange ends.
func runCLI(ctx context.Context, command invocation, talk exchange) error {
	dir, err := os.MkdirTemp("", "agent-harness-model-catalog-")
	if err != nil {
		return preflightFailure("scratch_directory")
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, child, err := process.Command(ctx, command.bin, command.args...)
	if err != nil {
		return &Error{Family: harness.FailureProcess, Cause: harness.CauseUnknown, Phase: PhaseProcess, Code: "process_start_failed"}
	}
	defer child.Close()
	cmd.Dir, cmd.Env, cmd.Stderr = dir, command.env, io.Discard
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdinReader.Close()
	defer stdinWriter.Close()
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	stopClosing := context.AfterFunc(ctx, func() {
		stdinWriter.CloseWithError(ctx.Err())
		stdoutReader.CloseWithError(ctx.Err())
	})
	defer stopClosing()
	cmd.Stdin, cmd.Stdout = stdinReader, stdoutWriter
	cmd.WaitDelay = time.Second
	done := make(chan error, 1)
	go func() {
		err := child.Run()
		stdoutWriter.CloseWithError(err)
		stdinReader.CloseWithError(err)
		done <- err
	}()
	err = talk(stdoutReader, stdinWriter)
	cancel()
	stdinWriter.Close()
	stdoutReader.Close()
	<-done
	return err
}

var errOutputLimit = errors.New("catalog output exceeds limit")

// boundedReader fails, rather than ending, once more than n bytes arrive, so
// a truncated stream is never mistaken for a complete one. It remembers the
// first failure so that a partial last line is never read as a message.
type boundedReader struct {
	r      io.Reader
	n      int64
	failed error
}

func (b *boundedReader) Read(p []byte) (int, error) {
	n, err := b.read(p)
	if err != nil && err != io.EOF && b.failed == nil {
		b.failed = err
	}
	return n, err
}

func (b *boundedReader) read(p []byte) (int, error) {
	if b.n <= 0 {
		var probe [1]byte
		n, err := b.r.Read(probe[:])
		if n > 0 {
			return 0, errOutputLimit
		}
		return 0, err
	}
	if int64(len(p)) > b.n {
		p = p[:b.n]
	}
	n, err := b.r.Read(p)
	b.n -= int64(n)
	return n, err
}

// lines scans newline-delimited messages within the output and line limits,
// skipping blank lines. A stream that fails mid-line ends with that failure
// rather than yielding the fragment.
func lines(reader io.Reader) *bufio.Scanner {
	bounded := &boundedReader{r: reader, n: outputLimit}
	scanner := bufio.NewScanner(bounded)
	scanner.Buffer(make([]byte, 4096), lineLimit)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && bounded.failed != nil {
			return 0, nil, bounded.failed
		}
		advance, token, err := bufio.ScanLines(data, atEOF)
		if token != nil && len(bytes.TrimSpace(token)) == 0 {
			return advance, nil, err
		}
		return advance, token, err
	})
	return scanner
}

// endOfStream explains a scan that ended before the awaited reply. A pipe
// error (the child's exit, or cancellation) is returned for classify.
func endOfStream(scanner *bufio.Scanner) error {
	err := scanner.Err()
	switch {
	case err == nil:
		return responseFailure("missing_response")
	case errors.Is(err, errOutputLimit), errors.Is(err, bufio.ErrTooLong):
		return responseFailure("output_limit")
	}
	return err
}
