package session

import (
	"encoding/json"
	"regexp"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/rawjson"
)

// Grok and Command Code both speak the Agent Client Protocol over JSON-RPC.
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
	if e.engine == harness.CommandCode {
		name = "command code"
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
