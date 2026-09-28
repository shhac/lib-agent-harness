// Package process contains subprocess trees for local CLI harnesses.
// Unix children use a separate process group, and carry a per-launch token in
// AGENT_HARNESS_LAUNCH that Stop and Close sweep for, so a descendant that left
// the group (a server an agent backgrounded) is stopped too; on macOS its
// unreadable platform-binary children are found through their parents. Windows
// children start suspended and are attached to a kill-on-close job before
// their first instruction runs.
// Callers own the context, streams, and wait timeout, and must Close the handle.
package process
