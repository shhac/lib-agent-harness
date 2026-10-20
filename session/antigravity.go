package session

// Antigravity's stream-json session protocol over the stdio of
// `agy -p= --input-format=stream-json --output-format=stream-json`. Every flag
// and frame shape here was checked against agy 0.2.2 / Antigravity 1.4.0:
//
//   - `-p=` is required with `=`, because commander parses `-p [prompt]` with
//     an optional argument and otherwise consumes the following flag as the
//     prompt text.
//   - On startup, before any stdin line is read, agy emits a single `init`
//     event on stdout carrying `conversation_id` and `init.model`, `init.cwd`,
//     `init.tools` and `init.permission_mode`.
//   - When `--conversation=<id>` names a conversation agy does not have, agy
//     warns on stderr and creates a fresh conversation with a new
//     `conversation_id`. A resumed session checks that `init.conversation_id`
//     matches the reference's ID and reports `errConversationGone` when it does
//     not.
//   - When `--model=<id>` is not honoured by the login or installation, agy
//     substitutes its default model in `init.model`; `initializeAntigravity`
//     checks `init.model` against `Options.Model` before any prompt is sent.
//   - Each user turn is sent as `{"event":"user","message":{"content":...}}`
//     and streams `step_update` events followed by a terminal `result` event.
//
// Rechecked against agy 1.4.2:
//
//   - `--mode` accepts accept-edits, auto and plan. Its default, which `init`
//     reports as `request-review`, is not a `--mode` value: agy warns and
//     carries on in its configured default. So request-review passes no flag,
//     and `init.permission_mode` must name the policy before any prompt.
//   - In request-review, a tool that needs approval is auto-denied, never
//     held for an answer; the result lists it in `denied_actions`.
//   - A result's usage covers every turn of the conversation so far. A turn's
//     settled usage is the sum of its own agent_response steps.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
)

func antigravityArgs(o Options, nativeID string, resuming bool) []string {
	args := []string{"-p=", "--input-format=stream-json", "--output-format=stream-json"}
	if o.Policy.AntigravityPermission != AntigravityRequestReview {
		args = append(args, "--mode="+o.Policy.AntigravityPermission)
	}
	if resuming {
		args = append(args, "--conversation="+nativeID)
	}
	if o.Model != "" {
		args = append(args, "--model="+o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort="+o.Effort)
	}
	return args
}

// antigravityHome is the only home an Antigravity session can use: `~/.gemini`,
// which `agy` derives from the user's home directory. A caller that names
// another is refused rather than silently given this one.
func antigravityHome(o Options) (Options, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return o, refuse(o, "home", RefusedHome, "home directory unavailable")
	}
	fixed := filepath.Join(home, ".gemini")
	cli := &o.Provider.CLI
	if cli.Home != "" {
		if cli.Home, err = filepath.Abs(cli.Home); err != nil || filepath.Clean(cli.Home) != fixed {
			return o, refuse(o, "home", RefusedHome, "Antigravity reads its configuration and login from ~/.gemini, and no variable moves it; leave Provider.CLI.Home empty")
		}
	}
	cli.Home = fixed
	return o, nil
}

// unsupportedAntigravity refuses what agy stream-json has no way to carry, so
// that nothing a caller asked for is silently dropped.
func unsupportedAntigravity(o Options) error {
	engine := harness.Antigravity
	if o.Instructions.Mode != "" || o.Instructions.Text != "" {
		feature := harness.AppendInstructions
		if o.Instructions.Mode == Replace {
			feature = harness.ReplaceInstructions
		}
		if c := harness.Support(engine, harness.Session, feature); !c.Usable() {
			return &UnsupportedError{Engine: engine, Operation: "instructions", Code: RefusedNotOffered, Capability: c}
		}
	}
	if len(o.Skills.Provided) > 0 {
		if c := harness.Support(engine, harness.Session, harness.ProvidedSkills); !c.Usable() {
			return &UnsupportedError{Engine: engine, Operation: "skills", Code: RefusedNotOffered, Capability: c}
		}
	}
	return nil
}

func normalizeAntigravityPolicy(o Options) (Options, error) {
	switch o.Policy.AntigravityPermission {
	case "":
		o.Policy.AntigravityPermission = AntigravityRequestReview
	case AntigravityRequestReview, AntigravityAcceptEdits, AntigravityAuto, AntigravityPlan:
	default:
		return o, refuse(o, "policy", RefusedPolicy, "invalid Antigravity permission policy")
	}
	return o, nil
}

// antigravityManaged is what an Antigravity session neither inherits nor
// accepts as an addition: provider credentials and endpoint/telemetry switches.
func antigravityManaged(key string) bool {
	upper := strings.ToUpper(key)
	return strings.HasPrefix(upper, "ANTIGRAVITY_") ||
		strings.HasPrefix(upper, "GEMINI_") ||
		strings.HasPrefix(upper, "GOOGLE_GENAI_") ||
		upper == "GOOGLE_API_KEY" ||
		strings.HasPrefix(upper, "AGY_") ||
		strings.HasPrefix(upper, "JETSKI_")
}

func refuseAntigravityAddition(o Options, key, _ string) error {
	if antigravityManaged(key) {
		return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" would change how Antigravity runs outside the session's policy")
	}
	return nil
}

func antigravityOverrides(_ Options, env []string) []string {
	return nativecli.Override(env, nativecli.AntigravityReducedTelemetry...)
}

// antigravityDialect is Antigravity's stream-json wire: every incoming frame is
// an event (`init`, `step_update`, `error`, `result`), with no JSON-RPC request
// or reply correlation.
func antigravityDialect(Policy) dialect {
	return dialect{
		envelope: func(_ string, method string, params map[string]any) map[string]any {
			m := cloneMap(params)
			m["event"] = method
			return m
		},
		parseReply: func(map[string]json.RawMessage) (string, response, bool, error) {
			return "", response{}, false, nil
		},
		answer: func(map[string]json.RawMessage) (map[string]any, bool) {
			return nil, false
		},
	}
}

type antigravityInitState struct {
	conversationID string
	model          string
	permissionMode string
	err            error
}

func parseAntigravityInit(m map[string]json.RawMessage) antigravityInitState {
	var r struct {
		ConversationID string `json:"conversation_id"`
		Init           *struct {
			Model          string `json:"model"`
			PermissionMode string `json:"permission_mode"`
		} `json:"init"`
	}
	if json.Unmarshal(mustMarshal(m), &r) != nil || r.ConversationID == "" || r.Init == nil {
		return antigravityInitState{err: ErrProtocol}
	}
	return antigravityInitState{
		conversationID: r.ConversationID,
		model:          r.Init.Model,
		permissionMode: r.Init.PermissionMode,
	}
}

func (s *Session) antigravityInitChan() chan antigravityInitState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.antigravityInit == nil {
		s.antigravityInit = make(chan antigravityInitState, 1)
	}
	return s.antigravityInit
}

func (s *Session) observeAntigravityInit(m map[string]json.RawMessage) bool {
	if str(m, "event") != "init" {
		return false
	}
	select {
	case s.antigravityInitChan() <- parseAntigravityInit(m):
	default:
	}
	return true
}

func (s *Session) initializeAntigravity(ctx context.Context, resume bool) error {
	var state antigravityInitState
	select {
	case state = <-s.antigravityInitChan():
	case <-s.done:
		return s.transportFailure()
	case <-ctx.Done():
		return ctx.Err()
	}
	if state.err != nil {
		return state.err
	}
	s.mu.Lock()
	expectedID := s.ref.ID
	s.mu.Unlock()
	if resume && state.conversationID != expectedID {
		return fmt.Errorf("%w: %w", errConversationGone, ErrRejected)
	}
	if s.options.Model != "" && state.model != s.options.Model {
		return &CapabilityError{Engine: harness.Antigravity, Code: CapabilityChangedModel, Phase: BeforeFirstPrompt}
	}
	if state.permissionMode != s.options.Policy.AntigravityPermission {
		return &CapabilityError{Engine: harness.Antigravity, Code: CapabilityChangedPermissionMode, Phase: BeforeFirstPrompt}
	}
	s.mu.Lock()
	s.ref.ID = state.conversationID
	s.mu.Unlock()
	return nil
}

func (s *Session) startAntigravityTurn(request, _ context.Context, _ *Turn, _ Ref, in Input) error {
	return s.transport.send(request, map[string]any{
		"event":   "user",
		"message": map[string]any{"content": in.Text},
	})
}

func (s *Session) interruptAntigravity(context.Context, string) error {
	return s.notOffered("interrupt", func(c Capabilities) harness.Capability { return c.Interrupt })
}

func (s *Session) steerAntigravity(context.Context, *Turn, string, Input) (SteerResult, error) {
	return SteerResult{}, s.notOffered("steer", func(c Capabilities) harness.Capability { return c.Steer })
}
