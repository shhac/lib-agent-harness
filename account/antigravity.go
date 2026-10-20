package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/process"
)

// antigravityTimeout bounds the whole inspection across /credits and /usage.
var antigravityTimeout = 30 * time.Second

const antigravityOutputLimit = 4 << 20

// inspectAntigravity runs `agy -p=/credits --output-format json` and
// `agy -p=/usage --output-format json` in a disposable directory. Neither
// slash command starts a model turn or creates a conversation. Some logins
// have no credits (agy 1.4.2: "no credits info found"); their quota still
// stands, and /usage then shows the login.
func inspectAntigravity(ctx context.Context, p harness.Provider, binary string) (harness.AccountReport, error) {
	report := unknownReport(p.Engine, "not inspected")
	env, code := nativecli.AntigravityEnvironment(p.CLI.Home)
	if code != "" {
		family := harness.FailurePreflight
		if code == "antigravity_home_unsupported" {
			family = harness.FailureCapability
		}
		return report, &Error{Engine: p.Engine, Code: code, Family: family}
	}
	dir, err := os.MkdirTemp("", "agent-harness-account-")
	if err != nil {
		return report, &Error{Engine: p.Engine, Code: CodeTransport, Family: harness.FailureProcess}
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(ctx, antigravityTimeout)
	defer cancel()

	creditsRaw, err := antigravityCommand(ctx, binary, dir, env, "/credits")
	if errors.Is(err, errAntigravityLoggedOut) {
		return antigravityLoggedOut(report), nil
	}
	if err != nil {
		return report, antigravityCommandError(ctx, err)
	}
	account, credits, loggedIn, err := parseAntigravityCredits(creditsRaw)
	creditsRefused := errors.Is(err, errAntigravityRejected)
	switch {
	case creditsRefused:
		report.Credits.Reason = "the CLI reported no credits for this login"
	case err != nil:
		return report, antigravityError(err)
	case !loggedIn:
		return antigravityLoggedOut(report), nil
	default:
		report.Account = account
		report.Credits = credits
	}

	usageRaw, err := antigravityCommand(ctx, binary, dir, env, "/usage")
	if errors.Is(err, errAntigravityLoggedOut) {
		return antigravityLoggedOut(report), nil
	}
	if err != nil {
		return report, antigravityCommandError(ctx, err)
	}
	quota, err := parseAntigravityUsage(usageRaw)
	if err != nil {
		return report, antigravityError(err)
	}
	report.Quota = quota
	if creditsRefused {
		loggedIn := true
		report.Account = harness.AccountSnapshot{Observation: quota.Observation, LoggedIn: &loggedIn}
	}
	return explained(report, "not reported by the CLI"), nil
}

func antigravityLoggedOut(report harness.AccountReport) harness.AccountReport {
	loggedIn := false
	report.Account = harness.AccountSnapshot{
		Observation: harness.Observation{Quality: harness.Measured, ObservedAt: time.Now().UTC(), Source: "agy"},
		LoggedIn:    &loggedIn,
	}
	report.Quota, report.Credits = harness.QuotaSnapshot{}, harness.CreditSnapshot{}
	report.Quota.Reason = "not logged in"
	report.Credits.Reason = "not logged in"
	return report
}

func antigravityCommandError(ctx context.Context, err error) error {
	if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return ctx.Err()
	}
	return antigravityError(err)
}

// antigravityCommand stops at agy's logged-out prompt rather than sitting out
// the minute it waits for a browser login.
func antigravityCommand(parent context.Context, binary, dir string, env []string, slash string) ([]byte, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	cmd, proc, err := process.Command(ctx, binary, "-p="+slash, "--output-format", "json")
	if err != nil {
		return nil, errAntigravityStart
	}
	defer proc.Close()
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(nil)
	prompt := &antigravityAuthWatch{cancel: cancel}
	cmd.Stderr = prompt
	cmd.WaitDelay = 2 * time.Second

	reader, writer := io.Pipe()
	cmd.Stdout = writer
	reaped := make(chan error, 1)
	go func() {
		err := proc.Run()
		writer.CloseWithError(io.EOF)
		reaped <- err
	}()
	stopClosing := context.AfterFunc(ctx, func() {
		reader.CloseWithError(ctx.Err())
	})
	defer stopClosing()

	data, readErr := readBoundedAntigravity(reader, antigravityOutputLimit)
	_ = reader.Close()
	exit := <-reaped
	if prompt.seen() && parent.Err() == nil {
		return nil, errAntigravityLoggedOut
	}

	if err := ctx.Err(); err != nil && (readErr != nil || len(bytes.TrimSpace(data)) == 0) {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, processExit(harness.Antigravity, exit)
	}
	return data, nil
}

// antigravityAuthWatch reads agy's stderr for the logged-out prompt and keeps
// none of it: the prompt carries a login URL.
type antigravityAuthWatch struct {
	cancel  context.CancelFunc
	mu      sync.Mutex
	partial []byte
	matched bool
}

func (w *antigravityAuthWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.matched {
		return len(p), nil
	}
	w.partial = append(w.partial, p...)
	for {
		line, rest, ok := bytes.Cut(w.partial, []byte("\n"))
		if !ok {
			break
		}
		w.partial = rest
		if nativecli.AntigravityAuthPrompt(string(line)) {
			w.matched, w.partial = true, nil
			w.cancel()
			return len(p), nil
		}
	}
	if len(w.partial) > 4<<10 {
		w.partial = nil
	}
	return len(p), nil
}

func (w *antigravityAuthWatch) seen() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.matched
}

func readBoundedAntigravity(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	var probe [1]byte
	copied, err := io.CopyN(&buf, r, limit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if copied == limit {
		if n, _ := r.Read(probe[:]); n > 0 {
			return nil, errAntigravityInvalid
		}
	}
	return buf.Bytes(), nil
}

type antigravityEnvelope struct {
	Status  string                      `json:"status"`
	Error   *nativecli.AntigravityError `json:"error"`
	Command *struct {
		Name string          `json:"name"`
		Data json.RawMessage `json:"data"`
	} `json:"command"`
}

func parseAntigravityEnvelope(raw []byte, wantCommand string) (json.RawMessage, bool, error) {
	var env antigravityEnvelope
	if json.Unmarshal(raw, &env) != nil {
		return nil, false, errAntigravityInvalid
	}
	if env.Status == "ERROR" || env.Error != nil {
		msg := ""
		if env.Error != nil {
			msg = strings.ToLower(env.Error.Message)
		}
		if nativecli.AntigravityAuthFailure(msg) || strings.Contains(msg, "not logged in") || strings.Contains(msg, "sign in") || strings.Contains(msg, "unauthenticated") || strings.Contains(msg, "login required") {
			return nil, true, nil
		}
		if strings.Contains(msg, "unknown command") || strings.Contains(msg, "unrecognized command") {
			return nil, false, errAntigravityMethod
		}
		return nil, false, errAntigravityRejected
	}
	if env.Status != "SUCCESS" || env.Command == nil || env.Command.Name != wantCommand || len(env.Command.Data) == 0 {
		return nil, false, errAntigravityInvalid
	}
	return env.Command.Data, false, nil
}

func parseAntigravityCredits(raw []byte) (harness.AccountSnapshot, harness.CreditSnapshot, bool, error) {
	data, loggedOut, err := parseAntigravityEnvelope(raw, "credits")
	now := time.Now().UTC()
	if err != nil {
		return harness.AccountSnapshot{}, harness.CreditSnapshot{}, false, err
	}
	if loggedOut {
		loggedIn := false
		return harness.AccountSnapshot{
			Observation: harness.Observation{Quality: harness.Measured, ObservedAt: now, Source: "/credits"},
			LoggedIn:    &loggedIn,
		}, harness.CreditSnapshot{}, false, nil
	}
	var payload struct {
		RemainingCredits *int64 `json:"remaining_credits"`
		PlanName         string `json:"plan_name"`
	}
	if json.Unmarshal(data, &payload) != nil || (payload.PlanName == "" && payload.RemainingCredits == nil) {
		return harness.AccountSnapshot{}, harness.CreditSnapshot{}, false, errAntigravityInvalid
	}
	loggedIn := true
	account := harness.AccountSnapshot{
		Observation: harness.Observation{Quality: harness.Measured, ObservedAt: now, Source: "/credits"},
		LoggedIn:    &loggedIn,
		Plan:        payload.PlanName,
	}
	var credits harness.CreditSnapshot
	if payload.RemainingCredits != nil {
		enabled := true
		unlimited := false
		credits = harness.CreditSnapshot{
			Observation: harness.Observation{Quality: harness.Measured, ObservedAt: now, Source: "/credits"},
			Enabled:     &enabled,
			Unlimited:   &unlimited,
			Balance: &harness.Amount{
				Value: strconv.FormatInt(*payload.RemainingCredits, 10),
				Unit:  harness.CreditUnit,
			},
		}
	} else {
		credits.Reason = "not reported by the CLI"
	}
	return account, credits, true, nil
}

func parseAntigravityUsage(raw []byte) (harness.QuotaSnapshot, error) {
	data, loggedOut, err := parseAntigravityEnvelope(raw, "usage")
	if err != nil {
		return harness.QuotaSnapshot{}, err
	}
	if loggedOut {
		return harness.QuotaSnapshot{}, errAntigravityRejected
	}
	var payload struct {
		Groups []struct {
			Name    string `json:"name"`
			Buckets []struct {
				Name              string   `json:"name"`
				Window            string   `json:"window"`
				Disabled          bool     `json:"disabled"`
				RemainingFraction *float64 `json:"remaining_fraction"`
				ResetTime         string   `json:"reset_time"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Groups == nil {
		return harness.QuotaSnapshot{}, errAntigravityInvalid
	}
	now := time.Now().UTC()
	obs := harness.Observation{Quality: harness.Measured, ObservedAt: now, Source: "/usage"}
	var windows []harness.QuotaWindow
	for _, group := range payload.Groups {
		for _, bucket := range group.Buckets {
			if bucket.Disabled || bucket.RemainingFraction == nil {
				continue
			}
			rem := *bucket.RemainingFraction
			if math.IsNaN(rem) || math.IsInf(rem, 0) || rem < 0 || rem > 1 {
				return harness.QuotaSnapshot{}, errAntigravityInvalid
			}
			used := math.Round((1-rem)*10000) / 100
			if used < 0 {
				used = 0
			}
			model := bucket.Name
			if group.Name != "" && !strings.EqualFold(group.Name, "Models") {
				model = group.Name
			}
			window := harness.QuotaWindow{
				Observation: obs,
				Model:       model,
				Name:        bucket.Name,
				UsedPercent: &used,
			}
			switch bucket.Window {
			case "5h":
				mins := int64(300)
				window.ID = "5h"
				window.Kind = harness.QuotaSession
				window.WindowMinutes = &mins
			case "weekly":
				mins := int64(10080)
				window.ID = "weekly"
				window.Kind = harness.QuotaWeekly
				if bucket.Name != "" {
					window.Kind = harness.QuotaWeeklyModel
				}
				window.WindowMinutes = &mins
			default:
				window.Kind = harness.QuotaOther
			}
			if bucket.ResetTime != "" {
				parsed, err := time.Parse(time.RFC3339Nano, bucket.ResetTime)
				if err != nil {
					return harness.QuotaSnapshot{}, errAntigravityInvalid
				}
				utc := parsed.UTC()
				window.ResetsAt = &utc
			}
			windows = append(windows, window)
		}
	}
	return harness.QuotaSnapshot{
		Observation: obs,
		Complete:    true,
		Windows:     windows,
	}, nil
}

var (
	errAntigravityStart     = errors.New("antigravity could not start")
	errAntigravityInvalid   = errors.New("antigravity answered in an unrecognized shape")
	errAntigravityMethod    = errors.New("antigravity does not offer the method")
	errAntigravityRejected  = errors.New("antigravity refused the request")
	errAntigravityLoggedOut = errors.New("antigravity asked for a login")
)

func antigravityError(err error) error {
	var typed *Error
	if errors.As(err, &typed) {
		return typed
	}
	e := &Error{Engine: harness.Antigravity, Family: harness.FailureRequest}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		e.Code, e.Cause = CodeTimedOut, harness.CauseTimeout
	case errors.Is(err, errAntigravityMethod):
		e.Code, e.Family = CodeMethodUnavailable, harness.FailureCapability
	case errors.Is(err, errAntigravityRejected):
		e.Code, e.Cause = CodeRejected, harness.CauseUnknown
	case errors.Is(err, errAntigravityStart):
		e.Code, e.Family = CodeProcessStartFailed, harness.FailureProcess
	default:
		e.Code = CodeInvalidResponse
	}
	return e
}
