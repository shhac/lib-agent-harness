package testenv

import (
	"context"
	"testing"
)

// RequireBwrap runs the caller's library version and full-flags trial check.
// Only that prerequisite check belongs here; a failed generated canary must
// fail the test after a successful trial, never be mistaken for a skip.
// The callback marks recognized environment refusals with fs.ErrPermission;
// internal failures are returned unchanged so they fail instead of skipping.
func RequireBwrap(t testing.TB, ctx context.Context, check func() error) {
	t.Helper()
	var err error
	if ctx.Err() == nil {
		err = check()
	}
	// An interrupted trial is incomplete evidence, even if the CLI returned
	// a recognized refusal code or a permission error.
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	require(t, "bubblewrap 0.8.0+ and unprivileged namespaces", err)
}
