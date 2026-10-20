package account

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// The documents below are trimmed from `fx status --json` and
// `fx credits --json` as fx 0.0.14 writes them.
func TestFxStatusReadsTheSignIn(t *testing.T) {
	snapshot, err := parseFxStatus([]byte(`{"kind":"status","model":"openai/gpt-5.4-nano","auth":"fx login","auth_refreshable":true,"team":"viable-cloud","permission_mode":"ask"}`))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LoggedIn == nil || !*snapshot.LoggedIn {
		t.Fatalf("logged in: %+v", snapshot)
	}
	if snapshot.AuthMethod != "fx login" || snapshot.Organization != "viable-cloud" {
		t.Fatalf("account: %+v", snapshot)
	}
	if !snapshot.Observation.Known() {
		t.Fatalf("observation: %+v", snapshot.Observation)
	}
}

func TestFxStatusStatesNoSignInAsSignedOut(t *testing.T) {
	snapshot, err := parseFxStatus([]byte(`{"kind":"status","model":"openai/gpt-5.4-nano","auth":""}`))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LoggedIn == nil || *snapshot.LoggedIn {
		t.Fatalf("logged in: %+v", snapshot)
	}
}

func TestFxStatusRefusesADocumentItCannotRead(t *testing.T) {
	if _, err := parseFxStatus([]byte(`{"kind":"something-else"}`)); err == nil {
		t.Fatal("an unreadable document was accepted")
	}
}

func TestFxCreditsReadsTheBalanceAsItStatedIt(t *testing.T) {
	snapshot, err := parseFxCredits([]byte(`{"kind":"credits","balance":"24.91261535","used":null,"plan":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Balance == nil || snapshot.Balance.Value != "24.91261535" || snapshot.Balance.Unit != "USD" {
		t.Fatalf("balance: %+v", snapshot.Balance)
	}
	if snapshot.Used != nil {
		t.Fatalf("used: %+v", snapshot.Used)
	}
}

func TestFxAccountIsOfferedForTheFxEngine(t *testing.T) {
	if c := harness.Support(harness.Fx, harness.Account, harness.Available); !c.Usable() {
		t.Fatalf("fx account: %+v", c)
	}
	if c := harness.Support(harness.Fx, harness.Account, harness.Quota); c.Usable() {
		t.Fatalf("fx quota: %+v", c)
	}
}

func isFxAccountArgs(args []string) bool {
	return slices.Equal(args, []string{"status", "--json"}) || slices.Equal(args, []string{"credits", "--json"})
}

// fxFixture is fx answering status and credits, as fx 0.0.14 does, in the
// mode written to $HOME/mode. fx runs with the allowlisted operating
// environment, so an ambient FX_ variable or provider key reaching it fails.
func fxFixture(command string) int {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return 10
	}
	if os.Getenv("FX_MODEL") != "" || os.Getenv("AI_GATEWAY_API_KEY") != "" || os.Getenv("LIB_HARNESS_ACCOUNT_FIXTURE") != "" {
		return 14
	}
	if cwd, _ := os.Getwd(); !strings.Contains(cwd, "agent-harness-account-") {
		return 12
	}
	written, _ := os.ReadFile(filepath.Join(home, "mode"))
	mode := string(written)
	switch {
	case mode == "exit" && command == "status":
		return 3
	case mode == "huge" && command == "status":
		fmt.Print(`{"kind":"status","auth":"fx login","pad":"` + strings.Repeat("x", fxCommandLimit) + `"}`)
		return 0
	case mode == "malformed" && command == "status":
		fmt.Println(`{"kind":"other"}`)
		return 0
	case command == "status":
		fmt.Println(`{"kind":"status","model":"gpt-6.1-sol","auth":"Codex subscription","team":"fixture-team","permission_mode":"auto"}`)
		return 0
	case mode == "no_credits":
		fmt.Println(`{"kind":"credits","error":"Credits are unavailable for the selected provider. must-not-escape"}`)
		return 1
	case mode == "malformed_credits":
		fmt.Println(`{"kind":"credits","balance":12}`)
		return 0
	}
	fmt.Println(`{"kind":"credits","balance":"24.91261535","used":"0.08","plan":null}`)
	return 0
}

func fxFixtureProvider(t *testing.T, mode string) harness.Provider {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("FX_MODEL", "must-not-reach-fx")
	t.Setenv("AI_GATEWAY_API_KEY", "synthetic-key-must-not-reach-fx")
	if err := os.WriteFile(filepath.Join(home, "mode"), []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return harness.Provider{Engine: harness.Fx, CLI: harness.CLI{Binary: bin}}
}

func TestFxInspect(t *testing.T) {
	report, err := Inspect(testContext(t), fxFixtureProvider(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Account.Known() || report.Account.LoggedIn == nil || !*report.Account.LoggedIn || report.Account.AuthMethod != "Codex subscription" {
		t.Fatalf("account: %+v", report.Account)
	}
	if !report.Credits.Known() || report.Credits.Balance == nil || report.Credits.Balance.Value != "24.91261535" || report.Credits.Used == nil || report.Credits.Used.Value != "0.08" {
		t.Fatalf("credits: %+v", report.Credits)
	}
	if report.Quota.Known() || report.Quota.Reason == "" {
		t.Fatalf("quota: %+v", report.Quota)
	}
}

// A sign-in with no AI Gateway balance (fx 0.0.14 signed in with a Codex
// subscription answers credits with an error and exit 1), or a balance in
// a shape this library cannot read, leaves credits unknown with a reason,
// never zero, and the sign-in is still reported.
func TestFxCreditsUnavailableStayUnknown(t *testing.T) {
	for _, mode := range []string{"no_credits", "malformed_credits"} {
		t.Run(mode, func(t *testing.T) {
			report, err := Inspect(testContext(t), fxFixtureProvider(t, mode))
			if err != nil || !report.Account.Known() {
				t.Fatalf("%+v %v", report, err)
			}
			if report.Credits.Known() || report.Credits.Balance != nil || report.Credits.Reason == "" {
				t.Fatalf("credits: %+v", report.Credits)
			}
			raw, _ := json.Marshal(report)
			if strings.Contains(string(raw), "must-not-escape") {
				t.Fatal("provider text escaped")
			}
		})
	}
}

func TestFxInspectionFailuresAreTyped(t *testing.T) {
	for mode, code := range map[string]string{"exit": CodeProcessExited, "malformed": CodeInvalidResponse, "huge": CodeInvalidResponse} {
		t.Run(mode, func(t *testing.T) {
			_, err := Inspect(testContext(t), fxFixtureProvider(t, mode))
			facts, ok := harness.ErrorFacts(err)
			if !ok || facts.Code != code || facts.Engine != harness.Fx {
				t.Fatalf("%v %+v", err, facts)
			}
		})
	}
}

// A balance fx states as null is a measured answer with no balance in it,
// not an empty one.
func TestFxCreditsWithANullBalance(t *testing.T) {
	snapshot, err := parseFxCredits([]byte(`{"kind":"credits","balance":null,"used":null}`))
	if err != nil || snapshot.Balance != nil || snapshot.Used != nil || !snapshot.Observation.Known() {
		t.Fatalf("%+v %v", snapshot, err)
	}
}
