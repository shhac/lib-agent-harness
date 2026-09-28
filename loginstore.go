package harness

import "github.com/shhac/lib-agent-harness/internal/loginstore"

// CodeKeychainUnavailable is the code every operation fails with, in the
// preflight family and not retryable, instead of launching an engine whose
// login lives in a locked keychain. Nothing is started, so no unlock prompt is
// raised. Wait until LoginStoreLocked is false before trying again.
const CodeKeychainUnavailable = "keychain_unavailable"

// LoginStoreLocked reports whether launching e now would make it read a
// locked OS keychain. Today that is Claude on macOS. It never prompts and
// takes about a millisecond, so an application can poll it to know when to
// resume polling quota or discovery.
func LoginStoreLocked(e Engine) bool { return loginstore.Locked(e == Claude) }
