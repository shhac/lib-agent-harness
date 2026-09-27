//go:build !windows

package sharedlogin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every credential here is a synthetic string in a temporary directory.

const credential = "auth.json"

func private(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func put(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func text(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, credential))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func home(source, runtime string) Home {
	return Home{Source: source, Runtime: runtime, Credential: credential, Files: map[string][]byte{"config.toml": []byte("# library\n")}, Valid: json.Valid}
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestRuntimeCannotBeTheSourceHome(t *testing.T) {
	source := private(t)
	put(t, source, credential, `"login"`)
	put(t, source, "config.toml", "owner")
	if code(home(source, source).Prepare()) != CodeRuntimeIsSource {
		t.Fatal("the source home was accepted as a runtime home")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	if code(home(source, alias).Prepare()) != CodeRuntimeIsSource {
		t.Fatal("a symlinked alias of the source home was accepted")
	}
	if raw, _ := os.ReadFile(filepath.Join(source, "config.toml")); string(raw) != "owner" {
		t.Fatal("the source configuration was replaced")
	}
}

func TestRuntimeOwnsItsConfigurationAndSharesOnlyTheLogin(t *testing.T) {
	source, runtime := private(t), private(t)
	put(t, source, credential, `"login"`)
	put(t, source, "config.toml", "[mcp_servers.owner]\n")
	put(t, source, "rules.md", "owner rules")
	put(t, runtime, "config.toml", "[mcp_servers.left_over]\n")
	if err := home(source, runtime).Prepare(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(runtime, "config.toml")); string(raw) != "# library\n" {
		t.Fatalf("runtime configuration %q", raw)
	}
	if _, err := os.Stat(filepath.Join(runtime, "rules.md")); !os.IsNotExist(err) {
		t.Fatal("something other than the login came across")
	}
	if text(t, runtime) != `"login"` {
		t.Fatal("the login was not shared")
	}
	info, err := os.Stat(filepath.Join(runtime, credential))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("shared login is not owner-only: %v %v", info, err)
	}
	raw, _ := os.ReadFile(filepath.Join(runtime, Record))
	if strings.Contains(string(raw), "login") {
		t.Fatal("the record holds the credential")
	}
}

func TestMissingSourceLoginIsRefused(t *testing.T) {
	if code(home(private(t), private(t)).Prepare()) != CodeLoginUnavailable {
		t.Fatal("a home without a login was accepted")
	}
}

func TestChangedSourceWinsAndRefreshSurvivesRestart(t *testing.T) {
	source, runtime := private(t), private(t)
	h := home(source, runtime)
	put(t, source, credential, `"first"`)
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	// The harness refreshed its copy, then the process died before write-back.
	put(t, runtime, credential, `"refreshed"`)
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	if text(t, runtime) != `"refreshed"` {
		t.Fatal("a restart discarded the harness's refresh")
	}
	if err := h.WriteBack(); err != nil {
		t.Fatal(err)
	}
	if text(t, source) != `"refreshed"` {
		t.Fatal("the refresh never reached the source")
	}
	// The operator logs in again: that wins over the runtime copy.
	put(t, source, credential, `"second"`)
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	if text(t, runtime) != `"second"` {
		t.Fatal("a changed source did not reach the runtime home")
	}
}

func TestWriteBackNeverOverwritesANewerLoginOrALogout(t *testing.T) {
	source, runtime := private(t), private(t)
	h := home(source, runtime)
	put(t, source, credential, `"original"`)
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	put(t, runtime, credential, `"worker-refresh"`)
	put(t, source, credential, `"owner-login"`)
	if err := h.WriteBack(); err != nil {
		t.Fatal(err)
	}
	if text(t, source) != `"owner-login"` {
		t.Fatal("a stale worker overwrote a newer login")
	}
	if err := os.Remove(filepath.Join(source, credential)); err != nil {
		t.Fatal(err)
	}
	if err := h.WriteBack(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, credential)); !os.IsNotExist(err) {
		t.Fatal("write-back undid a logout")
	}
}

// A credential caught halfway through a rewrite is neither shared nor
// written back.
func TestInvalidCredentialIsNeverCopied(t *testing.T) {
	source, runtime := private(t), private(t)
	h := home(source, runtime)
	put(t, source, credential, `"original"`)
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	put(t, runtime, credential, `{"trunc`)
	if code(h.WriteBack()) != CodeLoginUnreadable {
		t.Fatal("a truncated runtime login was not refused")
	}
	if text(t, source) != `"original"` {
		t.Fatal("a truncated login reached the source")
	}
	put(t, source, credential, `{"trunc`)
	if code(h.Prepare()) != CodeLoginUnreadable {
		t.Fatal("a truncated source login was not refused")
	}
}

func TestCredentialIsNeverReadThroughALink(t *testing.T) {
	source, elsewhere := private(t), private(t)
	put(t, elsewhere, "secret", `"elsewhere"`)
	if err := os.Symlink(filepath.Join(elsewhere, "secret"), filepath.Join(source, credential)); err != nil {
		t.Fatal(err)
	}
	if code(home(source, private(t)).Prepare()) != CodeLoginUnreadable {
		t.Fatal("a credential was read through a symbolic link")
	}
}

func TestUnchangedLoginIsNotRewritten(t *testing.T) {
	source, runtime := private(t), private(t)
	h := home(source, runtime)
	put(t, source, credential, `"login"`)
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(filepath.Join(runtime, credential))
	time.Sleep(10 * time.Millisecond)
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(filepath.Join(runtime, credential))
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged login was rewritten")
	}
}

func TestConfigurationCannotNameTheCredential(t *testing.T) {
	source, runtime := private(t), private(t)
	put(t, source, credential, `"login"`)
	h := home(source, runtime)
	h.Files = map[string][]byte{credential: []byte("x")}
	if code(h.Prepare()) != CodeConfigWrite {
		t.Fatal("configuration overwrote the credential")
	}
	h.Files = map[string][]byte{"../escape": []byte("x")}
	if code(h.Prepare()) != CodeConfigWrite {
		t.Fatal("configuration escaped the runtime home")
	}
}
