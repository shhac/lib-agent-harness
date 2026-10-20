//go:build !linux

package sandbox

import "os"

func workbenchSystemDirs() []string {
	return []string{"/System", "/usr", "/bin", "/sbin", "/Library/Developer/CommandLineTools", "/opt/homebrew"}
}

func workbenchSystemContains(system, dir string) bool {
	if system == "/System" && Within("/System/Volumes/Data", dir) {
		return false
	}
	if Within(system, dir) {
		return true
	}
	// APFS firmlinks need not be resolved by EvalSymlinks. Identity-based
	// ancestry catches a runtime reached through the data-volume spelling.
	if _, err := os.Stat(system); err != nil {
		return false
	}
	return Nested(system, dir)
}
