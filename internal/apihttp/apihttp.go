// Package apihttp is the HTTP plumbing every OpenAI-compatible operation
// shares: the caller's credential, a client that neither consults ambient
// proxies nor follows redirects, bounded bodies, and a failure classification
// drawn only from status codes and allowlisted enums, never provider prose.
// Each operation wraps a Failure in its own typed error.
package apihttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness"
)

const (
	errorBodyLimit  = 64 << 10
	retryAfterLimit = time.Hour
)

// Phase says how far an exchange got.
type Phase string

const (
	PhasePreflight Phase = "preflight"
	// PhaseTransport is a request that failed before any response status.
	PhaseTransport Phase = "transport"
	PhaseResponse  Phase = "response"
)

// Failure is a classified API failure. Every field is a library constant or an
// allowlisted provider enum.
type Failure struct {
	Cause      harness.Cause
	Phase      Phase
	Code       string
	RetryAfter time.Duration
}

func (f *Failure) Error() string { return "api request failed: " + f.Code }

// Retryable reports only explicit transient rejections.
func (f *Failure) Retryable() bool {
	return f.Cause == harness.CauseOverloaded || f.Cause == harness.CauseRateLimited || f.Cause == harness.CauseUnavailable
}

// ResponseFailure is an unusable response. The terminal states a provider
// explains carry their cause.
func ResponseFailure(code string) *Failure {
	cause := harness.CauseUnknown
	switch code {
	case "output_truncated":
		cause = harness.CauseOutputTruncated
	case "content_filtered", "model_refusal":
		cause = harness.CauseContentFiltered
	}
	return &Failure{Cause: cause, Phase: PhaseResponse, Code: code}
}

// DefaultTransport does not honour ambient proxy variables, for the same
// reason CLI children get an allowlisted environment: process-wide settings
// must not route a caller's credential.
var DefaultTransport http.RoundTripper = func() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

// Credential resolves the request's bearer token; an unauthenticated request
// has none. The source's error is never retained: it may describe the caller's
// secret store, or contain the secret.
func Credential(ctx context.Context, api harness.API) (string, error) {
	if api.Unauthenticated {
		return "", nil
	}
	token, err := api.Credentials(ctx)
	if ctx.Err() != nil {
		return "", Interrupted(ctx, PhasePreflight, ctx.Err())
	}
	if err != nil {
		return "", &Failure{Cause: harness.CauseAuthentication, Phase: PhasePreflight, Code: "credential_unavailable"}
	}
	if token == "" || strings.IndexFunc(token, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
		return "", &Failure{Cause: harness.CauseAuthentication, Phase: PhasePreflight, Code: "invalid_credential"}
	}
	return token, nil
}

// Request is one exchange. Transport nil means DefaultTransport.
type Request struct {
	Method    string
	URL       string
	Body      []byte
	Token     string
	Transport http.RoundTripper
	// Limit bounds the response body; a longer body fails with output_limit.
	Limit int
}

// Do sends the request and returns a successful JSON response body. Every
// failure is context.Canceled or a *Failure.
func Do(ctx context.Context, r Request) ([]byte, error) {
	var body io.Reader
	if r.Body != nil {
		body = bytes.NewReader(r.Body)
	}
	request, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return nil, &Failure{Cause: harness.CauseUnknown, Phase: PhasePreflight, Code: "api_base_url_invalid"}
	}
	if r.Token != "" {
		request.Header.Set("Authorization", "Bearer "+r.Token)
	}
	if r.Body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "lib-agent-harness")
	response, err := client(r.Transport).Do(request)
	if err != nil {
		return nil, Interrupted(ctx, PhaseTransport, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, StatusFailure(response)
	}
	if !jsonMediaType(response.Header.Get("Content-Type")) {
		return nil, ResponseFailure("unexpected_media_type")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(r.Limit)+1))
	if err != nil {
		return nil, Interrupted(ctx, PhaseResponse, err)
	}
	if len(data) > r.Limit {
		return nil, ResponseFailure("output_limit")
	}
	return data, nil
}

func client(transport http.RoundTripper) *http.Client {
	if transport == nil {
		transport = DefaultTransport
	}
	// A redirect would move the credential somewhere the caller did not
	// configure; the 3xx is reported instead.
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Interrupted keeps cancellation's identity and never retains a transport
// error, which can name hosts or echo request data.
func Interrupted(ctx context.Context, phase Phase, err error) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Failure{Cause: harness.CauseTimeout, Phase: phase, Code: "deadline_exceeded"}
	}
	return &Failure{Cause: harness.CauseUnknown, Phase: phase, Code: "transport_failed"}
}

func jsonMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

// StatusFailure consults only the status, an allowlisted error code and a
// delay; the body's prose is never read into an error.
func StatusFailure(response *http.Response) *Failure {
	status := response.StatusCode
	failure := ResponseFailure("http_" + strconv.Itoa(status))
	if status >= 300 && status < 400 {
		failure.Code = "redirect_refused"
		return failure
	}
	failure.Cause = StatusCause(status)
	switch code := errorCode(response.Body); {
	case status == http.StatusNotFound && code == "model_not_found":
		failure.Cause, failure.Code = harness.CauseModelUnavailable, code
	case status == http.StatusBadRequest && code == "context_length_exceeded":
		failure.Cause, failure.Code = harness.CauseContextLimit, code
	// Quota exhaustion shares 429 with rate limiting but will not clear by waiting.
	case status == http.StatusTooManyRequests && code == "insufficient_quota":
		failure.Cause, failure.Code = harness.CauseQuotaExhausted, code
	}
	if failure.Retryable() {
		failure.RetryAfter = retryAfter(response.Header.Get("Retry-After"))
	}
	return failure
}

// EmbeddedFailure classifies an error object a provider returned inside a
// successful HTTP response, as some gateways do. There is no status to trust,
// so only an allowlisted string code classifies it, and nothing makes it
// retryable: a numeric code there is not an HTTP status.
func EmbeddedFailure(errorObject json.RawMessage) *Failure {
	failure := ResponseFailure("provider_error")
	switch code := allowlistedCode(errorObject); code {
	case "model_not_found":
		failure.Cause, failure.Code = harness.CauseModelUnavailable, code
	case "context_length_exceeded":
		failure.Cause, failure.Code = harness.CauseContextLimit, code
	case "insufficient_quota":
		failure.Cause, failure.Code = harness.CauseQuotaExhausted, code
	}
	return failure
}

// StatusCause is what a provider's HTTP status alone says about a failure,
// the one table every mode reads a status through. It says only what the
// status says unambiguously: a 500, 502 or 504 may come after the upstream
// already acted, so it is not called unavailable, and a 429 may equally be an
// exhausted plan, which only the provider's own code tells apart.
func StatusCause(status int) harness.Cause {
	switch status {
	case http.StatusUnauthorized:
		return harness.CauseAuthentication
	case http.StatusForbidden:
		return harness.CausePermissionDenied
	case http.StatusRequestEntityTooLarge:
		return harness.CauseContextLimit
	case http.StatusTooManyRequests:
		return harness.CauseRateLimited
	case http.StatusServiceUnavailable:
		return harness.CauseUnavailable
	case 529:
		return harness.CauseOverloaded
	}
	return harness.CauseUnknown
}

func errorCode(body io.Reader) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	data, err := io.ReadAll(io.LimitReader(body, errorBodyLimit))
	if err != nil || json.Unmarshal(data, &envelope) != nil {
		return ""
	}
	return allowlistedCode(envelope.Error)
}

// allowlistedCode reads an error object's code or type, and returns it only
// when it is one this library classifies.
func allowlistedCode(errorObject json.RawMessage) string {
	var fields struct {
		Code json.RawMessage `json:"code"`
		Type json.RawMessage `json:"type"`
	}
	if json.Unmarshal(errorObject, &fields) != nil {
		return ""
	}
	for _, raw := range []json.RawMessage{fields.Code, fields.Type} {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			continue
		}
		switch value {
		case "model_not_found", "context_length_exceeded", "insufficient_quota":
			return value
		}
	}
	return ""
}

// retryAfter trusts only delta-seconds; a date depends on clocks agreeing.
func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	delay := time.Duration(seconds) * time.Second
	if delay > retryAfterLimit {
		return 0
	}
	return delay
}

// Echoes reports whether text carries the request's credential. An
// unauthenticated request has none, and every text contains "".
func Echoes(text, token string) bool {
	return token != "" && strings.Contains(text, token)
}
