//go:build windows

package skills

import (
	"context"
	"testing"

	"github.com/shhac/lib-agent-harness"
)

func TestScriptsAreNotRunOnWindows(t *testing.T) {
	loaded, err := Load([]harness.Skill{{Name: "demo", Dir: writeSkill(t, "---\nname: demo\ndescription: x\n---\n"), Scripts: true}})
	if err != nil {
		t.Fatal(err)
	}
	command, err := Prepare(loaded[0], Manifest, nil, t.TempDir(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), command)
	requireCode(t, err, CodePlatformUnsupported)
}
