//go:build unix

package process

import "syscall"

func leaveGroup() { _ = syscall.Setpgid(0, 0) }
