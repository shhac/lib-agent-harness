package completion

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/restrict"
	"github.com/shhac/lib-agent-harness/internal/sharedlogin"
)

// Grok is used only as an authenticated inference transport, like Codex: the
// conversation and the caller's tools are rendered into one prompt, the reply
// is a structured action envelope, and the caller alone executes proposals.
//
// Grok's native tools are removed by launch flags, and those flags are proven
// before the first credentialed launch of each binary and flag set: the same
// launch runs against a disposable home whose only model is a local provider
// that refuses inference, and the request it sends is judged. The operator's
// own Grok home would bring their MCP servers, hooks, plugins and rules, so a
// real run uses a private runtime home this library writes, sharing only the
// login file with the operator's home.

const (
	grokCredentialFile = "auth.json"
	// grokRuntimeHome is the runtime home's fixed place under WorkDirRoot. It
	// is durable so that several runs share one login, and Grok's own locking
	// coordinates their token refreshes.
	grokRuntimeHome = "grok-home"
)

// grokConfig is the runtime home's whole configuration, and the base of the
// probe's. The marketplace keys record the official plugin marketplace as
// already handled, so a fresh home does not install skills or plugins into
// what the next launch would read.
const grokConfig = "# Written by lib-agent-harness for constrained completion.\n" +
	"# Replaced before every launch; nothing here is inherited.\n\n" +
	"[marketplace]\n" +
	"default_skills_installs_purged = true\n" +
	"official_marketplace_auto_installed = true\n"

// grokSkillsConfig hides the skills Grok unpacks into its own home. A logged-in
// home gains them after the probe's disposable one was judged (verified with
// grok 1.0.41: a "skills are available" reminder joined the prompt), so they
// are ignored in both, by path, and the transcript check still backs this up.
func grokSkillsConfig(grokHome string) (string, error) {
	path, err := restrict.TOMLString(filepath.Join(grokHome, "bundled"))
	if err != nil {
		return "", err
	}
	return "\n[skills]\nignore = [" + path + "]\n", nil
}

// grokSwitches narrow Grok beyond the reduced-telemetry list shared by every
// Grok launch: without GROK_WORKFLOWS=0 a reminder listing workflows is added
// to the prompt, and the rest stop managed MCP servers, session indexing and
// the recap, summary and title-refresh requests a completion has no use for.
var grokSwitches = []string{
	"GROK_WORKFLOWS=0",
	"GROK_MANAGED_MCPS_ENABLED=0",
	"GROK_SESSION_SEARCH=0",
	"GROK_SESSION_RECAP=0",
	"GROK_TURN_SUMMARY=0",
	"GROK_TITLE_REFRESH=0",
}

// grokDirs is one launch's private layout. Work is the model's empty working
// directory; the prompt file sits beside it, never inside it.
type grokDirs struct {
	root, home, tmp, work, grokHome string
}

func newGrokDirs(root, grokHome string) (grokDirs, error) {
	d := grokDirs{root: root, home: filepath.Join(root, "home"), tmp: filepath.Join(root, "tmp"), work: filepath.Join(root, "work"), grokHome: grokHome}
	for _, dir := range []string{d.home, d.tmp, d.work} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return grokDirs{}, err
		}
	}
	return d, nil
}

// grokEnvironment is the environment of every Grok completion launch, probe
// and real alike; only the directories differ. Nothing from the parent passes
// except PATH, so neither account discovery nor ambient keys can reach it.
func grokEnvironment(d grokDirs) []string {
	env := nativecli.Without(nativecli.Operating(runtime.GOOS, os.Getenv), "HOME", "USER", "USERPROFILE", "APPDATA", "LOCALAPPDATA")
	env = append(env, "HOME="+d.home, "GROK_HOME="+d.grokHome)
	env = nativecli.WithTemporaryDirectory(env, runtime.GOOS, d.tmp)
	env = append(env, nativecli.GrokReducedTelemetry...)
	return append(env, grokSwitches...)
}

// grokArgs binds every value with --flag=value, since Grok reads a separate
// value beginning with "-" as a missing one. `--tools=` alone would mean every
// tool, and --disallowed-tools alone was observed not to remove them; together
// with --no-subagents they leave none.
func grokArgs(model, effort, schema, promptFile, work string) []string {
	args := []string{"--prompt-file=" + promptFile, "--output-format=streaming-json", "--model=" + model}
	if effort != "" {
		args = append(args, "--reasoning-effort="+effort)
	}
	return append(args,
		"--tools=read_file", "--disallowed-tools=read_file,search_tool,use_tool", "--no-subagents", "--disable-web-search",
		"--system-prompt-override="+codexInstructions, "--verbatim", "--json-schema="+schema, "--cwd="+work)
}

func grokComplete(ctx context.Context, cfg Config, messages []Message, tools []Tool) (Result, error) {
	var empty Result
	if runtime.GOOS == "windows" {
		// The shared login needs owner-only files and no-follow opens this
		// library does not have on Windows; refuse rather than share without them.
		return empty, preflightFailure(harness.Grok, "grok_platform_unsupported")
	}
	if cfg.WorkDirRoot == "" {
		return empty, preflightFailure(harness.Grok, "work_dir_root_required")
	}
	source, err := resolveGrokHome(cfg.Provider.CLI.Home)
	if err != nil {
		return empty, err
	}
	bin, err := resolveGrokBinary(cfg)
	if err != nil {
		return empty, err
	}
	runtimeHome, err := ensureDirectory(cfg.WorkDirRoot, grokRuntimeHome)
	if err != nil {
		return empty, preflightFailure(harness.Grok, "scratch_directory")
	}
	scratchRoot, err := ensureDirectory(cfg.WorkDirRoot, "model-runs")
	if err != nil {
		return empty, preflightFailure(harness.Grok, "scratch_directory")
	}
	scratch, err := os.MkdirTemp(scratchRoot, "agent-harness-grok-")
	if err != nil {
		return empty, preflightFailure(harness.Grok, "scratch_directory")
	}
	defer os.RemoveAll(scratch)
	dirs, err := newGrokDirs(scratch, runtimeHome)
	if err != nil {
		return empty, preflightFailure(harness.Grok, "scratch_directory")
	}
	schema, err := actionSchema(tools)
	if err != nil {
		return empty, preflightFailure(harness.Grok, "invalid_tool_catalog")
	}
	payload, err := json.Marshal(map[string]any{"messages": messages, "available_tools": tools})
	if err != nil || len(payload) > cfg.MaxContextBytes {
		return empty, &RequestError{Cause: harness.CauseContextLimit, Engine: harness.Grok, Phase: PhasePreflight, Code: "context_bytes"}
	}
	promptFile := filepath.Join(scratch, "prompt.txt")
	if err = os.WriteFile(promptFile, payload, 0600); err != nil {
		return empty, preflightFailure(harness.Grok, "scratch_write")
	}
	skills, err := grokSkillsConfig(runtimeHome)
	if err != nil {
		return empty, preflightFailure(harness.Grok, "grok_runtime_home")
	}
	login := grokLogin(source, runtimeHome, skills)
	if err = login.Prepare(); err != nil {
		return empty, grokLoginFailure(err)
	}
	evidence, err := proveGrok(ctx, cfg, bin, string(schema), tools, scratch)
	if err != nil {
		return empty, err
	}
	if err = ctx.Err(); err != nil {
		return empty, err
	}
	if cfg.BeforeRequest != nil {
		if err = cfg.BeforeRequest(ctx); err != nil {
			return empty, err
		}
	}
	stream := newGrokStream(tools)
	args := grokArgs(cfg.Model, cfg.Effort, string(schema), promptFile, dirs.work)
	runErr := runGrokLaunch(ctx, cfg, bin, args, dirs.work, grokEnvironment(dirs), stream.observe)
	// The process has exited, so a login it refreshed is complete on disk.
	// A failed write-back is recovered by the next launch, whose share keeps
	// this runtime copy while the source is unchanged, so it does not cost
	// the caller a result the provider may already have billed.
	_ = login.WriteBack()
	transcript := readGrokTranscript(runtimeHome, dirs.work)
	removeGrokSession(runtimeHome, dirs.work)
	result, failure := stream.result()
	if runErr != nil {
		// Even a well-formed reply is not accepted from a launch that did not
		// exit cleanly.
		result.Message = Message{}
		return result, grokRunFailure(ctx, runErr, stream)
	}
	if failure != nil {
		return result, failure
	}
	// The flags were proven against the probe's request, but a real run is
	// authenticated and may receive remote settings the probe could not. Its
	// own persisted transcript must show the same instruction surface before
	// any proposal is returned.
	if code := evidence.judgeTranscript(transcript, string(payload), dirs.work); code != "" {
		result.Message = Message{}
		return result, &RequestError{Cause: harness.CauseUnknown, Engine: harness.Grok, Phase: PhaseResponse, Code: code}
	}
	return result, nil
}

func grokLogin(source, runtimeHome, skills string) sharedlogin.Home {
	return sharedlogin.Home{
		Source:     source,
		Runtime:    runtimeHome,
		Credential: grokCredentialFile,
		Files:      map[string][]byte{"config.toml": []byte(grokConfig + skills)},
		Valid:      json.Valid,
	}
}

func grokLoginFailure(err error) error {
	var shared *sharedlogin.Error
	if !errors.As(err, &shared) {
		return preflightFailure(harness.Grok, "grok_login_unreadable")
	}
	switch shared.Code {
	case sharedlogin.CodeLoginUnavailable:
		return &RequestError{Cause: harness.CauseAuthentication, Engine: harness.Grok, Phase: PhasePreflight, Code: "grok_login_unavailable"}
	case sharedlogin.CodeRuntimeIsSource:
		return preflightFailure(harness.Grok, "grok_home_is_runtime")
	case sharedlogin.CodeLoginUnreadable:
		return preflightFailure(harness.Grok, "grok_login_unreadable")
	case sharedlogin.CodeUnsupported:
		return preflightFailure(harness.Grok, "grok_platform_unsupported")
	}
	return preflightFailure(harness.Grok, "grok_runtime_home")
}

// resolveGrokHome is the operator's login home: the configured one, else
// GROK_HOME, else ~/.grok. Only its login file is ever read.
func resolveGrokHome(home string) (string, error) {
	if home == "" {
		home = os.Getenv("GROK_HOME")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", preflightFailure(harness.Grok, "grok_home_unresolved")
		}
		home = filepath.Join(userHome, ".grok")
	}
	if !filepath.IsAbs(home) {
		return "", preflightFailure(harness.Grok, "grok_home_invalid")
	}
	return filepath.Clean(home), nil
}

func resolveGrokBinary(cfg Config) (string, error) {
	bin := cfg.Provider.CLI.Binary
	if bin == "" {
		bin = string(harness.Grok)
	}
	found, err := exec.LookPath(bin)
	if err != nil {
		if cfg.run != nil {
			return bin, nil
		}
		if failure := startFailure(harness.Grok, PhasePreflight, err); failure != nil {
			return "", failure
		}
		return "", preflightFailure(harness.Grok, "executable_not_found")
	}
	found, err = filepath.Abs(found)
	if err != nil {
		return "", preflightFailure(harness.Grok, "executable_unresolved")
	}
	return found, nil
}

// grokProbeEffort is the reasoning-effort table the probe model advertises,
// so the probe sees the effort carried as the real model would carry it.
func grokProbeEffort(effort string) (string, error) {
	if effort == "" {
		return "", nil
	}
	value, err := restrict.TOMLString(effort)
	if err != nil {
		return "", err
	}
	return "supports_reasoning_effort = true\nreasoning_efforts = [{ id = " + value + ", value = " + value + ", default = true }]\n", nil
}
