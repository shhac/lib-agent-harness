package completion

import (
	"context"
	"net"
	"net/url"
	"strings"
)

// APIDialect names a wire protocol. There is no default: an endpoint being
// "OpenAI-compatible" does not say which protocol it speaks.
type APIDialect string

const OpenAIChatCompletions APIDialect = "openai-chat-completions"

// CredentialSource returns one bearer token for one request. It is a function
// so that printing a Config cannot print a token. Its error is never retained.
type CredentialSource func(context.Context) (string, error)

// EffortParameter names where an endpoint reads Config.Effort. Compatible
// endpoints disagree: OpenAI and xAI read a top-level reasoning_effort, while
// gateways such as Vercel AI Gateway and OpenRouter read reasoning.effort.
type EffortParameter string

const (
	EffortReasoningEffort EffortParameter = "reasoning_effort"
	EffortReasoningObject EffortParameter = "reasoning.effort"
)

type APIConfig struct {
	// BaseURL is absolute https, or http to a loopback host, with no user
	// information, query or fragment. The dialect appends its own path.
	BaseURL     string
	Dialect     APIDialect
	Credentials CredentialSource
	// Unauthenticated sends no credential, for a local model server that takes
	// none. It is refused for a non-loopback BaseURL or beside Credentials, so
	// a forgotten credential source is never mistaken for this choice.
	Unauthenticated bool
	// EffortParameter is required when Config.Effort is set; the effort is
	// sent there and nowhere else.
	EffortParameter EffortParameter
}

func validateAPIConfig(cfg Config) (string, error) {
	switch cfg.API.Dialect {
	case OpenAIChatCompletions:
	case "":
		return "", preflightFailure(EngineOpenAICompatible, "api_dialect_required")
	default:
		return "", preflightFailure(EngineOpenAICompatible, "api_dialect_unsupported")
	}
	base, err := apiBaseURL(cfg.API.BaseURL)
	if err != nil {
		return "", err
	}
	if code := credentialConfigFailure(cfg.API, loopbackHost(base.Hostname())); code != "" {
		return "", preflightFailure(EngineOpenAICompatible, code)
	}
	if code := effortConfigFailure(cfg.Effort, cfg.API.EffortParameter); code != "" {
		return "", preflightFailure(EngineOpenAICompatible, code)
	}
	return base.JoinPath("chat", "completions").String(), nil
}

func credentialConfigFailure(api APIConfig, loopback bool) string {
	if !api.Unauthenticated {
		if api.Credentials == nil {
			return "api_credentials_required"
		}
		return ""
	}
	if api.Credentials != nil {
		return "api_credentials_conflict"
	}
	if !loopback {
		return "api_unauthenticated_remote"
	}
	return ""
}

// effortConfigFailure checks only the effort's shape; which levels a model
// accepts is the endpoint's to say.
func effortConfigFailure(effort string, parameter EffortParameter) string {
	switch parameter {
	case "", EffortReasoningEffort, EffortReasoningObject:
	default:
		return "api_effort_parameter_unsupported"
	}
	if effort == "" {
		return ""
	}
	if parameter == "" {
		return "api_effort_parameter_required"
	}
	if len(effort) > 32 || strings.IndexFunc(effort, func(r rune) bool { return (r < 'a' || r > 'z') && r != '_' && r != '-' }) >= 0 {
		return "api_effort_invalid"
	}
	return ""
}

func apiBaseURL(base string) (*url.URL, error) {
	invalid := preflightFailure(EngineOpenAICompatible, "api_base_url_invalid")
	if strings.ContainsAny(base, "?#") {
		return nil, invalid
	}
	u, err := url.Parse(base)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.User != nil {
		return nil, invalid
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !loopbackHost(u.Hostname()) {
			return nil, preflightFailure(EngineOpenAICompatible, "api_base_url_insecure")
		}
	default:
		return nil, invalid
	}
	return u, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
