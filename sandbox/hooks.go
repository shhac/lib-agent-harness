package sandbox

import (
	"context"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
)

func init() {
	sandboxhook.Access = func(value any) sandboxhook.Hooks {
		w := value.(*Workspace)
		return sandboxhook.Hooks{Root: w.root, WriteFault: &w.writeFault, Step: &w.step, OpenStep: &w.openStep, DirFault: &w.dirFault, Listed: &w.listed, MaxVisited: &w.maxVisited, Grace: &w.grace, Handles: &w.handles, Stopped: w.stopped, SetStuck: func(code string) { w.stuck.Store(&CommandError{Code: code}) }}
	}
	sandboxhook.MountID = &workspaceMountID

	sandboxhook.NewCommandSandbox = func(files, runner any, dir string, loopback bool) any {
		return &Sandbox{ws: files.(*Workspace), commands: runner.(*Runner), dir: dir, loopback: loopback, active: make(map[*StartedCommand]struct{})}
	}
	sandboxhook.RecordCommandProof = verified.record
	sandboxhook.HasCommandProof = verified.holds
	sandboxhook.RunnerAccess = func(value any) sandboxhook.RunnerHooks {
		r := value.(*Runner)
		return sandboxhook.RunnerHooks{Close: &r.close, SetExecute: func(f any) {
			r.execute = f.(func(context.Context, string, string, time.Duration, func()) (CommandResult, error))
		}}
	}
	sandboxhook.ResetCommandCache = func() { verified.mu.Lock(); defer verified.mu.Unlock(); verified.seen = map[string]bool{} }
	sandboxhook.CommandCacheSize = func() int { verified.mu.Lock(); defer verified.mu.Unlock(); return len(verified.seen) }
}
