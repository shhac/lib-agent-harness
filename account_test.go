package harness

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAccountReportRoundTrips(t *testing.T) {
	yes, no, used, minutes, resets := true, false, 11.0, int64(300), 2
	when := time.Date(2026, 9, 27, 19, 59, 59, 0, time.UTC)
	observed := Observation{Quality: Measured, ObservedAt: when, Source: "fixture"}
	report := AccountReport{
		Engine:  Claude,
		Account: AccountSnapshot{Observation: observed, LoggedIn: &yes, Email: "fixture@example.test", Plan: "max"},
		Quota: QuotaSnapshot{Observation: observed, Complete: true, LimitReached: &no, Windows: []QuotaWindow{
			{Observation: observed, ID: "five_hour", Kind: QuotaSession, UsedPercent: &used, WindowMinutes: &minutes, ResetsAt: &when},
			{Observation: observed, ID: "limits/weekly_model/Fable", Kind: QuotaWeeklyModel, Model: "Fable", Allowance: &Allowance{Unit: "USD", Limit: 250, Used: &used}},
		}},
		Credits: CreditSnapshot{Observation: observed, Enabled: &no, Unlimited: &no, LimitReached: &no, Balance: &Amount{"0", CreditUnit}, Used: &Amount{"2.51", "GBP"}, Limit: &Amount{"37.50", "GBP"}, DisabledReason: "out_of_credits", ResetsAvailable: &resets},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var back AccountReport
	if err = json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, report) {
		t.Fatalf("round trip changed the report:\n%s\n%+v", raw, back)
	}
	for _, field := range []string{`"kind":"session"`, `"kind":"weekly_model"`, `"model":"Fable"`, `"limit_reached":false`, `"used":{"value":"2.51","unit":"GBP"}`, `"resets_available":2`, `"credits":{`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("missing %s in %s", field, raw)
		}
	}
}

func TestUnknownPartsStayUnknown(t *testing.T) {
	report := AccountReport{Engine: Grok, Quota: QuotaSnapshot{Observation: Observation{Reason: "not exposed"}}, Credits: CreditSnapshot{Observation: Observation{Reason: "not exposed"}}}
	raw, _ := json.Marshal(report)
	var back AccountReport
	if json.Unmarshal(raw, &back) != nil || back.Quota.Known() || back.Credits.Known() || back.Account.Known() || back.Credits.Reason != "not exposed" {
		t.Fatalf("unknown became known: %s", raw)
	}
	for _, zero := range []string{`"balance"`, `"enabled"`, `"limit_reached"`, `"logged_in"`} {
		if strings.Contains(string(raw), zero) {
			t.Fatalf("unknown %s serialized as a value: %s", zero, raw)
		}
	}
	if (QuotaWindow{}).RemainingPercent() != nil {
		t.Fatal("unknown quota looks free")
	}
	over := 130.0
	if *(QuotaWindow{UsedPercent: &over}).RemainingPercent() != 0 {
		t.Fatal("remainder not clamped")
	}
}

func TestObservationFreshness(t *testing.T) {
	now := time.Now()
	o := Observation{Quality: Estimated, ObservedAt: now}
	if o.IsStale(now, time.Second) || !o.IsStale(now.Add(2*time.Second), time.Second) || o.IsStale(now.Add(time.Hour), 0) {
		t.Fatal("age check")
	}
	o.Invalidated = true
	if !o.IsStale(now, 0) || !(Observation{}).IsStale(now, 0) {
		t.Fatal("invalidated or unknown observation looked fresh")
	}
}
