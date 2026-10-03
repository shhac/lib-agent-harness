// Package sandbox owns confined workspace file access and shared sandbox path
// rules. It has no session, model, account or transport dependencies.
//
// OpenWorkspace returns one Workspace rooted at a caller-selected directory. Read,
// List, Search, Write and Edit accept the same JSON arguments as the hosted
// workbench tools. Definitions describes those tools without granting their
// admission: callers decide which operations to offer and authorize.
//
// A single worker serializes file tools. Cancellation waits for actual I/O
// settlement; a syscall that exceeds its grace closes admission and reports
// CommandError with WorkspaceIOStuck. Interrupted writes can have unknown
// outcomes, never implied rollback. Close is void and idempotent; callers can
// inspect Stuck after shutdown.
//
// This package's API is provisional during the staged extraction. Command
// profiles, proofs, execution and the standalone command API still live in
// session. No new harness.Support claim follows from this package boundary.
package sandbox
