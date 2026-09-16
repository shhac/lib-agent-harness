// Package process contains subprocess trees for local CLI harnesses.
// Unix children use a separate process group; Windows children start suspended
// and are attached to a kill-on-close job before their first instruction runs.
// Callers own the context, streams, and wait timeout, and must Close the handle.
package process
