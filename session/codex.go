package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
)

// Codex's session adapter: its launch arguments, policy and environment, and
// the app-server conversation's start, turns, interruption and steering. Its
// wire dialect, events, sandbox and telemetry have files of their own.

// codexHomeValue gives a restricted or sandboxed session its own runtime
// home: the library wrote that home's configuration and shared the login into
// it, so nothing the operator keeps beside their credential comes along.
func codexHomeValue(o Options) (string, bool) {
	if (o.Restriction != nil || o.Sandbox != nil) && o.RuntimeHome != "" {
		return o.RuntimeHome, true
	}
	return o.Provider.CLI.Home, true
}

func refuseCodexAddition(o Options, key, upper string) error {
	if o.Sandbox != nil && o.Browser && (strings.HasPrefix(upper, "NODE_REPL_") || strings.HasPrefix(upper, "BROWSER_USE_") || strings.HasPrefix(upper, "SKY_")) {
		return refuse(o, "env", RefusedEnvManaged, "environment addition "+key+" would change the sandboxed browser outside its proven configuration")
	}
	return nil
}

// codexArgs runs Codex's app server on stdio.
func codexArgs(o Options, _ string, _ bool, l *launch) []string {
	args := []string{"app-server", "--listen", "stdio://"}
	if o.Browser {
		args = append(args, nativecli.CodexBrowserArgs()...)
	}
	if l != nil {
		args = append(args, l.extra...)
	}
	return args
}

// codexRestrictedLaunch runs a restricted Codex session in a home this library
// owns, with the operator's login shared into it: their own home keeps its
// servers, hooks and trust settings, and none of it reaches the worker. The
// model catalog is narrowed to the requested model and effort.
func codexRestrictedLaunch(ctx context.Context, o Options, host *toolHost) (Options, []string, error) {
	if _, err := prepareRuntimeHome(o.Provider.CLI.Home, o.RuntimeHome); err != nil {
		return o, nil, err
	}
	if err := syncRuntimeSkills(o); err != nil {
		return o, nil, err
	}
	catalog, err := readCodexCatalog(ctx, o)
	if err != nil {
		return o, nil, err
	}
	restricted, effort, err := restrictedCatalogFor(catalog, o.Model, o.Effort)
	if err != nil {
		return o, nil, err
	}
	o.Effort = effort
	catalogFile := catalogPath(host.cfg.Dir)
	if err = writePrivate(catalogFile, restricted); err != nil {
		return o, nil, err
	}
	args, err := codexRestrictedArgs(host, catalogFile)
	return o, args, err
}

func normalizeCodexPolicy(o Options) (Options, error) {
	o = legacyPolicyDefaults(o)
	switch o.Policy.CodexSandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return o, refuse(o, "policy", RefusedPolicy, "invalid Codex sandbox policy")
	}
	switch o.Policy.CodexApproval {
	case "never", "on-request", "untrusted":
	default:
		return o, refuse(o, "policy", RefusedPolicy, "invalid Codex approval policy")
	}
	return o, nil
}

// initializeCodex starts or resumes the thread, and reads back the sandbox and
// browser the thread actually runs with before any turn.
func (s *Session) initializeCodex(ctx context.Context, resume bool) error {
	if err := codexHandshake(ctx, s.transport, s.options.Sandbox != nil); err != nil {
		return err
	}
	method := "thread/start"
	if resume {
		method = "thread/resume"
	}
	body, err := s.transport.request(ctx, method, codexThreadParams(s.options, s.options.WorkDir, resume, s.ref.ID))
	if err != nil {
		if resume && errors.Is(err, ErrRejected) {
			// Checked against codex 0.156.1: a thread it has no rollout for
			// is refused with an invalid-request error.
			return fmt.Errorf("%w: %w", errConversationGone, err)
		}
		return err
	}
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(body, &response) != nil || response.Thread.ID == "" {
		return ErrProtocol
	}
	if resume && response.Thread.ID != s.ref.ID {
		return ErrProtocol
	}
	if s.options.Sandbox != nil {
		if err = checkCodexSandbox(s.options, body); err != nil {
			return err
		}
	}
	if s.options.Browser {
		if err = checkCodexBrowser(ctx, s.transport, response.Thread.ID); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.ref.ID = response.Thread.ID
	s.mu.Unlock()
	return nil
}

// startCodexTurn asks the server to open the turn and adopts the id it assigns.
func (s *Session) startCodexTurn(ctx context.Context, t *Turn, ref Ref, in Input) error {
	body, err := s.transport.request(ctx, "turn/start", codexTurnParams(ref.ID, in.Text, s.options.Effort))
	if err != nil {
		return err
	}
	var r struct {
		Turn struct{ ID string } `json:"turn"`
	}
	if json.Unmarshal(body, &r) != nil || r.Turn.ID == "" {
		return ErrProtocol
	}
	s.replayStarting(t, r.Turn.ID)
	return nil
}

// startCodexTurnSynced brings the shared login up to date before the turn, so
// concurrent workers sharing refresh tokens never start on a stale one.
func (s *Session) startCodexTurnSynced(request, _ context.Context, t *Turn, ref Ref, in Input) error {
	s.syncLogin()
	return s.startCodexTurn(request, t, ref, in)
}

func (s *Session) interruptCodex(ctx context.Context, expected string) error {
	_, err := s.transport.request(ctx, "turn/interrupt", map[string]any{"threadId": s.Ref().ID, "turnId": expected})
	return err
}

// steerCodex is Codex's native turn/steer, which keeps the running turn.
func (s *Session) steerCodex(ctx context.Context, t *Turn, expected string, in Input) (SteerResult, error) {
	body, err := s.transport.request(ctx, "turn/steer", map[string]any{"threadId": s.Ref().ID, "expectedTurnId": expected, "input": codexInput(in.Text)})
	if err != nil {
		return SteerResult{}, s.controlFailed(opSteer, nil, err)
	}
	var reply struct {
		TurnID string `json:"turnId"`
	}
	if json.Unmarshal(body, &reply) != nil || reply.TurnID != expected {
		s.fail(ErrProtocol)
		return SteerResult{}, ErrProtocol
	}
	s.setCapability(&s.caps.Steer, harness.Capability{Availability: harness.Native, Reason: "turn/steer acknowledged by installed harness"})
	return SteerResult{harness.Native, t}, nil
}
