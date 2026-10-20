package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/process"
)

// fxTimeout bounds the whole inspection: each command is a short, local read.
var fxTimeout = 30 * time.Second

// fxCommandLimit bounds one fx command's output. The status and credits
// documents are small; a larger one is refused rather than truncated.
const fxCommandLimit = 1 << 20

// inspectFx reads fx's login, credit balance and spend.
//
// fx states its account in its own commands, each of which performs no
// inference and creates no conversation: `status` says which sign-in is
// active, `credits` says the AI Gateway balance, and `usage` says the spend
// for a period. There are no subscription allowance windows anywhere in that
// output, so quota stays unknown rather than being read as a percentage.
//
// A balance that cannot be read leaves credits unknown with a reason; it never
// becomes zero, which would read as an empty balance rather than a missing one.
func inspectFx(ctx context.Context, p harness.Provider, binary string) (harness.AccountReport, error) {
	report := unknownReport(p.Engine, "not inspected")
	report.Quota.Reason = "fx reports spend for a period, not subscription allowance windows"
	env, code := nativecli.FxEnvironment(p.CLI.Home)
	if code != "" {
		return report, &Error{Engine: p.Engine, Code: code, Family: harness.FailurePreflight}
	}
	dir, err := os.MkdirTemp("", "agent-harness-account-")
	if err != nil {
		return report, &Error{Engine: p.Engine, Code: CodeTransport, Family: harness.FailureProcess}
	}
	defer os.RemoveAll(dir)

	status, err := fxCommand(ctx, binary, dir, env, "status", "--json")
	if err != nil {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		return report, fxError(err)
	}
	account, err := parseFxStatus(status)
	if err != nil {
		return report, fxError(err)
	}
	report.Account = account

	credits, err := fxCommand(ctx, binary, dir, env, "credits", "--json")
	if err != nil {
		report.Credits.Reason = "fx did not state a credit balance"
		return report, nil
	}
	snapshot, err := parseFxCredits(credits)
	if err != nil {
		report.Credits.Reason = "fx stated a credit balance in an unreadable shape"
		return report, nil
	}
	report.Credits = snapshot
	return report, nil
}

// fxCommand runs one fx command and returns its standard output. Diagnostics
// go to standard error, which is discarded: only fx's own document is read.
func fxCommand(ctx context.Context, binary, dir string, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fxTimeout)
	defer cancel()
	cmd, proc, err := process.Command(ctx, binary, args...)
	if err != nil {
		return nil, errFxStart
	}
	defer proc.Close()
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stderr = io.Discard
	var out fxOutput
	out.limit = fxCommandLimit
	cmd.Stdout = &out
	cmd.WaitDelay = 2 * time.Second
	if err := proc.Run(); err != nil {
		return nil, err
	}
	if out.total > fxCommandLimit {
		return nil, errFxInvalid
	}
	return out.buf.Bytes(), nil
}

// fxOutput keeps a bounded prefix of a command's output and counts the whole
// of it, so a command that writes more than the limit is known to have done
// so.
type fxOutput struct {
	buf   bytes.Buffer
	limit int
	total int
}

func (o *fxOutput) Write(p []byte) (int, error) {
	o.total += len(p)
	if remaining := o.limit - o.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		o.buf.Write(p)
	}
	return len(p), nil
}

// parseFxStatus reads fx's status document. The sign-in is named by "auth";
// an empty value means no sign-in is active, which is stated, not guessed.
func parseFxStatus(raw []byte) (harness.AccountSnapshot, error) {
	var r struct {
		Kind     string `json:"kind"`
		Auth     string `json:"auth"`
		Team     string `json:"team"`
		Provider string `json:"provider"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Kind != "status" {
		return harness.AccountSnapshot{}, errFxInvalid
	}
	loggedIn := r.Auth != ""
	snapshot := harness.AccountSnapshot{
		Observation:  harness.Observation{Quality: harness.Measured, ObservedAt: time.Now().UTC(), Source: "fx status"},
		LoggedIn:     &loggedIn,
		AuthMethod:   r.Auth,
		Organization: r.Team,
	}
	return snapshot, nil
}

// parseFxCredits reads fx's balance document. The balance is the AI Gateway
// balance for the active credential, in dollars; it is kept as the provider
// stated it, never as a float.
func parseFxCredits(raw []byte) (harness.CreditSnapshot, error) {
	var r struct {
		Kind    string  `json:"kind"`
		Balance *string `json:"balance"`
		Used    *string `json:"used"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Kind != "credits" {
		return harness.CreditSnapshot{}, errFxInvalid
	}
	snapshot := harness.CreditSnapshot{
		Observation: harness.Observation{Quality: harness.Measured, ObservedAt: time.Now().UTC(), Source: "fx credits"},
	}
	if r.Balance != nil && *r.Balance != "" {
		snapshot.Balance = &harness.Amount{Value: *r.Balance, Unit: "USD"}
	}
	if r.Used != nil && *r.Used != "" {
		snapshot.Used = &harness.Amount{Value: *r.Used, Unit: "USD"}
	}
	return snapshot, nil
}

var (
	errFxStart   = errors.New("fx could not start")
	errFxInvalid = errors.New("fx answered in an unrecognized shape")
)

// fxError names why an fx inspection failed, in the account package's own
// vocabulary.
func fxError(err error) *Error {
	e := &Error{Engine: harness.Fx, Family: harness.FailureRequest}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		e.Code, e.Cause = CodeTimedOut, harness.CauseTimeout
	case errors.Is(err, errFxStart):
		e.Code, e.Family = CodeProcessStartFailed, harness.FailureProcess
	case errors.Is(err, errFxInvalid):
		e.Code = CodeInvalidResponse
	default:
		var status *exec.ExitError
		switch {
		case errors.As(err, &status) && status.ExitCode() >= 0:
			code := status.ExitCode()
			e.Code, e.Family, e.ExitCode = CodeProcessExited, harness.FailureProcess, &code
		case errors.As(err, &status):
			e.Code, e.Family = CodeProcessSignalled, harness.FailureProcess
		default:
			e.Code, e.Family = CodeTransport, harness.FailureProcess
		}
	}
	return e
}
