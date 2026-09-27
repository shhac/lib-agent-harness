package completion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// EngineOpenAICompatible selects a remote endpoint speaking an explicit OpenAI
// dialect instead of an installed CLI. The caller's API configuration is used;
// no native login or ambient API key is.
const EngineOpenAICompatible = "openai-compatible"

// APIDialect names a wire protocol. There is no default: an endpoint being
// "OpenAI-compatible" does not say which protocol it speaks.
type APIDialect string

const OpenAIChatCompletions APIDialect = "openai-chat-completions"

// CredentialSource returns one bearer token for one request. It is a function
// so that printing a Config cannot print a token. Its error is never retained.
type CredentialSource func(context.Context) (string, error)

type APIConfig struct {
	// BaseURL is absolute https, or http to a loopback host, with no user
	// information, query or fragment. The dialect appends its own path.
	BaseURL     string
	Dialect     APIDialect
	Credentials CredentialSource
}

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
	body, err := chatRequestBody(cfg.Model, messages, tools)
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
	token, err := apiCredential(ctx, cfg.API.Credentials)
	if err != nil {
		return Message{}, Usage{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Message{}, Usage{}, preflightFailure(EngineOpenAICompatible, "api_base_url_invalid")
	}
	request.Header.Set("Authorization", "Bearer "+token)
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
	if bytes.Contains(data, []byte(token)) {
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

func validateAPIConfig(cfg Config) (string, error) {
	switch cfg.API.Dialect {
	case OpenAIChatCompletions:
	case "":
		return "", preflightFailure(EngineOpenAICompatible, "api_dialect_required")
	default:
		return "", preflightFailure(EngineOpenAICompatible, "api_dialect_unsupported")
	}
	endpoint, err := chatCompletionsURL(cfg.API.BaseURL)
	if err != nil {
		return "", err
	}
	if cfg.API.Credentials == nil {
		return "", preflightFailure(EngineOpenAICompatible, "api_credentials_required")
	}
	if cfg.Effort != "" {
		return "", preflightFailure(EngineOpenAICompatible, "api_effort_unsupported")
	}
	return endpoint, nil
}

func chatCompletionsURL(base string) (string, error) {
	invalid := preflightFailure(EngineOpenAICompatible, "api_base_url_invalid")
	if strings.ContainsAny(base, "?#") {
		return "", invalid
	}
	u, err := url.Parse(base)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.User != nil {
		return "", invalid
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopbackHost(u.Hostname()) {
			return "", preflightFailure(EngineOpenAICompatible, "api_base_url_insecure")
		}
	default:
		return "", invalid
	}
	return u.JoinPath("chat", "completions").String(), nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is null only for an assistant message that carries tool calls.
	Content    *string    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      bool           `json:"strict,omitempty"`
}

func chatRequestBody(model string, messages []Message, tools []Tool) ([]byte, error) {
	if _, err := toolCatalog(tools); err != nil {
		return nil, preflightFailure(EngineOpenAICompatible, "invalid_tool_catalog")
	}
	wire, ok := chatMessages(messages)
	if !ok {
		return nil, preflightFailure(EngineOpenAICompatible, "invalid_messages")
	}
	request := chatRequest{Model: model, Messages: wire}
	for _, tool := range tools {
		request.Tools = append(request.Tools, chatTool{Type: "function", Function: chatFunction{
			Name: tool.Function.Name, Description: tool.Function.Description, Parameters: tool.Function.Parameters, Strict: tool.Function.Strict,
		}})
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, preflightFailure(EngineOpenAICompatible, "invalid_tool_catalog")
	}
	return body, nil
}

func chatMessages(messages []Message) ([]chatMessage, bool) {
	if len(messages) == 0 {
		return nil, false
	}
	wire := make([]chatMessage, 0, len(messages))
	for _, message := range messages {
		content := message.Content
		out := chatMessage{Role: message.Role, Content: &content, ToolCallID: message.ToolCallID}
		switch message.Role {
		case "system", "user":
			if len(message.ToolCalls) > 0 || message.ToolCallID != "" {
				return nil, false
			}
		case "tool":
			if len(message.ToolCalls) > 0 || message.ToolCallID == "" {
				return nil, false
			}
		case "assistant":
			if message.ToolCallID != "" {
				return nil, false
			}
			for _, call := range message.ToolCalls {
				if call.Type != "function" || call.ID == "" || call.Function.Name == "" {
					return nil, false
				}
			}
			out.ToolCalls = message.ToolCalls
			if len(message.ToolCalls) > 0 && content == "" {
				out.Content = nil
			}
		default:
			return nil, false
		}
		wire = append(wire, out)
	}
	return wire, true
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

type chatCompletionResponse struct {
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Message      *struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			Refusal   *string         `json:"refusal"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function *struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// parseChatCompletion accepts only an unambiguous terminal response. Anything
// else returns no reply, alongside the usage the response reported.
func parseChatCompletion(data []byte, tools []Tool) (Message, Usage, error) {
	usage := chatUsage(data)
	var response chatCompletionResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&response) != nil || decoder.Decode(new(any)) != io.EOF {
		return Message{}, usage, apiResponseFailure("malformed_response")
	}
	if len(response.Choices) != 1 {
		return Message{}, usage, apiResponseFailure("unexpected_choice_count")
	}
	choice := response.Choices[0]
	message := choice.Message
	if message == nil || (message.Role != "" && message.Role != "assistant") {
		return Message{}, usage, apiResponseFailure("malformed_response")
	}
	if message.Refusal != nil && *message.Refusal != "" {
		return Message{}, usage, apiResponseFailure("model_refusal")
	}
	result := Message{Role: "assistant"}
	if len(message.Content) > 0 && string(message.Content) != "null" && json.Unmarshal(message.Content, &result.Content) != nil {
		return Message{}, usage, apiResponseFailure("malformed_response")
	}
	if code := finishFailure(choice.FinishReason, len(message.ToolCalls)); code != "" {
		return Message{}, usage, apiResponseFailure(code)
	}
	if len(message.ToolCalls) > maxToolProposals {
		return Message{}, usage, apiResponseFailure("invalid_tool_call")
	}
	allowed, _ := toolCatalog(tools)
	seen := map[string]bool{}
	for _, call := range message.ToolCalls {
		if call.Type != "function" || call.Function == nil || call.ID == "" || len(call.ID) > apiToolCallIDLimit || seen[call.ID] || !allowed[call.Function.Name] || !jsonObject(call.Function.Arguments) {
			return Message{}, usage, apiResponseFailure("invalid_tool_call")
		}
		seen[call.ID] = true
		proposal := ToolCall{ID: call.ID, Type: "function"}
		proposal.Function.Name = call.Function.Name
		proposal.Function.Arguments = call.Function.Arguments
		result.ToolCalls = append(result.ToolCalls, proposal)
	}
	return result, usage, nil
}

func finishFailure(reason *string, calls int) string {
	if reason == nil {
		return "ambiguous_terminal_state"
	}
	switch {
	case *reason == "stop" && calls == 0, *reason == "tool_calls" && calls > 0:
		return ""
	case *reason == "length":
		return "output_truncated"
	case *reason == "content_filter":
		return "content_filtered"
	}
	return "ambiguous_terminal_state"
}

func jsonObject(arguments string) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal([]byte(arguments), &object) == nil && object != nil
}

// chatUsage applies the CLI adapters' rule: a report counts only when complete
// and consistent. prompt_tokens already includes cached input.
func chatUsage(data []byte) Usage {
	var response struct {
		Usage *struct {
			Prompt     *int `json:"prompt_tokens"`
			Completion *int `json:"completion_tokens"`
			Total      *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &response) != nil || response.Usage == nil {
		return Usage{}
	}
	usage, ok := normalizedUsage(response.Usage.Prompt, response.Usage.Completion, nil, nil)
	if !ok || (response.Usage.Total != nil && *response.Usage.Total != usage.TotalTokens) {
		return Usage{}
	}
	return usage
}

func messageContains(message Message, token string) bool {
	if strings.Contains(message.Content, token) {
		return true
	}
	for _, call := range message.ToolCalls {
		if strings.Contains(call.ID+call.Function.Name+call.Function.Arguments, token) {
			return true
		}
	}
	return false
}
