//go:build !windows && !darwin && !linux

package process

import "time"

// Other systems get the process-group kill only.
func candidates(time.Time) []candidate { return nil }
func environment(int) []string         { return nil }
