//go:build !windows

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every credential in these tests is a synthetic string in a temporary
// directory. No native login is read, written or looked for.

func putSynthetic(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func credentialText(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, codexCredentialFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The runtime home's configuration is this library's to replace. Pointing it at
// the operator's own home would destroy exactly the settings this design exists
// to leave alone, so it is refused before anything is written.
func TestRuntimeCannotOverwriteSourceConfiguration(t *testing.T) {
	source := privateDir(t)
	original := "# owner configuration\nmodel=\"owner-model\"\n"
	putSynthetic(t, source, codexCredentialFile, "synthetic-login")
	putSynthetic(t, source, codexConfigFile, original)
	if _, err := prepareRuntimeHome(source, source); err == nil {
		t.Error("source and runtime homes must be rejected before writing")
	}
	raw, err := os.ReadFile(filepath.Join(source, codexConfigFile))
	if err != nil || string(raw) != original {
		t.Error("source configuration changed")
	}
	// A symlinked alias of the same directory is the same directory.
	alias := filepath.Join(t.TempDir(), "alias")
	if err = os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	if _, err = prepareRuntimeHome(source, alias); err == nil {
		t.Error("a symlinked alias of the source home was accepted as a runtime home")
	}
}

// A worker holding an older login must not reinstate it over an account the
// operator has since changed.
func TestStaleWorkerDoesNotOverwriteNewOwnerLogin(t *testing.T) {
	source, runtime := privateDir(t), privateDir(t)
	putSynthetic(t, source, codexCredentialFile, "synthetic-original-login")
	if _, err := prepareRuntimeHome(source, runtime); err != nil {
		t.Fatal(err)
	}
	putSynthetic(t, source, codexCredentialFile, "synthetic-new-owner-login")
	if err := writeBackCredential(source, runtime); err != nil {
		t.Fatal(err)
	}
	if got := credentialText(t, source); got != "synthetic-new-owner-login" {
		t.Fatalf("stale worker overwrote newer owner login: %q", got)
	}
}

// A refresh the harness performed survives a crash that prevented writing it
// back: restarting must not restore the stale copy over it.
func TestCrashAfterNativeRefreshPreservesRefreshedCredential(t *testing.T) {
	source, runtime := privateDir(t), privateDir(t)
	putSynthetic(t, source, codexCredentialFile, "synthetic-original-login")
	if _, err := prepareRuntimeHome(source, runtime); err != nil {
		t.Fatal(err)
	}
	putSynthetic(t, runtime, codexCredentialFile, "synthetic-refreshed-login")
	// The daemon dies before write-back; the source has not changed.
	if _, err := prepareRuntimeHome(source, runtime); err != nil {
		t.Fatal(err)
	}
	if got := credentialText(t, runtime); got != "synthetic-refreshed-login" {
		t.Fatalf("restart discarded a native refresh and restored a stale login: %q", got)
	}
	// And the refresh reaches the source once the harness has stopped.
	if err := writeBackCredential(source, runtime); err != nil {
		t.Fatal(err)
	}
	if got := credentialText(t, source); got != "synthetic-refreshed-login" {
		t.Fatalf("a refresh was never returned to the source: %q", got)
	}
}

// An operator who logs in again wins, and the worker picks that up.
func TestChangedSourceLoginReplacesTheRuntimeCopy(t *testing.T) {
	source, runtime := privateDir(t), privateDir(t)
	putSynthetic(t, source, codexCredentialFile, "synthetic-first-login")
	if _, err := prepareRuntimeHome(source, runtime); err != nil {
		t.Fatal(err)
	}
	putSynthetic(t, source, codexCredentialFile, "synthetic-second-login")
	if _, err := prepareRuntimeHome(source, runtime); err != nil {
		t.Fatal(err)
	}
	if got := credentialText(t, runtime); got != "synthetic-second-login" {
		t.Fatalf("a changed source login did not reach the runtime home: %q", got)
	}
}

// The operator's own configuration is never read, and the runtime home's is
// always this library's.
func TestRuntimeHomeOwnsItsConfigurationAndSharesOnlyTheLogin(t *testing.T) {
	source, runtime := privateDir(t), privateDir(t)
	putSynthetic(t, source, codexCredentialFile, "synthetic-login")
	putSynthetic(t, source, codexConfigFile, "[mcp_servers.owner_tool]\ncommand = \"/bin/sh\"\n")
	putSynthetic(t, source, "history.jsonl", "owner history")
	putSynthetic(t, runtime, codexConfigFile, "[mcp_servers.left_over]\ncommand = \"/bin/sh\"\n")
	if _, err := prepareRuntimeHome(source, runtime); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(runtime, codexConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "mcp_servers") {
		t.Errorf("runtime configuration retained a tool-bearing table: %s", config)
	}
	if _, err = os.Stat(filepath.Join(runtime, "history.jsonl")); !os.IsNotExist(err) {
		t.Error("something other than the login came across from the source home")
	}
	if got := credentialText(t, runtime); got != "synthetic-login" {
		t.Errorf("the login was not shared: %q", got)
	}
	// The shared-login record is private state, and it records a digest rather
	// than anything that could reconstruct the credential.
	record, err := os.ReadFile(filepath.Join(runtime, sharedLoginRecord))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record), "synthetic-login") {
		t.Error("the shared-login record contains the credential itself")
	}
}

// A home with no file-backed login is a named, actionable refusal rather than a
// session that starts and then cannot reach an account.
func TestMissingSourceLoginIsAnActionableRefusal(t *testing.T) {
	_, err := prepareRuntimeHome(privateDir(t), privateDir(t))
	var failure *CapabilityError
	if !errors.As(err, &failure) || failure.Code != CapabilityLoginUnavailable {
		t.Fatalf("a home with no login produced %v", err)
	}
	if !strings.Contains(failure.Error(), "log in to that home") {
		t.Errorf("the refusal does not say what to do: %s", failure)
	}
}

// A credential is never opened through a symbolic link, so a link planted where
// one belongs cannot redirect a read or a write.
func TestCredentialAccessRefusesSymbolicLinks(t *testing.T) {
	source, elsewhere := privateDir(t), privateDir(t)
	putSynthetic(t, elsewhere, "secret", "synthetic-elsewhere")
	if err := os.Symlink(filepath.Join(elsewhere, "secret"), filepath.Join(source, codexCredentialFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := credentialDigest(filepath.Join(source, codexCredentialFile)); err == nil {
		t.Fatal("a credential was read through a symbolic link")
	}
}

// Sharing is idempotent and quick: nothing is rewritten when nothing changed.
func TestRepeatedPreparationLeavesTheCredentialAlone(t *testing.T) {
	source, runtime := privateDir(t), privateDir(t)
	putSynthetic(t, source, codexCredentialFile, "synthetic-login")
	if _, err := prepareRuntimeHome(source, runtime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(runtime, codexCredentialFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("shared credential is not owner-only: %v", info.Mode())
	}
	time.Sleep(10 * time.Millisecond)
	if _, err = prepareRuntimeHome(source, runtime); err != nil {
		t.Fatal(err)
	}
	again, err := os.Stat(filepath.Join(runtime, codexCredentialFile))
	if err != nil {
		t.Fatal(err)
	}
	if !again.ModTime().Equal(info.ModTime()) {
		t.Error("an unchanged credential was rewritten")
	}
	if err = writeBackCredential(source, runtime); err != nil {
		t.Fatal(err)
	}
	if got := credentialText(t, source); got != "synthetic-login" {
		t.Errorf("write-back changed an unchanged source: %q", got)
	}
}

// A restricted session will not start without somewhere private to run.
func TestRestrictedSessionRequiresARuntimeHome(t *testing.T) {
	o := restrictedOptions(t, Codex)
	o.RuntimeHome = ""
	if _, err := normalize(o); err == nil {
		t.Fatal("a restricted session was accepted with no runtime home")
	}
	// And the runtime home is part of its identity: resuming elsewhere is a
	// different session.
	first, err := normalize(restrictedOptions(t, Codex))
	if err != nil {
		t.Fatal(err)
	}
	moved := first
	moved.RuntimeHome = t.TempDir()
	if reference(first, "s1") == reference(moved, "s1") {
		t.Error("moving the runtime home left the session reference unchanged")
	}
}

func TestVerifyRestrictionRejectsAnUnrestrictedConfiguration(t *testing.T) {
	err := VerifyRestriction(context.Background(), Options{Engine: Claude, Binary: "/usr/bin/true"})
	if err == nil {
		t.Fatal("verification accepted a session with no restriction")
	}
}
