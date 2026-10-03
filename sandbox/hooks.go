package sandbox

import "github.com/shhac/lib-agent-harness/internal/sandboxhook"

func init() {
	sandboxhook.MountID = &workspaceMountID
	sandboxhook.Access = func(value any) sandboxhook.Hooks {
		w := value.(*Workspace)
		return sandboxhook.Hooks{Root: w.root, WriteFault: &w.writeFault, Step: &w.step, OpenStep: &w.openStep, DirFault: &w.dirFault, Listed: &w.listed, MaxVisited: &w.maxVisited, Grace: &w.grace, Handles: &w.handles, Stopped: w.stopped, SetStuck: func(code string) { w.stuck.Store(&CommandError{Code: code}) }}
	}
}
