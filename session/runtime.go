package session

import (
	"encoding/json"
	"errors"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sharedlogin"
)

// A restricted or sandboxed Codex session needs two things that pull in
// opposite directions: a configuration nobody else can add to, and the
// operator's existing login.
//
// Taking the operator's home gives the login and everything else in it —
// servers, hooks, plugins, project trust — and no flag on the installed Codex
// build removes them: `--ignore-user-config` does not exist on `app-server`,
// and overriding the table with `-c mcp_servers={}` was observed not to clear
// entries already declared there. Refusing any home that contains such settings
// would reject nearly every real one, which is not a boundary, it is a demand
// that people dismantle their own tools.
//
// So the session gets its own durable runtime home, with configuration this
// library writes, and shares exactly one thing from the source home: the
// credential. internal/sharedlogin decides which copy is authoritative; this
// file says what a Codex runtime home holds and how its failures read here.

const (
	codexConfigFile     = "config.toml"
	codexCredentialFile = "auth.json"
	// sharedLoginRecord remembers which source credential this runtime home was
	// given, so a later refresh can be told apart from a changed account.
	sharedLoginRecord = sharedlogin.Record
)

// runtimeConfig is the whole configuration a restricted session runs with. It
// is deliberately tiny: everything that matters is passed as explicit overrides
// at launch, and what is here exists so the home is well-formed rather than
// inherited.
const runtimeConfig = "# Written by lib-agent-harness for a restricted session.\n" +
	"# Configuration for these sessions is supplied at launch; edits here are\n" +
	"# replaced, and inherited settings are deliberately not read.\n"

func codexRuntime(source, runtime string) sharedlogin.Home {
	return sharedlogin.Home{
		Source:     source,
		Runtime:    runtime,
		Credential: codexCredentialFile,
		Files:      map[string][]byte{codexConfigFile: []byte(runtimeConfig)},
		Valid:      codexLoginValid,
		Account:    codexAccount,
	}
}

// codexLoginValid accepts only a whole auth.json. Codex may rewrite the file
// in place, so a copy taken mid-write would otherwise be shared.
func codexLoginValid(data []byte) bool {
	var login map[string]json.RawMessage
	return json.Unmarshal(data, &login) == nil && login != nil
}

// codexAccount is the ChatGPT account a login belongs to; empty for an API
// key login, which names none.
func codexAccount(data []byte) string {
	var login struct {
		Tokens *struct {
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &login) != nil || login.Tokens == nil {
		return ""
	}
	return login.Tokens.Account
}

// syncLogin reconciles a running Codex session's login with its source home
// between turns (see sharedlogin.Home.Sync), so a refresh one session makes
// reaches the others before they spend the same refresh token. A failure is
// reported, never fatal to the turn: the session still holds a login.
func (s *Session) syncLogin() {
	o := s.options
	if o.Provider.Engine != harness.Codex || o.RuntimeHome == "" || (o.Restriction == nil && o.Sandbox == nil) {
		return
	}
	err := codexRuntime(o.Provider.CLI.Home, o.RuntimeHome).Sync()
	var shared *sharedlogin.Error
	if err == nil || (errors.As(err, &shared) && shared.Code == sharedlogin.CodeUnsupported) {
		return
	}
	code := "login_sync_failed"
	if shared != nil {
		code = shared.Code
	}
	if report := o.OnDiagnostic; report != nil {
		report(Diagnostic{Engine: harness.Codex, Stage: "login_sync", Code: code, At: time.Now().UTC()})
	}
}

// prepareRuntimeHome makes the private home a restricted session runs in and
// shares the source login into it. It returns the home to launch with.
func prepareRuntimeHome(source, runtime string) (string, error) {
	if err := codexRuntime(source, runtime).Prepare(); err != nil {
		return "", runtimeFailure(err)
	}
	return runtime, nil
}

// writeBackCredential returns a refreshed login to the source home, unless the
// source has since changed or been removed: a newer login or a logout wins
// over a worker's copy.
func writeBackCredential(source, runtime string) error {
	return runtimeFailure(codexRuntime(source, runtime).WriteBack())
}

// credentialDigest identifies a credential file without revealing it. A
// missing file is nil; anything else that cannot be read is an error.
func credentialDigest(path string) ([]byte, error) {
	digest, err := sharedlogin.Digest(path)
	return digest, runtimeFailure(err)
}

// runtimeFailure states a shared-login failure in this package's terms. A
// missing login is the one an operator acts on, so it is a capability error
// that says which home to log in to.
func runtimeFailure(err error) error {
	var shared *sharedlogin.Error
	if !errors.As(err, &shared) {
		return err
	}
	switch shared.Code {
	case sharedlogin.CodeLoginUnavailable:
		return &CapabilityError{Engine: harness.Codex, Code: CapabilityLoginUnavailable, Phase: BeforeLaunch}
	case sharedlogin.CodeRuntimeRequired:
		return errors.New("a restricted session requires a durable runtime home; set Options.RuntimeHome")
	case sharedlogin.CodeRuntimeIsSource:
		return errors.New("a restricted session's runtime home must be separate from the login source home")
	case sharedlogin.CodeConfigWrite:
		return errors.New("restricted session runtime configuration could not be written")
	case sharedlogin.CodeLoginUnreadable:
		return errors.New("harness login could not be read")
	case sharedlogin.CodeLoginShare:
		return errors.New("harness login could not be shared")
	case sharedlogin.CodeUnsupported:
		return errors.New("restricted session runtime homes are not available on this platform")
	}
	return errors.New("restricted session runtime home is not a usable directory")
}
