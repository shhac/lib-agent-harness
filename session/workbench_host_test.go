package session

import (
	"context"
	"testing"

	"github.com/shhac/lib-agent-harness/sandbox"
)

func TestWorkbenchWorkspaceConfiguration(t *testing.T) {
	previous := openWorkspaceFiles
	defer func() { openWorkspaceFiles = previous }()
	var configs []sandbox.Config
	openWorkspaceFiles = func(config sandbox.Config) (*sandbox.Workspace, error) {
		configs = append(configs, config)
		return sandbox.OpenWorkspace(config)
	}
	o := workbenchOptions(t, nopHandler())
	o.complete = (&scriptedModel{}).complete
	o.Workbench.NewFileMode = 0644
	o.Restriction.Tools.MaxResultBytes = 4096
	s := startAPI(t, o)
	ref := s.Ref()
	closeAPI(t, s)
	resumed, err := Resume(context.Background(), o, ref)
	if err != nil {
		t.Fatal(err)
	}
	closeAPI(t, resumed)
	if len(configs) != 2 {
		t.Fatalf("opened %d workspaces", len(configs))
	}
	for _, config := range configs {
		if config.SessionID != ref.ID || config.OnFailure == nil || config.NewFileMode != 0644 || config.Budget != 4096 {
			t.Fatalf("wrong workspace configuration: %+v", config)
		}
	}
}
