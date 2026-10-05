package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/internal/skills"
)

// Options selects a proved OS command boundary for standalone or hosted work.
// RuntimeHome must be an existing private directory outside WorkDir.
type Options struct {
	WorkDir, RuntimeHome string
	Write                bool
	Read, Env            []string
	// Loopback permits all-interface binds on macOS; outbound stays on-machine.
	Loopback bool
	// LoopbackLocalOnly requires Loopback and proves private loopback networking.
	LoopbackLocalOnly bool
	// LoopbackPorts requests 1–32 selected ports (1–65535); nil leaves Loopback unchanged.
	// Selected-port confinement is refused before discovery or launch on every platform.
	LoopbackPorts []int
	// LoopbackControl is an off-machine IP literal for a selected-port proof.
	// It requires LoopbackPorts; no proof is currently offered.
	LoopbackControl string

	Timeout    time.Duration
	Background bool
	// ProcessInspection requires proof of inspection within each command's own tree.
	// Linux uses a private PID namespace. Other platforms refuse the request.
	ProcessInspection bool
	system            []string
}

func platformRefusal() error {
	return &RefusalError{Operation: "sandbox", Code: RefusedNotOffered, Capability: harness.Support(harness.OpenAICompatible, harness.Session, harness.Sandbox)}
}

func normalize(o Options, standalone bool) (Options, error) {
	if o.ProcessInspection && runtime.GOOS != "linux" {
		return o, refusal("process_inspection", RefusedProcessInspectionUnenforceable, harness.Support(harness.OpenAICompatible, harness.Session, harness.ProcessInspection).Reason)
	}
	var networkErr error
	if o, networkErr = normalizeNetwork(o); networkErr != nil {
		return o, networkErr
	}
	if o.LoopbackLocalOnly {
		if !o.Loopback {
			return o, refusal("loopback", RefusedConflict, "LoopbackLocalOnly requires Loopback")
		}
		if runtime.GOOS == "darwin" {
			return o, refusal("loopback", RefusedLoopbackNotLocal, harness.LoopbackLocalOnlySeatbeltReason)
		}
		if !standalone {
			return o, refusal("loopback", RefusedNotOffered, harness.Support(harness.OpenAICompatible, harness.Session, harness.LoopbackLocalOnly).Reason)
		}
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return o, platformRefusal()
	}
	var err error
	if o, err = normalizeRuntimeHome(o); err != nil {
		return o, err
	}
	if o.WorkDir == "" {
		return o, refusal("work_dir", RefusedWorkDir, "a workbench requires WorkDir, the workspace its tools are confined to")
	}
	if !filepath.IsAbs(o.WorkDir) {
		return o, refusal("work_dir", RefusedWorkDir, "WorkDir must be an absolute path")
	}
	work, err := filepath.EvalSymlinks(o.WorkDir)
	if err != nil {
		return o, refusal("work_dir", RefusedWorkDir, "WorkDir must be an existing directory")
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		return o, refusal("work_dir", RefusedWorkDir, "WorkDir must be an existing directory")
	}
	home, err := filepath.EvalSymlinks(o.RuntimeHome)
	if err != nil {
		return o, refusal("runtime_home", RefusedRuntimeHome, "the runtime home could not be resolved")
	}
	if Nested(work, home) || Nested(home, work) {
		return o, refusal("work_dir", RefusedWorkDir, "the workspace and the runtime home must not contain one another")
	}
	o.WorkDir = work
	return normalizeWorkbenchCommands(o, standalone)
}
func normalizeRuntimeHome(o Options) (Options, error) {
	if o.RuntimeHome == "" {
		return o, refusal("runtime_home", RefusedRuntimeHome, "an API session keeps its conversation in a durable private runtime home; set Options.RuntimeHome")
	}
	home, err := filepath.Abs(o.RuntimeHome)
	if err != nil {
		return o, refusal("runtime_home", RefusedRuntimeHome, "invalid runtime home")
	}
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() {
		return o, refusal("runtime_home", RefusedRuntimeHome, "the runtime home must be an existing directory")
	}
	if !privateStateDir(info) {
		return o, refusal("runtime_home", RefusedRuntimeHome, "the runtime home must be readable only by its owner")
	}
	o.RuntimeHome = home
	return o, nil
}

func normalizeWorkbenchCommands(o Options, standalone bool) (Options, error) {
	c := o
	c.Read = slices.Clone(c.Read)
	c.Env = slices.Clone(c.Env)
	if c.Timeout == 0 {
		c.Timeout = 2 * time.Minute
	}
	if c.Timeout < 0 || c.Timeout > skills.MaxTimeout {
		return o, refusal("commands", RefusedLimit, "command timeout must be between zero and ten minutes")
	}
	home, _ := os.UserHomeDir()
	if resolved, e := filepath.EvalSymlinks(home); e == nil {
		home = resolved
	}
	if home == "" || nested(o.WorkDir, home) {
		return o, refusal("work_dir", RefusedWorkDir, "a command workspace must not contain the owner's home")
	}
	read, problem := readDirs(c.Read)
	if problem != "" {
		return o, refusal("commands", RefusedSandboxRead, problem)
	}
	c.Read = nil
	runtimeHome, e := filepath.EvalSymlinks(o.RuntimeHome)
	if e != nil {
		return o, refusal("runtime_home", RefusedRuntimeHome, "runtime home could not be resolved")
	}
	o.RuntimeHome = runtimeHome
	var err error
	o, err = normalizeWorkbenchSystem(o, standalone)
	if err != nil {
		return o, err
	}
	for _, dir := range read {
		if runtime.GOOS == "linux" && linuxCommandSocketRead(dir) {
			return o, refusal("commands", RefusedSandboxRead, "command read paths must not overlap /run or contain /tmp: read-only binds expose host Unix sockets")
		}
		if info, e := os.Stat(dir); e != nil || !info.IsDir() {
			return o, refusal("commands", RefusedSandboxRead, "command read paths must be existing directories")
		}
		for _, alias := range workbenchDataAliases(home) {
			if nested(dir, alias) {
				return o, refusal("commands", RefusedSandboxRead, "command read paths must not reopen the owner's home through a data-volume alias")
			}
		}
		for _, alias := range workbenchDataAliases(runtimeHome) {
			if nested(dir, alias) || nested(alias, dir) {
				return o, refusal("runtime_home", RefusedRuntimeHome, "command read paths must not overlap the runtime home")
			}
		}
		covered := false
		for _, system := range o.system {
			if workbenchSystemContains(system, dir) {
				covered = true
				break
			}
		}
		if !covered {
			c.Read = append(c.Read, dir)
		}
	}
	slices.Sort(c.Read)
	c.Read = slices.Compact(c.Read)
	for _, entry := range c.Env {
		if why := workbenchEnvRefusal(entry); why != "" {
			return o, refusal("commands", RefusedConflict, "Commands.Env "+why)
		}
	}
	c.RuntimeHome = o.RuntimeHome
	c.system = o.system
	o = c
	return o, nil
}

// Paths have already been resolved, including /var/run aliases.
func linuxCommandSocketRead(dir string) bool {
	return lexicallyWithin(dir, "/run") || lexicallyWithin("/run", dir) || lexicallyWithin(dir, "/tmp")
}

func workbenchDataAliases(p string) []string {
	aliases := []string{p}
	const data = "/System/Volumes/Data"
	other := filepath.Join(data, p)
	if lexicallyWithin(data, p) {
		other = strings.TrimPrefix(p, data)
	}
	a, err := os.Stat(p)
	if err != nil {
		return aliases
	}
	b, err := os.Stat(other)
	if err == nil && os.SameFile(a, b) && other != p {
		aliases = append(aliases, other)
	}
	return aliases
}

// workbenchEnvRefusal says why a Commands.Env entry is refused, or "" when it
// is an ordinary setting the caller may pass to commands. What it refuses
// either belongs to the library, acts before the sandbox exists, runs ahead
// of the supervisor script, or would hand a credential to the model, which
// can print its environment.
func workbenchEnvRefusal(entry string) string {
	key, _, ok := strings.Cut(entry, "=")
	switch {
	case !ok || !workbenchEnvName(key) || strings.ContainsRune(entry, 0):
		return "entries must be NAME=value with a portable name"
	case key == "HOME" || key == "TMPDIR":
		return "cannot set HOME or TMPDIR: they name private scratch"
	case strings.HasPrefix(key, "AGENT_HARNESS_"):
		return "cannot set AGENT_HARNESS_* variables: they belong to the library"
	case strings.HasPrefix(key, "DYLD_") || strings.HasPrefix(key, "LD_"):
		return "cannot set dynamic loader variables: they act before the sandbox applies"
	case slices.Contains([]string{"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "IFS", "CDPATH"}, key):
		return "cannot set shell start-up variables: they run ahead of the command supervisor"
	case workbenchEnvCredential(key):
		return "cannot set credential-like variables: commands' output reaches the model"
	}
	return ""
}

// workbenchEnvName is a portable environment variable name, which also keeps
// out Bash's exported functions (BASH_FUNC_name%%).
func workbenchEnvName(key string) bool {
	for i, r := range key {
		letter := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return key != ""
}

// workbenchEnvCredential reports a name with a credential-like segment, such
// as OPENAI_API_KEY, GITHUB_TOKEN or SSH_AUTH_SOCK.
func workbenchEnvCredential(key string) bool {
	for _, segment := range strings.Split(strings.ToUpper(key), "_") {
		switch segment {
		case "KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "CREDENTIALS", "AUTH":
			return true
		}
	}
	return false
}

func readDirs(dirs []string) ([]string, string) {
	return sandboxbridge.ReadDirs(dirs)
}

func nested(a, b string) bool          { return Nested(a, b) }
func lexicallyWithin(a, b string) bool { return Within(a, b) }
