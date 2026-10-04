//go:build !darwin

package testenv

// Linux callers use RequireBwrap with the library's full-flags trial.
func probeNestedSandbox() error { return nil }
