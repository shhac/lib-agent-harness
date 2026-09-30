package completion

import (
	"context"
	"errors"
	"net/http"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/apihttp"
)

const (
	apiResponseLimit   = 2 << 20
	apiToolCallIDLimit = 256
	maxToolProposals   = 16
)

// openAIComplete serves a harness.OpenAICompatible provider, whose
// configuration Complete has already checked with Provider.Problem. The
// caller's API configuration is used; no native login or ambient API key is.
func openAIComplete(ctx context.Context, cfg Config, messages []Message, tools []Tool) (Result, error) {
	api := cfg.Provider.API
	if code := api.EffortProblem(cfg.Effort); code != "" {
		return Result{}, preflightFailure(harness.OpenAICompatible, code)
	}
	endpoint, code := api.Endpoint("chat", "completions")
	if code != "" {
		return Result{}, preflightFailure(harness.OpenAICompatible, code)
	}
	body, err := chatRequestBody(cfg, messages, tools)
	if err != nil {
		return Result{}, err
	}
	if len(body) > cfg.MaxContextBytes {
		return Result{}, &RequestError{Cause: harness.CauseContextLimit, Engine: harness.OpenAICompatible, Phase: PhasePreflight, Code: "context_bytes"}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if cfg.BeforeRequest != nil {
		if err := cfg.BeforeRequest(ctx); err != nil {
			return Result{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	token, err := apihttp.Credential(ctx, api)
	if err != nil {
		return Result{}, apiFailure(err)
	}
	data, err := apihttp.Do(ctx, apihttp.Request{Method: http.MethodPost, URL: endpoint, Body: body, Token: token, Transport: cfg.transport, Limit: apiResponseLimit})
	if err != nil {
		return Result{}, apiFailure(err)
	}
	// A reflecting endpoint would otherwise put the credential in a transcript.
	if apihttp.Echoes(string(data), token) {
		return Result{Usage: chatUsage(data)}, apiResponseFailure("credential_echoed")
	}
	result, err := parseChatCompletion(data, tools, replayBinding(cfg))
	if err != nil {
		return result, err
	}
	if messageContains(result.Message, token) {
		return Result{Usage: result.Usage}, apiResponseFailure("credential_echoed")
	}
	return result, nil
}

// apiFailure gives a shared API failure this package's typed error;
// cancellation passes through unchanged.
func apiFailure(err error) error {
	var failure *apihttp.Failure
	if !errors.As(err, &failure) {
		return err
	}
	return &RequestError{Cause: failure.Cause, Engine: harness.OpenAICompatible, Phase: ErrorPhase(failure.Phase), Code: failure.Code, RetryAfter: failure.RetryAfter}
}

func apiResponseFailure(code string) error {
	return apiFailure(apihttp.ResponseFailure(code))
}

func messageContains(message Message, token string) bool {
	if apihttp.Echoes(message.Content, token) {
		return true
	}
	for _, call := range message.ToolCalls {
		if apihttp.Echoes(call.ID+call.Function.Name+call.Function.Arguments, token) {
			return true
		}
	}
	return false
}
