package account

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/process"
)

// grokTimeout bounds the whole probe: start, handshake and one request.
var grokTimeout = 30 * time.Second

const (
	grokSubscriptionMethod = "_x.ai/auth/check_subscription"
	// grokFrameLimit bounds one JSON-RPC line. The handshake advertises the
	// agent's capabilities, which is the largest frame the probe reads.
	grokFrameLimit = 4 << 20
)

// inspectGrok asks Grok's agent protocol for the login's subscription. It
// initializes the protocol and sends one extension request; it never creates
// a session, so nothing is sent to a model. Grok states no quota windows or
// credit balance, so those parts are unknown, never zero.
func inspectGrok(ctx context.Context, p harness.Provider, binary string) (harness.AccountReport, error) {
	report := unknownReport(p.Engine, "not inspected")
	report.Quota.Reason = "Grok exposes no subscription quota windows"
	report.Credits.Reason = "Grok exposes no credit balance"
	// The same allowlisted environment as every other Grok launch: nothing
	// from the parent but its operating context, so no ambient key reaches it.
	env, code := nativecli.GrokEnvironment(p.CLI.Home)
	if code != "" {
		return report, &Error{Engine: p.Engine, Code: code, Family: harness.FailurePreflight}
	}
	dir, err := os.MkdirTemp("", "agent-harness-account-")
	if err != nil {
		return report, &Error{Engine: p.Engine, Code: CodeTransport, Family: harness.FailureProcess}
	}
	defer os.RemoveAll(dir)
	raw, err := grokExchange(ctx, binary, dir, env)
	if err != nil {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		return report, grokError(err)
	}
	account, err := parseGrokSubscription(raw)
	if err != nil {
		return report, grokError(err)
	}
	report.Account = account
	return report, nil
}

// grokExchange runs the contained agent for one handshake and one request,
// and returns only after the process is gone.
func grokExchange(ctx context.Context, binary, dir string, env []string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, grokTimeout)
	defer cancel()
	cmd, proc, err := process.Command(ctx, binary, "agent", "--no-leader", "stdio")
	if err != nil {
		return nil, errGrokStart
	}
	defer proc.Close()
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errGrokStart
	}
	reader, writer := io.Pipe()
	cmd.Stdout = writer
	reaped := make(chan error, 1)
	go func() {
		err := proc.Run()
		writer.CloseWithError(io.EOF)
		reaped <- err
	}()
	type outcome struct {
		raw json.RawMessage
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		raw, err := grokConverse(stdin, reader)
		done <- outcome{raw, err}
	}()
	var out outcome
	select {
	case out = <-done:
	case <-ctx.Done():
	}
	// A deadline or cancellation also ends the process, so the exchange may
	// report the ending first; the context is the cause.
	if err := ctx.Err(); err != nil && (out.err != nil || out.raw == nil) {
		out = outcome{err: err}
	}
	cancel()
	stdin.Close()
	reader.Close()
	exit := <-reaped
	if out.err != nil && errors.Is(out.err, errGrokEnded) {
		return nil, grokExit(exit)
	}
	return out.raw, out.err
}

// grokConverse initializes the agent protocol, then asks for the
// subscription. Notifications and requests from the agent are skipped.
func grokConverse(w io.Writer, r io.Reader) (json.RawMessage, error) {
	lines := bufio.NewScanner(r)
	lines.Buffer(make([]byte, 0, 64<<10), grokFrameLimit)
	out := json.NewEncoder(w)
	if out.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}}) != nil {
		return nil, errGrokEnded
	}
	if _, err := grokAwait(lines, 1); err != nil {
		return nil, err
	}
	if out.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": grokSubscriptionMethod, "params": map[string]any{}}) != nil {
		return nil, errGrokEnded
	}
	return grokAwait(lines, 2)
}

func grokAwait(lines *bufio.Scanner, id int) (json.RawMessage, error) {
	want := strconv.Itoa(id)
	for lines.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code *int `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(lines.Bytes(), &m) != nil {
			return nil, errGrokInvalid
		}
		if m.Method != "" || string(m.ID) != want {
			continue
		}
		if m.Error != nil {
			if m.Error.Code != nil && *m.Error.Code == -32601 {
				return nil, errGrokMethod
			}
			return nil, errGrokRejected
		}
		if len(m.Result) == 0 {
			return nil, errGrokInvalid
		}
		return m.Result, nil
	}
	if errors.Is(lines.Err(), bufio.ErrTooLong) {
		return nil, errGrokInvalid
	}
	return nil, errGrokEnded
}

func parseGrokSubscription(raw json.RawMessage) (harness.AccountSnapshot, error) {
	var r struct {
		Authenticated *bool `json:"authenticated"`
		Meta          *struct {
			Email    string  `json:"email"`
			AuthMode string  `json:"auth_mode"`
			Tier     string  `json:"subscription_tier"`
			Team     *string `json:"team_name"`
		} `json:"meta"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Authenticated == nil {
		return harness.AccountSnapshot{}, errGrokInvalid
	}
	a := harness.AccountSnapshot{Observation: harness.Observation{Quality: harness.Measured, ObservedAt: time.Now().UTC(), Source: grokSubscriptionMethod}, LoggedIn: r.Authenticated}
	if !*r.Authenticated || r.Meta == nil {
		return a, nil
	}
	a.Email, a.AuthMethod, a.Plan = r.Meta.Email, r.Meta.AuthMode, r.Meta.Tier
	if r.Meta.Team != nil {
		a.Organization = *r.Meta.Team
	}
	return a, nil
}

var (
	errGrokStart    = errors.New("grok could not start")
	errGrokEnded    = errors.New("grok ended the exchange")
	errGrokInvalid  = errors.New("grok answered in an unrecognized shape")
	errGrokMethod   = errors.New("grok does not offer the method")
	errGrokRejected = errors.New("grok refused the request")
)

// grokExit reports why the agent ended the exchange early.
func grokExit(err error) error {
	var status *exec.ExitError
	switch {
	case errors.As(err, &status) && status.ExitCode() >= 0:
		code := status.ExitCode()
		return &Error{Engine: harness.Grok, Code: CodeProcessExited, Family: harness.FailureProcess, ExitCode: &code}
	case errors.As(err, &status):
		return &Error{Engine: harness.Grok, Code: CodeProcessSignalled, Family: harness.FailureProcess}
	case err != nil:
		return &Error{Engine: harness.Grok, Code: CodeProcessStartFailed, Family: harness.FailureProcess}
	}
	return &Error{Engine: harness.Grok, Code: CodeProcessExited, Family: harness.FailureProcess}
}

func grokError(err error) error {
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	e := &Error{Engine: harness.Grok, Family: harness.FailureRequest}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		e.Code, e.Cause = CodeTimedOut, harness.CauseTimeout
	case errors.Is(err, errGrokMethod):
		e.Code, e.Family = CodeMethodUnavailable, harness.FailureCapability
	case errors.Is(err, errGrokRejected):
		e.Code, e.Cause = CodeRejected, harness.CauseUnknown
	case errors.Is(err, errGrokStart):
		e.Code, e.Family = CodeProcessStartFailed, harness.FailureProcess
	default:
		e.Code = CodeInvalidResponse
	}
	return e
}
