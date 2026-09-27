package completion

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
)

// EngineOpenAICompatible selects a remote endpoint speaking an explicit OpenAI
// dialect instead of an installed CLI. The caller's API configuration is used;
// no native login or ambient API key is.
const EngineOpenAICompatible = "openai-compatible"

const (
	apiResponseLimit   = 2 << 20
	apiErrorBodyLimit  = 64 << 10
	apiToolCallIDLimit = 256
	apiRetryAfterLimit = time.Hour
	maxToolProposals   = 16
)

// Ambient proxy variables are not honoured, for the same reason CLI children
// get an allowlisted environment: process-wide settings must not route a
// caller's credential.
var apiTransport http.RoundTripper = func() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

func openAIComplete(ctx context.Context, cfg Config, messages []Message, tools []Tool) (Message, Usage, error) {
	endpoint, err := validateAPIConfig(cfg)
	if err != nil {
		return Message{}, Usage{}, err
	}
	body, err := chatRequestBody(cfg, messages, tools)
	if err != nil {
		return Message{}, Usage{}, err
	}
	if len(body) > cfg.MaxContextBytes {
		return Message{}, Usage{}, &RequestError{Kind: ErrorContextLimit, Engine: EngineOpenAICompatible, Phase: PhasePreflight, Code: "context_bytes"}
	}
	if err := ctx.Err(); err != nil {
		return Message{}, Usage{}, err
	}
	if cfg.BeforeRequest != nil {
		if err := cfg.BeforeRequest(ctx); err != nil {
			return Message{}, Usage{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	token, err := requestCredential(ctx, cfg.API)
	if err != nil {
		return Message{}, Usage{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, preflightFailure(EngineOpenAICompatible, "api_base_url_invalid")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "lib-agent-harness")
	response, err := apiClient(cfg).Do(request)
	if err != nil {
		return Message{}, Usage{}, apiInterrupted(ctx, PhaseTransport, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Message{}, Usage{}, apiStatusFailure(response)
	}
	if !jsonMediaType(response.Header.Get("Content-Type")) {
		return Message{}, Usage{}, apiResponseFailure("unexpected_media_type")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, apiResponseLimit+1))
	if err != nil {
		return Message{}, Usage{}, apiInterrupted(ctx, PhaseResponse, err)
	}
	if len(data) > apiResponseLimit {
		return Message{}, Usage{}, apiResponseFailure("output_limit")
	}
	// A reflecting endpoint would otherwise put the credential in a transcript.
	if echoes(string(data), token) {
		return Message{}, chatUsage(data), apiResponseFailure("credential_echoed")
	}
	result, usage, err := parseChatCompletion(data, tools)
	if err != nil {
		return Message{}, usage, err
	}
	if messageContains(result, token) {
		return Message{}, usage, apiResponseFailure("credential_echoed")
	}
	return result, usage, nil
}

// requestCredential resolves the request's bearer token; an unauthenticated
// request has none.
func requestCredential(ctx context.Context, api APIConfig) (string, error) {
	if api.Unauthenticated {
		return "", nil
	}
	return apiCredential(ctx, api.Credentials)
}

// apiCredential never retains the source's error: it may describe the
// caller's secret store, or contain the secret.
func apiCredential(ctx context.Context, source CredentialSource) (string, error) {
	token, err := source(ctx)
	if ctx.Err() != nil {
		return "", apiInterrupted(ctx, PhasePreflight, ctx.Err())
	}
	if err != nil {
		return "", &RequestError{Kind: ErrorAuthentication, Engine: EngineOpenAICompatible, Phase: PhasePreflight, Code: "credential_unavailable"}
	}
	if token == "" || strings.IndexFunc(token, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
		return "", &RequestError{Kind: ErrorAuthentication, Engine: EngineOpenAICompatible, Phase: PhasePreflight, Code: "invalid_credential"}
	}
	return token, nil
}

func apiClient(cfg Config) *http.Client {
	transport := cfg.transport
	if transport == nil {
		transport = apiTransport
	}
	// A redirected POST would move the credential and body somewhere the caller
	// did not configure; the 3xx is reported instead.
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func apiInterrupted(ctx context.Context, phase ErrorPhase, err error) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &RequestError{Kind: ErrorTimeout, Engine: EngineOpenAICompatible, Phase: phase, Code: "deadline_exceeded"}
	}
	return &RequestError{Kind: ErrorUnknown, Engine: EngineOpenAICompatible, Phase: phase, Code: "transport_failed"}
}

func apiResponseFailure(code string) *RequestError {
	return &RequestError{Kind: ErrorUnknown, Engine: EngineOpenAICompatible, Phase: PhaseResponse, Code: code}
}

func jsonMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

// apiStatusFailure consults only the status, an allowlisted error code and a
// delay; the body's prose is never read into an error.
func apiStatusFailure(response *http.Response) *RequestError {
	status := response.StatusCode
	failure := apiResponseFailure("http_" + strconv.Itoa(status))
	if status >= 300 && status < 400 {
		failure.Code = "redirect_refused"
		return failure
	}
	code := openAIErrorCode(response.Body)
	switch {
	case status == http.StatusUnauthorized:
		failure.Kind = ErrorAuthentication
	case status == http.StatusForbidden:
		failure.Kind = ErrorPermissionDenied
	case status == http.StatusNotFound && code == "model_not_found":
		failure.Kind, failure.Code = ErrorModelUnavailable, code
	case status == http.StatusBadRequest && code == "context_length_exceeded":
		failure.Kind, failure.Code = ErrorContextLimit, code
	case status == http.StatusRequestEntityTooLarge:
		failure.Kind = ErrorContextLimit
	// Quota exhaustion shares 429 with rate limiting but will not clear by waiting.
	case status == http.StatusTooManyRequests && code == "insufficient_quota":
		failure.Code = code
	case status == http.StatusTooManyRequests:
		failure.Kind = ErrorRateLimited
	case status == http.StatusServiceUnavailable:
		failure.Kind = ErrorUnavailable
	case status == 529:
		failure.Kind = ErrorOverloaded
	}
	if failure.Retryable() {
		failure.RetryAfter = retryAfter(response.Header.Get("Retry-After"))
	}
	return failure
}

func openAIErrorCode(body io.Reader) string {
	var envelope struct {
		Error struct {
			Code json.RawMessage `json:"code"`
			Type json.RawMessage `json:"type"`
		} `json:"error"`
	}
	data, err := io.ReadAll(io.LimitReader(body, apiErrorBodyLimit))
	if err != nil || json.Unmarshal(data, &envelope) != nil {
		return ""
	}
	for _, raw := range []json.RawMessage{envelope.Error.Code, envelope.Error.Type} {
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
	if delay > apiRetryAfterLimit {
		return 0
	}
	return delay
}

func messageContains(message Message, token string) bool {
	if echoes(message.Content, token) {
		return true
	}
	for _, call := range message.ToolCalls {
		if echoes(call.ID+call.Function.Name+call.Function.Arguments, token) {
			return true
		}
	}
	return false
}

// echoes reports whether text carries the request's credential. An
// unauthenticated request has none, and every text contains "".
func echoes(text, token string) bool {
	return token != "" && strings.Contains(text, token)
}
