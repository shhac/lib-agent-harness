package native

// FxOptions are fx's own execution options.
type FxOptions struct {
	// PermissionMode selects how fx resolves the permission requests a run
	// makes: "ask", "auto" (fx reviews each request itself) or
	// "full-access" (fx makes no checks at all). Empty is the same as
	// "ask": fx ask passes no flag for it.
	//
	// A run started by this library has no terminal to ask on. In fx 0.0.14
	// an asking run ends at its first request, a write or a shell command,
	// with exit code 1 and the error NonInteractivePermissionRequired, even
	// when the operator's saved interactive mode is "auto". Reads and web
	// fetches do not ask. "full-access" is a widening of what the run may do
	// and is only ever used when the caller asked for it by name.
	PermissionMode string
}

func (o FxOptions) set() bool { return o.PermissionMode != "" }

// fxArgs builds the invocation for one noninteractive request.
//
// fx takes the request as trailing arguments, so the prompt is last and is
// preceded by "--", which makes every following argument prompt text whatever
// it begins with.
//
// fx states no output schema and no separate system prompt that appends to
// its own, so the caller's instructions travel with the request itself.
func fxArgs(c Config, r Request) []string {
	args := []string{"ask", "--json"}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	if c.Effort != "" {
		args = append(args, "--effort", c.Effort)
	}
	switch c.Fx.PermissionMode {
	case "", "ask":
	case "auto":
		args = append(args, "--auto")
	case "full-access":
		args = append(args, "--full-access")
	}
	if r.ResumeSession != "" {
		args = append(args, "--resume-id", r.ResumeSession)
	}
	args = append(args, c.Args...)
	return append(args, "--", fxPrompt(r))
}

// fxPrompt is the request as fx receives it. fx has no channel for
// instructions that are added to its own system prompt, so the caller's
// instructions lead the request text instead.
func fxPrompt(r Request) string {
	if r.AppendInstructions == "" {
		return r.Prompt
	}
	return r.AppendInstructions + "\n\n" + r.Prompt
}
