//go:build darwin || linux

package sandbox

import (
	"runtime"

	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

// interfaceCanary uses a disposable native socket client, not inference.
// Darwin uses base-system Perl; Linux LocalOnly retains its Python client.
// Each attempt reports its actual errno; failures cannot look like a denial.
func interfaceCanary(client string, attempts []interfaceAttempt) string {
	if runtime.GOOS == "darwin" {
		return interfacePerlCanary(attempts)
	}
	return sandboxprobe.InterfacePythonCanary(client, attempts, false)
}

func interfacePerlCanary(attempts []interfaceAttempt) string {
	return sandboxprobe.InterfacePerlCanary(attempts)
}
