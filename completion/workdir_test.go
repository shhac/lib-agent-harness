package completion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCodexScratchUsesSelectedStateAndIsRemoved(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var observed string
	cfg := Config{Engine: "codex", Model: "test-model", CodexBin: "sh", CodexHome: t.TempDir(), WorkDirRoot: root}
	cfg.codexRun = func(_ context.Context, _ string, _ []string, dir string, _ []string, _ string) ([]byte, error) {
		observed = dir
		if filepath.Dir(dir) != filepath.Join(root, "model-runs") {
			t.Errorf("scratch=%q", dir)
		}
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("scratch=%v err=%v", info, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0700 {
			t.Errorf("permissions=%v", info.Mode())
		}
		return nil, errors.New("stop synthetic catalog lookup")
	}
	if _, _, err := Complete(context.Background(), cfg, nil, nil); err == nil {
		t.Fatal("expected synthetic failure")
	}
	if observed == "" {
		t.Fatal("probe never reached selected scratch")
	}
	if _, err := os.Stat(observed); !os.IsNotExist(err) {
		t.Fatalf("scratch retained after request: %v", err)
	}
}

func TestCodexRejectsScratchParentSymlinkBeforeSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires Windows privileges")
	}
	root := t.TempDir()
	repository := t.TempDir()
	if err := os.Symlink(repository, filepath.Join(root, "model-runs")); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Engine: "codex", Model: "test-model", CodexBin: "sh", CodexHome: t.TempDir(), WorkDirRoot: root}
	cfg.codexRun = func(context.Context, string, []string, string, []string, string) ([]byte, error) {
		t.Fatal("subprocess ran through unsafe scratch path")
		return nil, nil
	}
	if _, _, err := Complete(context.Background(), cfg, nil, nil); err == nil {
		t.Fatal("accepted scratch parent symlink")
	}
	entries, _ := os.ReadDir(repository)
	if len(entries) != 0 {
		t.Fatal("wrote into linked repository")
	}
}
