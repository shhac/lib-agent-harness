//go:build darwin || linux

package sandbox

import (
	"os"

	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

type workbenchLayout struct {
	Work, Home, Tmp string
	Read, System    []string
	Write, Loopback bool
	scratch         *os.Root
	scratchIdentity os.FileInfo
}

func workbenchProbeEnvironment(l workbenchLayout) []string {
	return []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + l.Home, "TMPDIR=" + l.Tmp}
}

func workbenchCapability(code string) *ProofError {
	return &ProofError{Code: code}
}
func workbenchShellQuote(s string) string { return sandboxprobe.ShellQuote(s) }
