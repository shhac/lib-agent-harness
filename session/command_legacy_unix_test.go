//go:build darwin || linux

package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/process"
)

func TestWorkbenchLegacyCommandTokenResume(t *testing.T) {
	testenv.RequireProcessStatus(t)
	requireLegacyCommandPlatform(t)
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "sweeps_v022_token", true: "preserves_uncertain_token"}[corrupt], func(t *testing.T) {
			o := workbenchOptions(t, nopHandler())
			o.Workbench.Commands = &Commands{}
			o.complete = (&scriptedModel{}).complete
			s := startAPI(t, o)
			ref := s.Ref()
			closeAPI(t, s)
			dir := sessionDir(o.RuntimeHome, ref.ID)
			path := filepath.Join(dir, "workbench-token.json")
			// Independent v0.22 token schema, never a new-code serialization helper.
			data, err := json.Marshal(struct {
				Token string
				Since time.Time
			}{process.NewToken(), time.Now().Add(-time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			if corrupt {
				data = []byte("invalid-v022-token")
			}
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			resumed, err := Resume(context.Background(), o, ref)
			if corrupt {
				var state *StateError
				if !errors.As(err, &state) || state.Code != StateUnusable {
					if resumed != nil {
						closeAPI(t, resumed)
					}
					t.Fatal(err)
				}
				after, e := os.ReadFile(path)
				if e != nil || string(after) != string(data) {
					t.Fatal("uncertain legacy marker changed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer closeAPI(t, resumed)
			after, e := os.ReadFile(path)
			if e != nil || string(after) == string(data) {
				t.Fatal("legacy marker was not replaced after sweep", e)
			}
		})
	}
}
