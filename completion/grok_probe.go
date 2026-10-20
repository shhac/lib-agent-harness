package completion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/nativecli"
)

// The probe proves Grok's launch flags before a credentialed launch. It runs
// the very flags a real launch will use against a disposable home that has no
// login and whose only model is a loopback provider that refuses inference,
// then judges the request that reached it: no tools, the system text replaced
// by ours, one fixed vendor context message, our prompt, our schema. The probe
// model is declared the way Grok's own catalog declares its models (Responses
// API, backend search supported, the requested effort advertised) so the
// request is built by the same path a real model's would be.

const (
	grokProbeKey   = "agent-harness-probe"
	grokProbeModel = "agent-harness-probe-model"
	grokProbeLimit = 2 * 1024 * 1024
)

// grokEvidence is what a probe established about the vendor context Grok adds
// before the prompt, which a real run's transcript must match.
type grokEvidence struct {
	os, shell string
	// rules is the whole vendor rules block, byte for byte.
	rules string
}

// grokProofs caches evidence only for the same binary, version and launch
// configuration. Unproven or failed probes are never cached.
var grokProofs = struct {
	sync.Mutex
	evidence map[string]grokEvidence
}{evidence: map[string]grokEvidence{}}

const grokProofCapacity = 64

// proveGrok returns evidence for this launch configuration, probing when none
// is cached.
func proveGrok(ctx context.Context, cfg Config, bin, schema string, tools []Tool, scratch string) (grokEvidence, error) {
	root := filepath.Join(scratch, "probe")
	if err := os.Mkdir(root, 0700); err != nil {
		return grokEvidence{}, preflightFailure(harness.Grok, "scratch_directory")
	}
	grokHome := filepath.Join(root, "grok")
	if err := os.Mkdir(grokHome, 0700); err != nil {
		return grokEvidence{}, preflightFailure(harness.Grok, "scratch_directory")
	}
	dirs, err := newGrokDirs(root, grokHome)
	if err != nil {
		return grokEvidence{}, preflightFailure(harness.Grok, "scratch_directory")
	}
	env := grokEnvironment(dirs)
	key, err := grokProofKey(ctx, cfg, bin, schema, dirs, env)
	if err != nil {
		return grokEvidence{}, err
	}
	grokProofs.Lock()
	evidence, cached := grokProofs.evidence[key]
	grokProofs.Unlock()
	if cached {
		return evidence, nil
	}
	evidence, err = probeGrok(ctx, cfg, bin, schema, tools, dirs, env)
	if err != nil {
		return grokEvidence{}, err
	}
	grokProofs.Lock()
	if len(grokProofs.evidence) >= grokProofCapacity {
		grokProofs.evidence = map[string]grokEvidence{}
	}
	grokProofs.evidence[key] = evidence
	grokProofs.Unlock()
	return evidence, nil
}

// grokProofKey identifies the binary by path, file identity and stated
// version, and the launch by every flag and setting except the per-run prompt
// file and working directory.
func grokProofKey(ctx context.Context, cfg Config, bin, schema string, dirs grokDirs, env []string) (string, error) {
	identity := ""
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		info, err := os.Stat(resolved)
		if err != nil {
			return "", preflightFailure(harness.Grok, "executable_unresolved")
		}
		identity = resolved + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
	} else if cfg.run == nil {
		return "", preflightFailure(harness.Grok, "executable_unresolved")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	version, err := runCLI(probeCtx, cfg, bin, []string{"--version"}, dirs.work, env, "")
	if err != nil {
		if failure := probeRunFailure(harness.Grok, ctx, err); failure != nil {
			return "", failure
		}
		return "", preflightFailure(harness.Grok, "grok_version_unavailable")
	}
	version = bytes.TrimSpace(version)
	if len(version) == 0 || len(version) > 256 {
		return "", preflightFailure(harness.Grok, "grok_version_unavailable")
	}
	hash := sha256.New()
	for _, part := range append([]string{identity, string(version), grokConfig, strings.Join(nativecli.GrokReducedTelemetry, "\n"), strings.Join(grokSwitches, "\n")}, grokArgs(cfg.Model, cfg.Effort, schema, "", "")...) {
		hash.Write([]byte(part))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type grokProbeRequest struct {
	path string
	body []byte
}

func probeGrok(ctx context.Context, cfg Config, bin, schema string, tools []Tool, dirs grokDirs, env []string) (grokEvidence, error) {
	if err := ctx.Err(); err != nil {
		return grokEvidence{}, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return grokEvidence{}, preflightFailure(harness.Grok, "probe_listen_failed")
	}
	var mu sync.Mutex
	var posts []grokProbeRequest
	others, oversized := 0, false
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			// Grok checks the endpoint's reachability with a bodyless GET;
			// it is not a model request and establishes nothing.
			mu.Lock()
			others++
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, readErr := io.ReadAll(io.LimitReader(r.Body, grokProbeLimit+1))
		mu.Lock()
		if readErr != nil || len(data) > grokProbeLimit {
			oversized = true
		} else if len(posts) < 8 {
			posts = append(posts, grokProbeRequest{path: r.URL.Path, body: data})
		} else {
			others++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"local capability check; no inference performed"}}`)
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	stop := sync.OnceFunc(func() { server.Close(); <-done })
	defer stop()

	effort, err := grokProbeEffort(cfg.Effort)
	if err != nil {
		return grokEvidence{}, preflightFailure(harness.Grok, "invalid_effort")
	}
	skills, err := grokSkillsConfig(dirs.grokHome)
	if err != nil {
		return grokEvidence{}, preflightFailure(harness.Grok, "scratch_write")
	}
	config := grokConfig + skills + "\n[model." + grokProbeKey + "]\n" +
		"model = \"" + grokProbeModel + "\"\n" +
		"base_url = \"http://" + listener.Addr().String() + "/v1\"\n" +
		"api_key = \"agent-harness-local-probe\"\n" +
		"api_backend = \"responses\"\n" +
		"supports_backend_search = true\n" +
		"context_window = 100000\n" + effort
	if err = os.WriteFile(filepath.Join(dirs.grokHome, "config.toml"), []byte(config), 0600); err != nil {
		return grokEvidence{}, preflightFailure(harness.Grok, "scratch_write")
	}
	prompt, err := json.Marshal(map[string]any{"messages": []Message{{Role: "user", Content: "Capability check only."}}, "available_tools": tools})
	if err != nil {
		return grokEvidence{}, preflightFailure(harness.Grok, "invalid_tool_catalog")
	}
	promptFile := filepath.Join(dirs.root, "prompt.txt")
	if err = os.WriteFile(promptFile, prompt, 0600); err != nil {
		return grokEvidence{}, preflightFailure(harness.Grok, "scratch_write")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stream := newGrokStream(tools)
	output, runErr := runCLI(probeCtx, cfg, bin, grokArgs(grokProbeKey, cfg.Effort, schema, promptFile, dirs.work), dirs.work, env, "")
	if err = ctx.Err(); err != nil {
		return grokEvidence{}, err
	}
	if failure := probeRunFailure(harness.Grok, ctx, runErr); failure != nil {
		return grokEvidence{}, failure
	}
	for _, line := range bytes.Split(output, []byte("\n")) {
		if stream.observe(line) != "" {
			return grokEvidence{}, preflightFailure(harness.Grok, "probe_unexpected_tools")
		}
	}
	// Nothing reaches the provider once the probe has been judged.
	stop()
	mu.Lock()
	defer mu.Unlock()
	if probeCtx.Err() != nil {
		return grokEvidence{}, preflightFailure(harness.Grok, "probe_timeout")
	}
	if oversized {
		return grokEvidence{}, preflightFailure(harness.Grok, "probe_invalid_request")
	}
	if others > 8 {
		return grokEvidence{}, preflightFailure(harness.Grok, "probe_request_limit")
	}
	if !stream.catalog {
		return grokEvidence{}, preflightFailure(harness.Grok, "probe_missing_tool_catalog")
	}
	want := grokExpectation{model: grokProbeModel, effort: cfg.Effort, schema: []byte(schema), prompt: string(prompt), work: dirs.work}
	evidence, code := judgeGrokProbe(posts, want)
	if code != "" {
		return grokEvidence{}, preflightFailure(harness.Grok, code)
	}
	// The same judgement a real run's transcript will face, made here on the
	// probe's own transcript, proves this binary persists what it sent in the
	// form the real run is checked against.
	if evidence.judgeTranscript(readGrokTranscript(dirs.grokHome, dirs.work), string(prompt), dirs.work) != "" {
		return grokEvidence{}, preflightFailure(harness.Grok, "probe_transcript_unverified")
	}
	return evidence, nil
}
