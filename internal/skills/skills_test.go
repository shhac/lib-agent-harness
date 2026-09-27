package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shhac/lib-agent-harness"
)

// writeSkill makes a synthetic skill directory holding SKILL.md with the
// given text, and returns its path.
func writeSkill(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Manifest), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if Code(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	if err != nil && strings.Contains(err.Error(), "secret") {
		t.Fatalf("error carries file contents: %v", err)
	}
}

func TestLoadReadsTheFrontMatterSubset(t *testing.T) {
	for name, tc := range map[string]struct{ manifest, description string }{
		"plain":         {"---\nname: demo\ndescription: Does a thing.\n---\n# Demo\n", "Does a thing."},
		"crlf and bom":  {"\xef\xbb\xbf---\r\nname: demo\r\ndescription: Does a thing.\r\n---\r\n", "Does a thing."},
		"double quoted": {"---\nname: \"demo\"\ndescription: \"Use when: a colon \\\"matters\\\".\"\n---\n", `Use when: a colon "matters".`},
		"single quoted": {"---\nname: 'demo'\ndescription: 'It''s quoted: fine'\n---\n", "It's quoted: fine"},
		"folded block":  {"---\nname: demo\ndescription: >-\n  First line,\n  second line: with colon.\n\n  After a blank.\n---\n", "First line, second line: with colon. After a blank."},
		"literal block": {"---\nname: demo\ndescription: |\n  One\n  Two\n---\n", "One Two"},
		"continued":     {"---\nname: demo\ndescription: Starts here\n  and continues\n---\n", "Starts here and continues"},
		"comment":       {"---\n# a comment\nname: demo # trailing\ndescription: Kept. # dropped\n---\n", "Kept."},
		"other keys":    {"---\nname: demo\nlicense: MIT\nallowed-tools:\n  - Read\n  - Grep\nmetadata:\n  owner: team\n  nested: {a: 1}\ndescription: Does a thing.\n---\n", "Does a thing."},
	} {
		t.Run(name, func(t *testing.T) {
			loaded, err := Load([]harness.Skill{{Name: "demo", Dir: writeSkill(t, tc.manifest), Scripts: true}})
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != 1 || loaded[0].Name != "demo" || loaded[0].Description != tc.description || !loaded[0].Scripts {
				t.Fatalf("%+v", loaded)
			}
		})
	}
}

func TestLoadRefusals(t *testing.T) {
	valid := "---\nname: demo\ndescription: Does a thing.\n---\n"
	for name, tc := range map[string]struct {
		manifest string
		code     string
	}{
		"no front matter":        {"# demo\n", CodeFrontMatterInvalid},
		"unclosed":               {"---\nname: demo\ndescription: x\n", CodeFrontMatterInvalid},
		"duplicate key":          {"---\nname: demo\nname: demo\ndescription: x\n---\n", CodeFrontMatterInvalid},
		"flow value":             {"---\nname: demo\ndescription: [a, b]\n---\n", CodeFrontMatterInvalid},
		"anchor":                 {"---\nname: &n demo\ndescription: x\n---\n", CodeFrontMatterInvalid},
		"unquoted colon":         {"---\nname: demo\ndescription: Use when: this\n---\n", CodeFrontMatterInvalid},
		"bad double quote":       {"---\nname: demo\ndescription: \"open\n---\n", CodeFrontMatterInvalid},
		"bad single quote":       {"---\nname: demo\ndescription: 'it's'\n---\n", CodeFrontMatterInvalid},
		"name block":             {"---\nname: |\n  demo\ndescription: x\n---\n", CodeFrontMatterInvalid},
		"name continued":         {"---\nname: demo\n  more\ndescription: x\n---\n", CodeFrontMatterInvalid},
		"no name":                {"---\ndescription: x\n---\n", CodeFrontMatterInvalid},
		"malformed name":         {"---\nname: Demo\ndescription: x\n---\n", CodeFrontMatterInvalid},
		"key without colon":      {"---\nname: demo\ndescription: x\nstray\n---\n", CodeFrontMatterInvalid},
		"name mismatch":          {"---\nname: other\ndescription: x\n---\n", CodeNameMismatch},
		"no description":         {"---\nname: demo\n---\n", CodeDescriptionInvalid},
		"empty description":      {"---\nname: demo\ndescription: \"\"\n---\n", CodeDescriptionInvalid},
		"long description":       {"---\nname: demo\ndescription: " + strings.Repeat("x", MaxDescription+1) + "\n---\n", CodeDescriptionInvalid},
		"control in description": {"---\nname: demo\ndescription: \"a\\u001bb\"\n---\n", CodeDescriptionInvalid},
		"not text":               {valid + "secret\xff\n", CodeManifestInvalid},
		"nul":                    {valid + "secret\x00\n", CodeManifestInvalid},
		"too large":              {valid + strings.Repeat("secret ", MaxFileBytes/7+1), CodeManifestTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load([]harness.Skill{{Name: "demo", Dir: writeSkill(t, tc.manifest)}})
			requireCode(t, err, tc.code)
		})
	}

	good := writeSkill(t, valid)
	notDir := filepath.Join(good, Manifest)
	manifestDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(manifestDir, Manifest), 0700); err != nil {
		t.Fatal(err)
	}
	many := make([]harness.Skill, MaxSkills+1)
	for name, tc := range map[string]struct {
		skills []harness.Skill
		code   string
	}{
		"relative dir": {[]harness.Skill{{Name: "demo", Dir: "skills/demo"}}, CodeDirInvalid},
		"empty dir":    {[]harness.Skill{{Name: "demo"}}, CodeDirInvalid},
		"missing dir":  {[]harness.Skill{{Name: "demo", Dir: filepath.Join(good, "missing")}}, CodeDirUnavailable},
		"file as dir":  {[]harness.Skill{{Name: "demo", Dir: notDir}}, CodeDirUnavailable},
		"no manifest":  {[]harness.Skill{{Name: "demo", Dir: t.TempDir()}}, CodeManifestMissing},
		"manifest dir": {[]harness.Skill{{Name: "demo", Dir: manifestDir}}, CodeManifestInvalid},
		"invalid name": {[]harness.Skill{{Name: "Demo_1", Dir: good}}, CodeNameInvalid},
		"long name":    {[]harness.Skill{{Name: strings.Repeat("a", MaxNameLength+1), Dir: good}}, CodeNameInvalid},
		"duplicate":    {[]harness.Skill{{Name: "demo", Dir: good}, {Name: "demo", Dir: good}}, CodeDuplicate},
		"too many":     {many, CodeTooMany},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(tc.skills)
			requireCode(t, err, tc.code)
		})
	}
}

func TestReadStaysInsideTheSkill(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := writeSkill(t, "---\nname: demo\ndescription: x\n---\n")
	for rel, content := range map[string]string{
		"docs/guide.md": "guide",
		"binary.bin":    "secret\xff\xfe",
		"nul.txt":       "secret\x00",
		"large.txt":     strings.Repeat("s", MaxFileBytes+1),
		"limit.txt":     strings.Repeat("s", MaxFileBytes),
	} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := Load([]harness.Skill{{Name: "demo", Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	skill := loaded[0]
	for _, rel := range []string{"docs/guide.md", "./docs/../docs/guide.md", "docs//guide.md"} {
		if text, err := skill.Read(rel); err != nil || text != "guide" {
			t.Fatalf("%s: %q %v", rel, text, err)
		}
	}
	if text, err := skill.Read("limit.txt"); err != nil || len(text) != MaxFileBytes {
		t.Fatalf("a file at the limit: %d %v", len(text), err)
	}
	relOutside, _ := filepath.Rel(dir, filepath.Join(outside, "secret.txt"))
	for rel, code := range map[string]string{
		"":                              CodePathInvalid,
		".":                             CodePathInvalid,
		"/etc/passwd":                   CodePathInvalid,
		"docs\\guide.md":                CodePathInvalid,
		"C:guide.md":                    CodePathInvalid,
		"docs/\x00":                     CodePathInvalid,
		"docs/\nx":                      CodePathInvalid,
		"..":                            CodeOutsideSkill,
		"../secret.txt":                 CodeOutsideSkill,
		"docs/../../secret.txt":         CodeOutsideSkill,
		filepath.ToSlash(relOutside):    CodeOutsideSkill,
		"missing.md":                    CodeFileNotFound,
		"docs":                          CodeFileNotRegular,
		"large.txt":                     CodeFileTooLarge,
		"binary.bin":                    CodeFileNotText,
		"nul.txt":                       CodeFileNotText,
		strings.Repeat("a/", 600) + "x": CodePathInvalid,
	} {
		_, err := skill.Read(rel)
		requireCode(t, err, code)
	}
}
