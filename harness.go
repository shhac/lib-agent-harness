// Package harness is the vocabulary every execution mode shares: which engine
// runs a request, where it is reached, what it can do, what it consumed, and
// how it failed. The modes themselves live in completion (the model proposes,
// the caller executes), native (one agent run) and session (a persistent
// agent). An application that speaks this vocabulary can offer its users any
// engine without learning each one.
package harness

import (
	"context"
	"net"
	"net/url"
	"strings"
)

// Engine names the harness that executes a request. Its spelling is persisted
// by callers and never changes.
type Engine string

const (
	Codex  Engine = "codex"
	Claude Engine = "claude"
	Grok   Engine = "grok"
	// OpenAICompatible is any HTTP endpoint speaking an OpenAI dialect, such as
	// a gateway. It names the harness, not the model family: a Grok model
	// through a gateway is OpenAICompatible with a model such as "xai/grok-4".
	OpenAICompatible Engine = "openai-compatible"
)

// Engines lists every engine the library knows, in a stable order for display.
func Engines() []Engine { return []Engine{Codex, Claude, Grok, OpenAICompatible} }

// Transport says how an engine is reached.
type Transport string

const (
	CLITransport Transport = "cli"
	APITransport Transport = "api"
)

// Transport is "" for an engine the library does not know.
func (e Engine) Transport() Transport {
	switch e {
	case Codex, Claude, Grok:
		return CLITransport
	case OpenAICompatible:
		return APITransport
	}
	return ""
}

// Provider says where inference comes from. The engine's transport decides
// which half applies; setting the other half is refused, so a configuration
// can never be half-read.
type Provider struct {
	Engine Engine
	CLI    CLI
	API    API
}

// CLI locates an installed command-line harness.
type CLI struct {
	// Binary is the command to run; empty means the engine's name on PATH.
	Binary string
	// Home is the harness's configuration and login directory. Empty keeps
	// each mode's established default, which stored session references depend on.
	Home string
}

// Dialect names an HTTP wire protocol. There is no default: an endpoint being
// "OpenAI-compatible" does not say which protocol it speaks.
type Dialect string

const OpenAIChatCompletions Dialect = "openai-chat-completions"

// CredentialSource returns one bearer token for one request. It is a function
// so that printing a configuration cannot print a token. Its error is never
// retained.
type CredentialSource func(context.Context) (string, error)

// EffortParameter names where an endpoint reads reasoning effort. Compatible
// endpoints disagree: OpenAI and xAI read a top-level reasoning_effort, while
// gateways such as Vercel AI Gateway and OpenRouter read reasoning.effort.
type EffortParameter string

const (
	EffortReasoningEffort EffortParameter = "reasoning_effort"
	EffortReasoningObject EffortParameter = "reasoning.effort"
)

// API locates an HTTP endpoint.
type API struct {
	// BaseURL is absolute https, or http to a loopback host, with no user
	// information, query or fragment. The dialect appends its own paths.
	BaseURL     string
	Dialect     Dialect
	Credentials CredentialSource
	// Unauthenticated sends no credential, for a local model server that takes
	// none. It is refused for a non-loopback BaseURL or beside Credentials, so
	// a forgotten credential source is never mistaken for this choice.
	Unauthenticated bool
	// EffortParameter is required when an effort is requested; the effort is
	// sent there and nowhere else.
	EffortParameter EffortParameter
}

func (a API) zero() bool {
	return a.BaseURL == "" && a.Dialect == "" && a.Credentials == nil && !a.Unauthenticated && a.EffortParameter == ""
}

// Problem returns a fixed code naming what is wrong with the provider, or ""
// when it is well formed. Each mode reports the code in its own typed error;
// whether a mode supports the engine at all is Support's question.
func (p Provider) Problem() string {
	switch p.Engine.Transport() {
	case CLITransport:
		if !p.API.zero() {
			return "api_config_for_cli_engine"
		}
		return ""
	case APITransport:
		if p.CLI != (CLI{}) {
			return "cli_config_for_api_engine"
		}
		return p.API.problem()
	}
	return "unsupported_engine"
}

func (a API) problem() string {
	switch a.Dialect {
	case OpenAIChatCompletions:
	case "":
		return "api_dialect_required"
	default:
		return "api_dialect_unsupported"
	}
	base, code := a.base()
	if code != "" {
		return code
	}
	loopback := loopbackHost(base.Hostname())
	switch {
	case !a.Unauthenticated && a.Credentials == nil:
		return "api_credentials_required"
	case a.Unauthenticated && a.Credentials != nil:
		return "api_credentials_conflict"
	case a.Unauthenticated && !loopback:
		return "api_unauthenticated_remote"
	}
	switch a.EffortParameter {
	case "", EffortReasoningEffort, EffortReasoningObject:
		return ""
	}
	return "api_effort_parameter_unsupported"
}

// Endpoint joins the dialect path segments onto the validated base URL.
func (a API) Endpoint(segments ...string) (string, string) {
	base, code := a.base()
	if code != "" {
		return "", code
	}
	return base.JoinPath(segments...).String(), ""
}

func (a API) base() (*url.URL, string) {
	if strings.ContainsAny(a.BaseURL, "?#") {
		return nil, "api_base_url_invalid"
	}
	u, err := url.Parse(a.BaseURL)
	if err != nil || !u.IsAbs() || u.Opaque != "" || u.Host == "" || u.User != nil {
		return nil, "api_base_url_invalid"
	}
	switch u.Scheme {
	case "https":
		return u, ""
	case "http":
		if loopbackHost(u.Hostname()) {
			return u, ""
		}
		return nil, "api_base_url_insecure"
	}
	return nil, "api_base_url_invalid"
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// EffortProblem checks an effort's shape for an API provider; which levels a
// model accepts is the endpoint's to say.
func (a API) EffortProblem(effort string) string {
	if effort == "" {
		return ""
	}
	if a.EffortParameter == "" {
		return "api_effort_parameter_required"
	}
	if len(effort) > 32 || strings.IndexFunc(effort, func(r rune) bool { return (r < 'a' || r > 'z') && r != '_' && r != '-' }) >= 0 {
		return "api_effort_invalid"
	}
	return ""
}
