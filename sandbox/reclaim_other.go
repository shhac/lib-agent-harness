//go:build !darwin && !linux

package sandbox

import "os"

func removeCommandTree(path string) error { return os.RemoveAll(path) }
