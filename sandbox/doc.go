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
// Open proves Seatbelt (macOS) or bubblewrap (Linux) before creating command
// recovery state, then returns a Sandbox with Run, Start and Close. Command
// cancellation and Close await process-tree settlement; failed cleanup preserves
// recovery markers. Windows command sandboxes are refused before launch.
//
// Prove and NewRunner expose the same mechanism to hosted workbench callers.
// Proof carries frozen system directories and binary identity, with a separate
// process-local cache from native CLI probes. Linux always re-proves.
// No new harness.Support claim follows from this package boundary.
package sandbox
