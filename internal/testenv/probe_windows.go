package testenv

// Windows has no process groups in this sense, and the tests that need a Unix
// domain socket or ps are Unix-only, so nothing is probed there.

func probeUnixSocket() error    { return nil }
func probeProcessGroup() error  { return nil }
func probeProcessStatus() error { return nil }
func probeGroupPriority() error { return nil }
