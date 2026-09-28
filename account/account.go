// Package account reports who is logged in to a CLI harness and what their
// usage draws on, in one shape for every engine.
//
// Usage on a harness is paid for by up to three independent systems, and this
// package reports them side by side rather than combining them, because only
// the application knows which one its users care about:
//
//   - Quota: subscription allowance windows, such as a five-hour session
//     window and a weekly window, each a percentage used with a reset time.
//     A percentage is never converted into a token or message count.
//   - Cost: token spend valued at API rates. It belongs to a turn or run, not
//     to an account, so it is reported by the execution modes (harness.Cost)
//     and only when the harness reports it.
//   - Credits: a prepaid or overage balance that can pay for usage beyond a
//     quota window, or instead of one.
//
// Every part of a harness.AccountReport is present and says whether it is
// known. An unknown part is never zero, unlimited or free: its Reason says
// why it is unknown, including when the engine cannot report it at all.
//
// Inspection performs no inference and creates no conversation. It reads the
// selected CLI's own native login, in a disposable working directory.
package account

import (
	"context"
	"errors"
	"os/exec"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/session"
)

// Inspect reads the account, quota and credits of the CLI harness the
// provider selects. It may return a partial report alongside an error; each
// part's Observation says whether it was read. Errors implement
// harness.Factual, except the caller's own context cancellation, which is
// returned as it is.
func Inspect(ctx context.Context, p harness.Provider) (harness.AccountReport, error) {
	report := unknownReport(p.Engine, "not inspected")
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if support := harness.Support(p.Engine, harness.Account, harness.Available); !support.Usable() {
		return report, &Error{Engine: p.Engine, Code: CodeUnsupportedEngine, Family: harness.FailureCapability}
	}
	if code := p.Problem(); code != "" {
		return report, &Error{Engine: p.Engine, Code: code, Family: harness.FailurePreflight}
	}
	if harness.LoginStoreLocked(p.Engine) {
		return report, &Error{Engine: p.Engine, Code: harness.CodeKeychainUnavailable, Family: harness.FailurePreflight}
	}
	binary := p.CLI.Binary
	if binary == "" {
		binary = string(p.Engine)
	}
	if _, err := exec.LookPath(binary); err != nil {
		return report, &Error{Engine: p.Engine, Code: CodeNotInstalled, Family: harness.FailurePreflight}
	}
	switch p.Engine {
	case harness.Grok:
		return inspectGrok(ctx, p, binary)
	case harness.Codex, harness.Claude:
		return inspectSession(ctx, p)
	}
	return report, &Error{Engine: p.Engine, Code: CodeUnsupportedEngine, Family: harness.FailureCapability}
}

// inspectSession reads Codex and Claude through their session protocol, which
// already speaks each harness's account and usage methods.
func inspectSession(ctx context.Context, p harness.Provider) (harness.AccountReport, error) {
	in, err := session.Inspect(ctx, session.Options{Provider: p})
	report := explained(harness.AccountReport{Engine: p.Engine, Account: in.Account, Quota: in.Quota, Credits: in.Credits}, "not reported by the CLI")
	if err == nil {
		return report, nil
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return report, ctx.Err()
	}
	return report, fromSession(p.Engine, err)
}

// unknownReport has every part present and unknown, with a reason.
func unknownReport(e harness.Engine, reason string) harness.AccountReport {
	return explained(harness.AccountReport{Engine: e}, reason)
}

// explained gives every unknown part a reason, so no part is silently empty.
func explained(r harness.AccountReport, reason string) harness.AccountReport {
	for _, o := range []*harness.Observation{&r.Account.Observation, &r.Quota.Observation, &r.Credits.Observation} {
		if !o.Known() && o.Reason == "" {
			o.Reason = reason
		}
	}
	return r
}
