package completion

// Grok's `--output-format=streaming-json` is NDJSON, one type-tagged object
// per line (verified against grok 1.0.41: available_commands, thought, text,
// tool_call, tool_call_update, usage, error, end). A completion reads it as it
// arrives: a native tool catalog or call stops the process at once, and only
// a single `end` with stopReason end_turn and structured output succeeds.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/apihttp"
	"github.com/shhac/lib-agent-harness/process"
)

type grokEvent struct {
	Type          string             `json:"type"`
	SessionUpdate string             `json:"sessionUpdate"`
	Tools         *[]json.RawMessage `json:"tools"`
	Message       string             `json:"message"`
	StopReason    string             `json:"stopReason"`
	Status        string             `json:"status"`
	grokSpend
	Structured json.RawMessage `json:"structuredOutput"`
}

// grokSpend is the headless spend projection on `end`, and on `error` when
// usage was recorded before the failure.
type grokSpend struct {
	Usage           *grokUsageReport `json:"usage"`
	UsageIncomplete bool             `json:"usage_is_incomplete"`
	CostUSD         *float64         `json:"total_cost_usd"`
	CostTicks       *int64           `json:"total_cost_usd_ticks"`
	CostPartial     bool             `json:"cost_is_partial"`
}

// grokUsageReport's input_tokens is uncached input only; both cache figures
// are reported beside it.
type grokUsageReport struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
	Reasoning  *int64 `json:"reasoning_tokens"`
}

func (s grokSpend) present() bool { return s.Usage != nil || s.CostUSD != nil || s.CostTicks != nil }

// grokStream judges one launch's stream, line by line.
type grokStream struct {
	tools []Tool
	// catalog is set by an available_commands event stating an empty tool list:
	// positive evidence, not the absence of a complaint.
	catalog    bool
	unexpected string
	malformed  bool
	// activity is any model output before a failure, which makes a failure no
	// longer a clean rejection a caller could retry.
	activity   bool
	errored    bool
	errorCause harness.Cause
	errorCode  string
	errorSpend *grokSpend
	ends       int
	end        grokEvent
}

func newGrokStream(tools []Tool) *grokStream { return &grokStream{tools: tools} }

// observe reads one line and returns a code when the process must be stopped
// now: the native surface was not what the probe proved.
func (s *grokStream) observe(line []byte) string {
	if len(bytes.TrimSpace(line)) == 0 || s.unexpected != "" {
		return s.unexpected
	}
	var ev grokEvent
	if json.Unmarshal(line, &ev) != nil {
		s.malformed = true
		return ""
	}
	kind := ev.Type
	if kind == "" {
		kind = ev.SessionUpdate
	}
	switch kind {
	case "available_commands":
		if ev.Tools != nil && len(*ev.Tools) != 0 {
			s.unexpected = "unexpected_native_tool_catalog"
		} else if ev.Tools != nil {
			s.catalog = true
		}
	case "tool_call", "tool_call_update":
		s.unexpected = "unexpected_native_tool_call"
	case "text", "thought", "agent_message_chunk", "agent_thought_chunk", "usage":
		s.activity = true
	case "error":
		s.observeError(ev)
	case "end":
		s.ends++
		s.end = ev
	case "":
		s.malformed = true
	default:
		if strings.Contains(kind, "tool") {
			s.unexpected = "unexpected_native_tool_call"
		}
	}
	return s.unexpected
}

func (s *grokStream) observeError(ev grokEvent) {
	if ev.grokSpend.present() {
		spend := ev.grokSpend
		s.errorSpend = &spend
	}
	if s.errored {
		return
	}
	s.errored = true
	s.errorCause, s.errorCode = grokErrorClass(ev.Message)
	if s.activity {
		s.errorCause = harness.CauseUnknown
	}
}

// grokErrorClass reads only the HTTP status Grok states in its error
// envelope, never its prose.
func grokErrorClass(message string) (harness.Cause, string) {
	body, ok := strings.CutPrefix(message, "Internal error: ")
	if !ok {
		return harness.CauseUnknown, "grok_error"
	}
	var envelope struct {
		Status *int `json:"http_status"`
	}
	if json.Unmarshal([]byte(body), &envelope) != nil || envelope.Status == nil || *envelope.Status < 100 || *envelope.Status > 599 {
		return harness.CauseUnknown, "grok_error"
	}
	return apihttp.StatusCause(*envelope.Status), "http_" + strconv.Itoa(*envelope.Status)
}

// result settles the stream. Accounting comes from the single `end` (or an
// earlier `error`'s spend when `end` carries none) whether or not the request
// succeeded; a failure never carries a proposal.
func (s *grokStream) result() (Result, *RequestError) {
	accounting := s.accounting()
	fail := func(cause harness.Cause, code string) (Result, *RequestError) {
		return accounting, &RequestError{Cause: cause, Engine: harness.Grok, Phase: PhaseResponse, Code: code}
	}
	switch {
	case s.unexpected != "":
		return fail(harness.CauseUnknown, s.unexpected)
	case s.malformed:
		return fail(harness.CauseUnknown, "malformed_event_json")
	case s.ends > 1:
		return fail(harness.CauseUnknown, "duplicate_terminal_result")
	case s.errored:
		return fail(s.errorCause, s.errorCode)
	case s.ends == 0:
		return fail(harness.CauseUnknown, "missing_terminal_result")
	}
	if code := grokStopCode(s.end); code != "" {
		return fail(harness.CauseUnknown, code)
	}
	if !s.catalog {
		return fail(harness.CauseUnknown, "missing_native_tool_catalog")
	}
	if jsonAbsent(s.end.Structured) {
		return fail(harness.CauseUnknown, "missing_structured_output")
	}
	message, err := parseActionEnvelope(s.end.Structured, s.tools)
	if err != nil {
		return fail(harness.CauseUnknown, "invalid_action_envelope")
	}
	accounting.Message = message
	return accounting, nil
}

// grokStopCode: only end_turn is a finished turn. Every other reason, and any
// reason this parser does not know, is a failure with a fixed code.
func grokStopCode(end grokEvent) string {
	if end.Status == "failed" || end.Status == "error" || end.StopReason == "error" {
		return "end_error"
	}
	switch end.StopReason {
	case "end_turn":
		return ""
	case "max_tokens", "max_turn_requests", "refusal", "cancelled":
		return end.StopReason
	}
	return "unexpected_stop_reason"
}

func (s *grokStream) accounting() Result {
	if s.ends != 1 {
		return Result{}
	}
	spend := s.end.grokSpend
	if !spend.present() && s.errorSpend != nil {
		spend = *s.errorSpend
	}
	incomplete := s.end.UsageIncomplete || spend.UsageIncomplete
	var out Result
	if !incomplete {
		out.Usage = grokUsage(spend.Usage)
	}
	if !incomplete && !spend.CostPartial && !s.end.CostPartial {
		out.Cost = grokCost(spend)
	}
	return out
}

// grokUsage adds both cache figures back into Input, which counts every prompt
// token. Absent, negative or overflowing figures leave the usage unknown.
func grokUsage(report *grokUsageReport) harness.Usage {
	if report == nil || report.Input == nil || report.Output == nil {
		return harness.Usage{}
	}
	cacheRead, cacheWrite := valueOrZero(report.CacheRead), valueOrZero(report.CacheWrite)
	input, ok := sumTokens(*report.Input, cacheRead, cacheWrite)
	if !ok {
		return harness.Usage{}
	}
	if _, ok = sumTokens(input, *report.Output); !ok {
		return harness.Usage{}
	}
	usage := harness.Usage{
		Known:      true,
		Input:      input,
		Output:     *report.Output,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
		CacheKnown: report.CacheRead != nil && report.CacheWrite != nil,
	}
	if reasoning := report.Reasoning; reasoning != nil {
		if *reasoning < 0 {
			return harness.Usage{}
		}
		usage.Reasoning, usage.ReasoningKnown = *reasoning, true
	}
	return usage
}

// grokCostTicksPerUSD: Grok states cost exactly in ticks of 10^-10 USD.
const grokCostTicksPerUSD = 1e10

func grokCost(spend grokSpend) harness.Cost {
	switch {
	case spend.CostTicks != nil && *spend.CostTicks >= 0:
		return harness.Cost{USD: float64(*spend.CostTicks) / grokCostTicksPerUSD, Known: true}
	case spend.CostTicks == nil && spend.CostUSD != nil && *spend.CostUSD >= 0:
		return harness.Cost{USD: *spend.CostUSD, Known: true}
	}
	return harness.Cost{}
}

func jsonAbsent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// errGrokStopped reports that the launch was stopped because its stream showed
// a native surface the probe did not prove.
var errGrokStopped = errors.New("grok launch stopped at an unexpected native surface")

const (
	grokMaxLine   = 8 << 20
	grokMaxStream = 64 << 20
)

// grokLines hands each complete line to observe as it arrives and keeps only
// the line in progress; the stream judge holds everything else.
type grokLines struct {
	partial  []byte
	total    int
	exceeded bool
	stopped  bool
	stop     func()
	observe  func([]byte) string
}

func (w *grokLines) Write(p []byte) (int, error) {
	if w.stopped || w.exceeded {
		return 0, errGrokStopped
	}
	w.total += len(p)
	if w.total > grokMaxStream {
		w.exceeded = true
		w.stop()
		return 0, errOutputLimit
	}
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		line := w.partial[:i]
		w.partial = w.partial[i+1:]
		if w.observe(line) != "" {
			w.stopped = true
			w.stop()
			return 0, errGrokStopped
		}
	}
	if len(w.partial) > grokMaxLine {
		w.exceeded = true
		w.stop()
		return 0, errOutputLimit
	}
	w.partial = append([]byte(nil), w.partial...)
	return len(p), nil
}

func (w *grokLines) flush() {
	if w.stopped || w.exceeded || len(w.partial) == 0 {
		return
	}
	if w.observe(w.partial) != "" {
		w.stopped = true
	}
	w.partial = nil
}

// runGrokLaunch runs the credentialed launch with the stream judged as it
// arrives. The synthetic runner's output is judged line by line the same way,
// stopping where a real launch would have been stopped.
func runGrokLaunch(ctx context.Context, cfg Config, bin string, args []string, dir string, env []string, observe func([]byte) string) error {
	if cfg.run != nil {
		output, err := cfg.run(ctx, bin, args, dir, env, "")
		for _, line := range bytes.Split(output, []byte("\n")) {
			if observe(line) != "" {
				return errGrokStopped
			}
		}
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	cmd, child, err := process.Command(ctx, bin, args...)
	if err != nil {
		return err
	}
	defer child.Close()
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = 2 * time.Second
	lines := &grokLines{stop: child.Stop, observe: observe}
	cmd.Stdout = lines
	// Diagnostics may carry provider text or record content; they never reach
	// errors, results or logs.
	cmd.Stderr = io.Discard
	err = child.Run()
	lines.flush()
	switch {
	case lines.stopped:
		return errGrokStopped
	case ctx.Err() != nil:
		return ctx.Err()
	case lines.exceeded:
		return errOutputLimit
	}
	return err
}

// grokRunFailure classifies a launch that did not exit cleanly, preferring
// what the stream established over the exit status.
func grokRunFailure(ctx context.Context, runErr error, stream *grokStream) error {
	if errors.Is(runErr, errGrokStopped) {
		if _, failure := stream.result(); failure != nil {
			return failure
		}
		return &RequestError{Cause: harness.CauseUnknown, Engine: harness.Grok, Phase: PhaseResponse, Code: "unexpected_native_tool"}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(runErr, context.Canceled) {
		return context.Canceled
	}
	process := &RequestError{Cause: harness.CauseUnknown, Engine: harness.Grok, Phase: PhaseProcess}
	switch {
	case errors.Is(runErr, context.DeadlineExceeded):
		process.Cause, process.Code = harness.CauseTimeout, "deadline_exceeded"
		return process
	case errors.Is(runErr, errOutputLimit):
		process.Code = "output_limit"
		return process
	}
	if start := startFailure(harness.Grok, PhaseProcess, runErr); start != nil {
		return start
	}
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) {
		return process
	}
	code := exit.ExitCode()
	if _, failure := stream.result(); failure != nil && failure.Code != "missing_terminal_result" {
		failure.ExitCode = &code
		return failure
	}
	process.ExitCode = &code
	return process
}
