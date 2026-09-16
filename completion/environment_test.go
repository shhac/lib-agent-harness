package completion

import (
	"reflect"
	"strings"
	"testing"
)

func TestWindowsOperatingEnvironmentPreservesNativeLocationsOnly(t *testing.T) {
	source := map[string]string{
		"PATH": `C:\tools`, "HOME": `C:\home`, "USER": "owner",
		"USERPROFILE": `C:\Users\owner`, "APPDATA": `C:\Users\owner\AppData\Roaming`, "LOCALAPPDATA": `C:\Users\owner\AppData\Local`,
		"SystemRoot": `C:\Windows`, "COMSPEC": `C:\Windows\System32\cmd.exe`, "PATHEXT": ".COM;.EXE;.BAT;.CMD",
		"OPENAI_API_KEY": "provider-secret", "ANTHROPIC_API_KEY": "provider-secret", "CLAUDE_CODE_OAUTH_TOKEN": "login-secret", "SLACK_BOT_TOKEN": "integration-secret", "NODE_OPTIONS": "--require untrusted.js",
		"CODEX_HOME": "ambient-home", "CLAUDE_CONFIG_DIR": "ambient-home", "TEMP": "ambient-temp",
	}
	env := operatingEnvironment("windows", func(key string) string { return source[key] })
	for _, key := range []string{"PATH", "HOME", "USER", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "COMSPEC", "PATHEXT"} {
		if environmentValue(env, key) != source[key] {
			t.Errorf("missing native context %s", key)
		}
	}
	if len(env) != 9 {
		t.Fatalf("unexpected context: %v", env)
	}
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "SLACK_BOT_TOKEN", "NODE_OPTIONS", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "TEMP"} {
		if environmentValue(env, key) != "" {
			t.Errorf("ambient override inherited: %s", key)
		}
	}
	// A non-Windows process must not inherit unrelated Windows environment names.
	if got := operatingEnvironment("darwin", func(key string) string { return source[key] }); len(got) != 3 {
		t.Fatalf("non-Windows env: %v", got)
	}
}

func TestWindowsDummyEnvironmentRebasesAllAccountAndTempPaths(t *testing.T) {
	input := []string{"PATH=C:\\tools", "SystemRoot=C:\\Windows", "COMSPEC=C:\\Windows\\System32\\cmd.exe", "PATHEXT=.EXE", "home=real-home", "User=real-user", "USERNAME=real-user", "UserProfile=real-profile", "appdata=real-roaming", "LocalAppData=real-local", "HomeDrive=real-drive", "HOMEPATH=real-path", "Codex_Home=real-codex", "CLAUDE_CONFIG_DIR=real-claude", "tmp=real-tmp", "TEMP=real-temp", "TMPDIR=real-tmpdir", "DISABLE_TELEMETRY=1"}
	before := append([]string(nil), input...)
	dir := `C:\private\scratch`
	got := isolatedOperatingEnvironment(input, "windows", dir)
	if !reflect.DeepEqual(input, before) {
		t.Fatal("environment builder mutated input")
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TMP", "TEMP", "TMPDIR"} {
		if environmentValue(got, key) != dir {
			t.Errorf("%s not rebased: %v", key, got)
		}
	}
	for _, entry := range got {
		if strings.Contains(entry, "real-") {
			t.Fatalf("dummy probe retained account context: %s", entry)
		}
	}
	for _, key := range []string{"PATH", "SystemRoot", "COMSPEC", "PATHEXT", "DISABLE_TELEMETRY"} {
		if environmentValue(got, key) == "" {
			t.Errorf("lost OS startup context: %s", key)
		}
	}
}

func TestTemporaryDirectoryOverridesWindowsFallbacks(t *testing.T) {
	got := withTemporaryDirectory([]string{"PATH=tools", "tmp=old", "TEMP=older", "TMPDIR=oldest"}, "windows", `C:\scratch`)
	if len(got) != 4 {
		t.Fatalf("duplicate temp variables: %v", got)
	}
	for _, key := range []string{"TMP", "TEMP", "TMPDIR"} {
		if environmentValue(got, key) != `C:\scratch` {
			t.Errorf("bad %s override", key)
		}
	}
}
