package account

import (
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
