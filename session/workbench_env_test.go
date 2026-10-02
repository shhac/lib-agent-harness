package session

import "testing"

// Ordinary settings reach commands; what belongs to the library, acts before
// the sandbox, runs ahead of the supervisor or looks like a credential never
// does.
func TestWorkbenchEnvRefusal(t *testing.T) {
	for _, entry := range []string{
		"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C",
		"GOMODCACHE=/cache/mod", "GOCACHE=/cache/build", "GOPROXY=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod",
		"npm_config_cache=/cache/npm", "XDG_CACHE_HOME=/cache/xdg", "CI=1", "PORT=4321", "KEYBOARD_LAYOUT=us", "EMPTY=",
	} {
		if why := workbenchEnvRefusal(entry); why != "" {
			t.Errorf("%s refused: %s", entry, why)
		}
	}
	for _, entry := range []string{
		"", "NOVALUE", "=value", "1PORT=1", "BAD-NAME=1", "NUL=a\x00b", "BASH_FUNC_ls%%=() { true; }",
		"HOME=/elsewhere", "TMPDIR=/elsewhere",
		"AGENT_HARNESS_LAUNCH=x", "AGENT_HARNESS_TOOL_SOCKET=/tmp/s",
		"DYLD_INSERT_LIBRARIES=/tmp/x.dylib", "LD_PRELOAD=/tmp/x.so", "LD_LIBRARY_PATH=/tmp",
		"BASH_ENV=/tmp/rc", "ENV=/tmp/rc", "SHELLOPTS=xtrace", "BASHOPTS=extglob", "IFS=x", "CDPATH=/",
		"API_KEY=secret", "OPENAI_API_KEY=sk", "GITHUB_TOKEN=t", "aws_secret_access_key=s", "DB_PASSWORD=p", "SSH_AUTH_SOCK=/tmp/agent", "GOOGLE_APPLICATION_CREDENTIALS=/k.json",
	} {
		if workbenchEnvRefusal(entry) == "" {
			t.Errorf("%q accepted", entry)
		}
	}
}
