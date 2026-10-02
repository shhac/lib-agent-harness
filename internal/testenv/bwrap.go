package testenv

import "testing"

// RequireBwrap runs the caller's library version and full-flags trial check.
// Only that prerequisite check belongs here; a failed generated canary must
// fail the test after a successful trial, never be mistaken for a skip.
// The callback marks recognized environment refusals with fs.ErrPermission;
// internal failures are returned unchanged so they fail instead of skipping.
func RequireBwrap(t testing.TB, check func() error) {
	t.Helper()
	require(t, "bubblewrap 0.8.0+ and unprivileged namespaces", check())
}
