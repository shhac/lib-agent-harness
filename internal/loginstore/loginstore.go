// Package loginstore answers whether an engine's login lives in an OS secret
// store that is locked right now, without asking the store anything that
// would prompt.
package loginstore

import (
	"runtime"

	keyring "github.com/shhac/lib-agent-keyring"
)

// Status is the host store's state. Tests replace it.
var Status = keyring.HostStatus

// Locked reports whether launching claude would make it read a locked store.
// Claude Code keeps its login in the macOS login keychain, and every process
// reads it at startup; with the keychain locked each one raises its own unlock
// prompt, and several at once have wedged SecurityAgent. Codex and Grok keep
// their logins in files by default.
func Locked(claude bool) bool {
	return claude && runtime.GOOS == "darwin" && Status() == keyring.Locked
}
