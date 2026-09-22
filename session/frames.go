package session

import "context"

// The request shapes a session sends to start its harness and its turns. The
// capability probe sends them too, because what it proves is only worth
// anything for a launch that is the same one; building both from one copy is
// what makes that equivalence structural rather than two functions agreeing.

// codexHandshake opens the app-server protocol.
func codexHandshake(ctx context.Context, w wire) error {
	if _, err := w.request(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "lib-agent-harness", "version": "1"}, "capabilities": map[string]any{}}); err != nil {
		return err
	}
	return w.send(ctx, map[string]any{"method": "initialized"})
}

// codexThreadParams starts, or resumes by id, a thread in cwd under the
// session's policy, model and instructions.
func codexThreadParams(o Options, cwd string, resume bool, id string) map[string]any {
	p := map[string]any{"cwd": cwd, "approvalPolicy": o.Policy.CodexApproval, "sandbox": o.Policy.CodexSandbox}
	if o.Model != "" {
		p["model"] = o.Model
	}
	switch o.Instructions.Mode {
	case Replace:
		p["baseInstructions"] = o.Instructions.Text
	case Append:
		p["developerInstructions"] = o.Instructions.Text
	}
	if resume {
		p["threadId"] = id
	}
	return p
}

// codexTurnParams starts one text turn on a thread.
func codexTurnParams(thread, text, effort string) map[string]any {
	p := map[string]any{"threadId": thread, "input": codexInput(text)}
	if effort != "" {
		p["effort"] = effort
	}
	return p
}

func codexInput(text string) []any {
	return []any{map[string]any{"type": "text", "text": text}}
}

// claudeUserFrame is one user turn on Claude's stream-json input.
func claudeUserFrame(session, text string) map[string]any {
	return map[string]any{"type": "user", "session_id": session, "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": text}}
}
