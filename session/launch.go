package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shhac/lib-agent-harness/internal/restrict"
)

// prepareLaunch builds a restricted session's runtime and proves it before any
// credentialed process exists. Order is the point: the tool channel and the
// restricted arguments are assembled, checked against the installed harness
// with a disposable login and a provider that refuses to infer, and only then
// handed to the real launch. A caller's login is never present while the
// question "does this build actually drop its own tools" is still open.
//
// lease is the assignment lease when the caller already holds it, or nil for
// the tool host to take; either way the launch owns it from here.
func prepareLaunch(ctx context.Context, o Options, lease *os.File) (*launch, error) {
	if o.Sandbox != nil {
		return prepareSandbox(ctx, o)
	}
	if o.Restriction == nil {
		return nil, nil
	}
	host, err := newToolHost(o.Restriction.Tools, lease)
	if err != nil {
		return nil, err
	}
	l := &launch{host: host}
	fail := func(err error) (*launch, error) { host.close(); return nil, err }
	if o.Engine == Claude {
		l.extra = claudeRestrictedArgs(host)
	} else {
		// The session runs in a home this library owns, with the operator's login
		// shared into it. Their own home keeps its servers, hooks and trust
		// settings, and none of it reaches the worker.
		if _, err = prepareRuntimeHome(o.Home, o.RuntimeHome); err != nil {
			return fail(err)
		}
		catalog, readErr := readCodexCatalog(ctx, o)
		if readErr != nil {
			return fail(readErr)
		}
		if o.Effort == "" {
			o.Effort = restrict.CodexCatalogEffort(catalog, o.Model)
		}
		restricted, restrictErr := restrictedCatalogFor(catalog, o.Model, o.Effort)
		if restrictErr != nil {
			return fail(restrictErr)
		}
		catalogFile := catalogPath(host.cfg.Dir)
		if err = writePrivate(catalogFile, restricted); err != nil {
			return fail(err)
		}
		if l.extra, err = codexRestrictedArgs(host, catalogFile); err != nil {
			return fail(err)
		}
	}
	// The probe is unconditional. What can be skipped is repeating it for a
	// binary and an argument set already proved in this process — which is a
	// record of evidence, not an assertion that evidence was unnecessary.
	key, err := verificationKey(o, l)
	if err != nil {
		return fail(err)
	}
	if verified.holds(key) {
		return l, nil
	}
	host.setProbing(true)
	err = probeRestriction(ctx, o, l)
	host.setProbing(false)
	if err != nil {
		return fail(err)
	}
	verified.record(key)
	return l, nil
}

// launch describes one restricted or sandboxed session's prepared runtime: the
// tool channel it serves, if any, and the arguments it adds.
type launch struct {
	host  *toolHost
	extra []string
}

func commandArgs(o Options, nativeID string, resuming bool, l *launch) []string {
	if o.Engine == Codex {
		args := []string{"app-server", "--listen", "stdio://"}
		if l != nil {
			args = append(args, l.extra...)
		}
		return args
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--permission-mode", o.Policy.ClaudePermission}
	if l != nil {
		args = append(args, l.extra...)
	}
	if resuming {
		args = append(args, "--resume", nativeID)
	} else {
		args = append(args, "--session-id", nativeID)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.Policy.ClaudeTools != nil {
		args = append(args, "--tools="+strings.Join(o.Policy.ClaudeTools, ","))
	}
	if o.Instructions.Mode != "" {
		flag := "--system-prompt"
		if o.Instructions.Mode == Append {
			flag = "--append-system-prompt"
		}
		args = append(args, flag, o.Instructions.Text)
	}
	return args
}

// verificationKey identifies exactly what a probe established: this binary, as
// it is on disk right now, launched with these arguments. Anything else — a
// different binary, an upgraded one, a changed tool surface — is a different
// question and gets asked again.
//
// The channel's ephemeral paths are excluded, because they change per launch
// and are not part of what the probe judged. The tool identifiers are included,
// because they are.
func verificationKey(o Options, l *launch) (string, error) {
	binary, info, err := binaryIdentity(o)
	if err != nil {
		return "", &CapabilityError{Engine: string(o.Engine), Code: CapabilityProbeFailed, Phase: BeforeLaunch}
	}
	stable := make([]string, 0, len(l.extra))
	for _, arg := range l.extra {
		if strings.Contains(arg, l.host.socketDir) || strings.Contains(arg, l.host.cfg.Dir) {
			continue
		}
		stable = append(stable, arg)
	}
	payload, _ := json.Marshal(struct {
		Engine                Engine
		Binary, Model, Effort string
		Size                  int64
		Modified              time.Time
		Args, Tools           []string
		Instructions          Instructions
		Policy                Policy
	}{o.Engine, binary, o.Model, o.Effort, info.Size(), info.ModTime(), stable, o.Restriction.Tools.Qualified(), o.Instructions, o.Policy})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// binaryIdentity is the file the launch will run, as it resolves on disk now:
// a bare name is found on PATH, as the launch finds it, and a link is followed
// to its target, so an upgrade in place or a different executable earlier on
// PATH is a different binary.
func binaryIdentity(o Options) (string, fs.FileInfo, error) {
	binary, err := exec.LookPath(o.Binary)
	if err == nil {
		binary, err = filepath.EvalSymlinks(binary)
	}
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(binary)
	if err != nil {
		return "", nil, err
	}
	return binary, info, nil
}

// verified remembers capability checks for this process only. Nothing is
// written to disk: a restart re-proves, because a restart is exactly when an
// installed CLI is most likely to have changed underneath.
var verified = &verificationCache{seen: map[string]bool{}}

type verificationCache struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (c *verificationCache) holds(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[key]
}
func (c *verificationCache) record(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) > 64 {
		c.seen = map[string]bool{}
	}
	c.seen[key] = true
}

// readCodexCatalog reads the installed model catalog with a disposable home and
// no credentials. It creates no thread and performs no inference.
func readCodexCatalog(ctx context.Context, o Options) ([]byte, error) {
	dir, err := os.MkdirTemp("", "agent-harness-catalog-")
	if err != nil {
		return nil, &CapabilityError{Engine: string(Codex), Code: CapabilityCatalogUnavailable, Phase: BeforeLaunch}
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := runOnce(ctx, o.Binary, []string{"debug", "models", "--bundled"}, dir, disposableEnvironment(o, dir))
	if err != nil {
		return nil, &CapabilityError{Engine: string(Codex), Code: CapabilityCatalogUnavailable, Phase: BeforeLaunch}
	}
	return out, nil
}

// disposableEnvironment strips the operating environment down to what a CLI
// needs to start, with its home pointed at a throwaway directory. No login, no
// provider credential and no account identity is reachable from it.
func disposableEnvironment(o Options, dir string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "HOME=" + dir, "TMPDIR=" + dir}
	if o.Engine == Codex {
		return append(env, "CODEX_HOME="+dir)
	}
	return append(env, "CLAUDE_CONFIG_DIR="+dir)
}
