//go:build !windows

package sharedlogin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// syncHome shares a login whose account is its "account" field.
func syncHome(source, runtime string) Home {
	h := home(source, runtime)
	h.Account = func(data []byte) string {
		var login struct{ Account string }
		_ = json.Unmarshal(data, &login)
		return login.Account
	}
	return h
}

func prepared(t *testing.T, source string) (Home, string) {
	t.Helper()
	runtime := private(t)
	h := syncHome(source, runtime)
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	return h, runtime
}

// Codex refresh tokens are single use. Two harnesses that each refresh their
// own copy of one login spend the same token twice, and the second refresh is
// refused, observed against codex-cli 0.159.0 as refresh_token_reused. So one
// harness's refresh reaches the source, and every other one, between turns.
func TestSyncCarriesOneRefreshToEveryHarness(t *testing.T) {
	source := private(t)
	put(t, source, credential, `{"account":"a","token":1}`)
	first, firstHome := prepared(t, source)
	second, secondHome := prepared(t, source)

	put(t, firstHome, credential, `{"account":"a","token":2}`)
	if err := first.Sync(); err != nil {
		t.Fatal(err)
	}
	if text(t, source) != `{"account":"a","token":2}` {
		t.Fatalf("the refresh did not reach the source: %s", text(t, source))
	}
	if err := second.Sync(); err != nil {
		t.Fatal(err)
	}
	if text(t, secondHome) != `{"account":"a","token":2}` {
		t.Fatalf("the other harness kept the spent token: %s", text(t, secondHome))
	}
	// Settled: nothing moves.
	if err := first.Sync(); err != nil || text(t, firstHome) != `{"account":"a","token":2}` {
		t.Fatalf("%v %s", err, text(t, firstHome))
	}
}

func TestSyncLeavesWhatItCannotReconcile(t *testing.T) {
	for name, tc := range map[string]struct {
		source, runtime string // written after both homes were prepared; "" leaves it
		removeSource    bool
		want            string
	}{
		// Both refreshed: the source took another's, and this copy holds its
		// own. Neither is thrown away mid-conversation.
		"both refreshed": {source: `{"account":"a","token":2}`, runtime: `{"account":"a","token":3}`, want: `{"account":"a","token":3}`},
		// The operator logged in to another account; a running harness stays
		// on the account its conversation began with.
		"another account": {source: `{"account":"b","token":1}`, want: `{"account":"a","token":1}`},
		// A logout is for the next launch to act on.
		"logged out": {removeSource: true, want: `{"account":"a","token":1}`},
		// A file caught halfway through a rewrite is never copied.
		"torn source": {source: `{"account":"a","tok`, want: `{"account":"a","token":1}`},
	} {
		t.Run(name, func(t *testing.T) {
			source := private(t)
			put(t, source, credential, `{"account":"a","token":1}`)
			h, runtime := prepared(t, source)
			if tc.source != "" {
				put(t, source, credential, tc.source)
			}
			if tc.runtime != "" {
				put(t, runtime, credential, tc.runtime)
			}
			if tc.removeSource {
				if err := removeFile(source); err != nil {
					t.Fatal(err)
				}
			}
			err := h.Sync()
			if name == "torn source" {
				if code(err) != CodeLoginUnreadable {
					t.Fatalf("a torn source was not refused: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if text(t, runtime) != tc.want {
				t.Fatalf("runtime holds %s", text(t, runtime))
			}
			if tc.source != "" && name != "torn source" && text(t, source) != tc.source {
				t.Fatalf("the source was overwritten: %s", text(t, source))
			}
		})
	}
}

func removeFile(dir string) error { return os.Remove(filepath.Join(dir, credential)) }
