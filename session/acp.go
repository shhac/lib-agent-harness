package session

import (
	"encoding/json"
	"errors"
	"regexp"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/rawjson"
)

// Grok, Command Code and fx all speak the Agent Client Protocol over JSON-RPC.
// What their refusals mean is shared; how each states them differs, so each
// engine classifies its own error replies into these codes.

// acpRefusal is a JSON-RPC error from an ACP agent, classified into a fixed
// code. The agent's own message and data are read only to classify it and are
// never kept.
type acpRefusal struct {
	engine harness.Engine
	rpc    int
	code   string
}

// Refusal codes every ACP agent can produce. Each engine adds its own.
const (
	acpMethodMissing       = "method_not_found"
	acpAuthRequired        = "authentication_required"
	acpAuthFailed          = "authentication_failed"
	acpRateLimited         = "rate_limited"
	acpTimedOut            = "timeout"
	acpProviderUnavailable = "provider_unavailable"
	acpProviderRejected    = "provider_rejected"
	acpRejected            = "rejected"
)

func (e *acpRefusal) Error() string {
	name := "grok"
	switch e.engine {
	case harness.CommandCode:
		name = "command code"
	case harness.Fx:
		name = "fx"
	}
	return name + " refused the request: " + e.code
}

func (e *acpRefusal) Unwrap() error {
	if e.rpc == -32601 {
		return ErrUnsupported
	}
	return ErrRejected
}

func (e *acpRefusal) HarnessFacts() harness.Facts {
	facts := harness.Facts{Engine: e.engine, Operation: harness.Session, Family: harness.FailureRequest, Cause: harness.CauseUnknown, Code: e.code}
	switch e.code {
	case acpMethodMissing:
		facts.Family, facts.Cause = harness.FailureCapability, ""
	case acpAuthRequired, acpAuthFailed:
		facts.Cause = harness.CauseAuthentication
	case acpRateLimited:
		facts.Cause = harness.CauseRateLimited
	case acpTimedOut:
		facts.Cause = harness.CauseTimeout
	case acpProviderUnavailable:
		facts.Cause = harness.CauseUnavailable
	}
	return facts
}

// acpStatusCode classifies the provider's HTTP status an agent passes on for
// a failed run, or "" when it states none.
func acpStatusCode(status int) string {
	switch {
	case status == 401 || status == 403:
		return acpAuthFailed
	case status == 429:
		return acpRateLimited
	case status == 408 || status == 504:
		return acpTimedOut
	case status >= 500:
		return acpProviderUnavailable
	case status >= 400:
		return acpProviderRejected
	}
	return ""
}

// acpAgent is what initialize established about an installed ACP agent.
type acpAgent struct{ resume, list bool }

// parseACPAgent reads initialize's answer, refusing any protocol version but
// the one the engine was verified against.
func parseACPAgent(raw json.RawMessage, version int) (acpAgent, error) {
	var r struct {
		ProtocolVersion *int `json:"protocolVersion"`
		Capabilities    struct {
			Session struct {
				Resume json.RawMessage `json:"resume"`
				List   json.RawMessage `json:"list"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	if json.Unmarshal(raw, &r) != nil || r.ProtocolVersion == nil || *r.ProtocolVersion != version {
		return acpAgent{}, ErrProtocol
	}
	return acpAgent{resume: !rawjson.Absent(r.Capabilities.Session.Resume), list: !rawjson.Absent(r.Capabilities.Session.List)}, nil
}

// acpToolIdentifier is a tool name or title safe to report as an identifier.
var acpToolIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// acpEnvelope frames a request as JSON-RPC 2.0, as every ACP agent reads it.
func acpEnvelope(id, method string, params map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

// Refusal codes two or more ACP agents state the same way.
const (
	acpInvalidParams   = "invalid_params"
	acpSessionNotFound = "session_not_found"
)

// acpClassifier turns an ACP agent's JSON-RPC error into a fixed refusal
// code. The message and data are read only to classify it, and never kept.
type acpClassifier func(code int, message string, data json.RawMessage) string

// acpDialect is an ACP agent's wire: replies are read as JSON-RPC and an error
// classified by the engine's own rule, and a request from the agent is
// answered by permission, which answers a permission request's params.
func acpDialect(engine harness.Engine, classify acpClassifier, permission func(params json.RawMessage) map[string]any) dialect {
	return dialect{
		envelope: acpEnvelope,
		parseReply: func(m map[string]json.RawMessage) (string, response, bool, error) {
			return parseACPReply(m, engine, classify)
		},
		answer: func(m map[string]json.RawMessage) (map[string]any, bool) {
			if !jsonRPCRequest(m) {
				return nil, false
			}
			return acpServerReply(m, permission), true
		},
	}
}

// parseACPReply reads a JSON-RPC response, then classifies an error reply from
// what the agent states in fixed fields.
func parseACPReply(m map[string]json.RawMessage, engine harness.Engine, classify acpClassifier) (string, response, bool, error) {
	id, r, isReply, err := parseJSONRPCReply(m)
	if !isReply || err != nil || r.err == nil {
		return id, r, isReply, err
	}
	var e struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(m["error"], &e) != nil {
		return "", response{}, true, ErrProtocol
	}
	return id, response{err: &acpRefusal{engine: engine, rpc: e.Code, code: classify(e.Code, e.Message, e.Data)}}, true, nil
}

// acpServerReply answers a request from the agent. Only a permission request
// is answered, and by the session's policy; anything else is refused with a
// JSON-RPC error rather than left unanswered.
func acpServerReply(m map[string]json.RawMessage, permission func(params json.RawMessage) map[string]any) map[string]any {
	reply := map[string]any{"jsonrpc": "2.0", "id": m["id"]}
	if str(m, "method") != "session/request_permission" {
		reply["error"] = refusedOperation()
		return reply
	}
	reply["result"] = map[string]any{"outcome": permission(m["params"])}
	return reply
}

// acpStrictPermissionOutcome is Command Code's and fx's answer to a permission
// request: the allow-once option when allow is set and the reject-once option
// otherwise, and only when the request offers both, never an "always" option,
// which would grant or refuse the tool for the rest of the session. A tool
// that would change the permission mode is rejected under either policy:
// allowing it would let every later change run without asking. A request that
// offers two options of the same kind or the same id is answered cancelled,
// since a shared id would let a deny be read as an allow; so is a question
// asked through the same method, which offers only allow-once options, one
// per answer, and anything unreadable. No answer and no permission is ever
// guessed.
func acpStrictPermissionOutcome(raw json.RawMessage, allow bool) map[string]any {
	var p struct {
		ToolCall struct {
			Kind string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	cancelled := map[string]any{"outcome": "cancelled"}
	if json.Unmarshal(raw, &p) != nil {
		return cancelled
	}
	chosen := map[string]string{}
	ids := map[string]bool{}
	for _, option := range p.Options {
		if option.ID == "" {
			continue
		}
		if _, seen := chosen[option.Kind]; seen {
			return cancelled
		}
		if ids[option.ID] {
			return cancelled
		}
		chosen[option.Kind] = option.ID
		ids[option.ID] = true
	}
	allowOnce, rejectOnce := chosen["allow_once"], chosen["reject_once"]
	if allowOnce == "" || rejectOnce == "" {
		return cancelled
	}
	if allow && p.ToolCall.Kind != "switch_mode" {
		return map[string]any{"outcome": "selected", "optionId": allowOnce}
	}
	return map[string]any{"outcome": "selected", "optionId": rejectOnce}
}

// acpSessionGone reports whether a refusal means the agent no longer holds the
// conversation. Only an engine that resumes by falling back to a fresh
// conversation asks.
func acpSessionGone(err error) bool {
	var refusal *acpRefusal
	return errors.As(err, &refusal) && refusal.code == acpSessionNotFound
}
