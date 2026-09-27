package skills

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Codes for linking skills where a harness discovers them.
const (
	CodeLinkFailed   = "skill_link_failed"
	CodeLinkConflict = "skill_link_conflict"
)

// PluginName is the plugin a library-written plugin directory declares. Claude
// Code lists its skills as "agent-harness-skills:<name>"; Grok lists them by
// name.
const PluginName = "agent-harness-skills"

// pluginManifest is read by both Claude Code 2.1.283 and grok 1.0.41 (which
// also accepts a plugin directory with no manifest).
const pluginManifest = `{"name":"` + PluginName + `","description":"Skills provided for one invocation by lib-agent-harness."}` + "\n"

// maxCopyBytes bounds a skill copied where it cannot be linked.
const maxCopyBytes = 64 << 20

// WritePlugin makes dir, an empty private directory, a plugin whose skills/
// entries are the loaded skills: a symbolic link to each skill's resolved
// directory, or, where the platform refuses links, a bounded copy of its
// regular files. Nothing outside dir is written.
func WritePlugin(dir string, loaded []Skill) error {
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o700); err != nil {
		return fail(CodeLinkFailed)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(pluginManifest), 0o600); err != nil {
		return fail(CodeLinkFailed)
	}
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		return fail(CodeLinkFailed)
	}
	for _, skill := range loaded {
		target := filepath.Join(skillsDir, skill.Name)
		if os.Symlink(skill.root, target) == nil {
			continue
		}
		if err := copyTree(skill.root, target); err != nil {
			return err
		}
	}
	return nil
}

// SyncLinks makes skillsDir, a directory a harness scans for skills in a home
// this library owns, hold exactly a link to each loaded skill beside whatever
// the harness keeps there itself. A symbolic link the set does not name is
// removed, since it can only be one this library left for an earlier launch;
// anything else is the harness's and is left alone, and one that occupies a
// provided skill's name is refused rather than replaced.
func SyncLinks(skillsDir string, loaded []Skill) error {
	if _, err := os.Lstat(skillsDir); len(loaded) == 0 && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		return fail(CodeLinkFailed)
	}
	want := map[string]string{}
	for _, skill := range loaded {
		want[skill.Name] = skill.root
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return fail(CodeLinkFailed)
	}
	for _, entry := range entries {
		path := filepath.Join(skillsDir, entry.Name())
		if entry.Type()&fs.ModeSymlink == 0 {
			if _, provided := want[entry.Name()]; provided {
				return fail(CodeLinkConflict)
			}
			continue
		}
		if target, err := os.Readlink(path); err == nil && want[entry.Name()] == target {
			delete(want, entry.Name())
			continue
		}
		if err := os.Remove(path); err != nil {
			return fail(CodeLinkFailed)
		}
	}
	for _, skill := range loaded {
		if _, pending := want[skill.Name]; !pending {
			continue
		}
		if err := os.Symlink(skill.root, filepath.Join(skillsDir, skill.Name)); err != nil {
			return fail(CodeLinkFailed)
		}
	}
	return nil
}

// copyTree copies root's directories and regular files to target. Links and
// other special files inside a skill are skipped rather than followed, so a
// copy never reaches outside the skill.
func copyTree(root, target string) error {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out := filepath.Join(target, rel)
		switch {
		case entry.IsDir():
			return os.MkdirAll(out, 0o700)
		case !entry.Type().IsRegular():
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if total += info.Size(); total > maxCopyBytes {
			return errors.New("skill too large to copy")
		}
		return copyFile(path, out, info.Mode().Perm())
	})
	if err != nil {
		return fail(CodeLinkFailed)
	}
	return nil
}

func copyFile(from, to string, perm fs.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm&0o700)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
