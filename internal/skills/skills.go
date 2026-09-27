// Package skills loads caller-provided skill directories and reads, or runs,
// files inside them without ever leaving the directory.
//
// A skill is a directory holding SKILL.md, whose YAML front matter names the
// skill and describes it, plus the files that SKILL.md references. Everything
// here is bounded and contained: paths are relative, cleaned, and must still
// lie inside the skill's directory after every symbolic link is resolved;
// files are regular, bounded in size and, when read, UTF-8 text. Failures carry
// a fixed code and never a file's contents or a path.
package skills

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/shhac/lib-agent-harness"
)

// Error carries a fixed reason code.
type Error struct{ Code string }

func (e *Error) Error() string { return "skill: " + e.Code }

func fail(code string) error { return &Error{Code: code} }

// Code returns the fixed code an error from this package carries, or "".
func Code(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

// Codes for loading a skill.
const (
	CodeTooMany            = "too_many_skills"
	CodeNameInvalid        = "skill_name_invalid"
	CodeDuplicate          = "skill_duplicate"
	CodeDirInvalid         = "skill_dir_invalid"
	CodeDirUnavailable     = "skill_dir_unavailable"
	CodeManifestMissing    = "skill_manifest_missing"
	CodeManifestInvalid    = "skill_manifest_invalid"
	CodeManifestTooLarge   = "skill_manifest_too_large"
	CodeFrontMatterInvalid = "skill_front_matter_invalid"
	CodeNameMismatch       = "skill_name_mismatch"
	CodeDescriptionInvalid = "skill_description_invalid"
)

const (
	// Manifest is the file every skill directory holds.
	Manifest = "SKILL.md"
	// MaxSkills bounds one invocation's set, and so the index a model sees.
	MaxSkills         = 64
	MaxNameLength     = 64
	MaxDescription    = 1024
	MaxFileBytes      = 256 << 10
	maxRelativeLength = 1024
)

// Skill is a loaded skill: its name and description from SKILL.md, and its
// directory with every symbolic link resolved.
type Skill struct {
	Name        string
	Description string
	// Scripts is the caller's permission for this skill's scripts to be run.
	Scripts bool
	root    string
}

// Root is the skill's resolved directory.
func (s Skill) Root() string { return s.root }

// ValidName reports whether name is a well-formed skill name: lowercase ASCII
// letters, digits and hyphens, at most MaxNameLength characters.
func ValidName(name string) bool {
	if name == "" || len(name) > MaxNameLength {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// Load loads every provided skill, refusing the whole set when any one is
// unusable or two share a name.
func Load(provided []harness.Skill) ([]Skill, error) {
	if len(provided) > MaxSkills {
		return nil, fail(CodeTooMany)
	}
	seen := map[string]bool{}
	loaded := make([]Skill, 0, len(provided))
	for _, skill := range provided {
		if !ValidName(skill.Name) {
			return nil, fail(CodeNameInvalid)
		}
		if seen[skill.Name] {
			return nil, fail(CodeDuplicate)
		}
		seen[skill.Name] = true
		s, err := load(skill)
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, s)
	}
	return loaded, nil
}

func load(skill harness.Skill) (Skill, error) {
	if skill.Dir == "" || strings.ContainsRune(skill.Dir, 0) || !filepath.IsAbs(skill.Dir) {
		return Skill{}, fail(CodeDirInvalid)
	}
	root, err := filepath.EvalSymlinks(filepath.Clean(skill.Dir))
	if err != nil {
		return Skill{}, fail(CodeDirUnavailable)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return Skill{}, fail(CodeDirUnavailable)
	}
	s := Skill{Name: skill.Name, Scripts: skill.Scripts, root: root}
	text, err := s.Read(Manifest)
	if err != nil {
		switch Code(err) {
		case CodeFileNotFound:
			return Skill{}, fail(CodeManifestMissing)
		case CodeFileTooLarge:
			return Skill{}, fail(CodeManifestTooLarge)
		}
		return Skill{}, fail(CodeManifestInvalid)
	}
	fields, ok := frontMatter(text)
	if !ok {
		return Skill{}, fail(CodeFrontMatterInvalid)
	}
	name, ok := fields["name"]
	if !ok || !ValidName(name) {
		return Skill{}, fail(CodeFrontMatterInvalid)
	}
	if name != skill.Name {
		return Skill{}, fail(CodeNameMismatch)
	}
	description := strings.Join(strings.Fields(fields["description"]), " ")
	if description == "" || utf8.RuneCountInString(description) > MaxDescription || strings.IndexFunc(description, control) >= 0 {
		return Skill{}, fail(CodeDescriptionInvalid)
	}
	s.Description = description
	return s, nil
}

func control(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }
