//go:build darwin || linux

package session

import (
	"strings"

	harness "github.com/shhac/lib-agent-harness"
)

type workbenchLayout struct {
	Work, Home, Tmp string
	Read, System    []string
	Write, Loopback bool
}

func workbenchProbeEnvironment(l workbenchLayout) []string {
	return []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + l.Home, "TMPDIR=" + l.Tmp}
}

func workbenchCapability(code string) *CapabilityError {
	return &CapabilityError{Engine: harness.OpenAICompatible, Code: code, Phase: BeforeLaunch}
}
func workbenchShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
