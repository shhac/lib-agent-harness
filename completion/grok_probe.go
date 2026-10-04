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
	"github.com/shhac/lib-agent-harness/internal/rawjson"
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

type grokExpectation struct {
	model, effort, prompt, work string
	schema                      []byte
}

// judgeGrokProbe judges every request the probe provider received: exactly one
// model request, which must pass judgeGrokRequest, beside at most two
// session-title requests Grok makes on its own account. A real run does not
// depend on those; they are only tolerated here.
func judgeGrokProbe(posts []grokProbeRequest, want grokExpectation) (grokEvidence, string) {
	var evidence grokEvidence
	models, titles := 0, 0
	for _, post := range posts {
		if grokTitleRequest(post.body) {
			titles++
			continue
		}
		models++
		if models > 1 {
			continue
		}
		if post.path != "/v1/responses" {
			return grokEvidence{}, "probe_mismatch"
		}
		var code string
		evidence, code = judgeGrokRequest(post.body, want)
		if code != "" {
			return grokEvidence{}, code
		}
	}
	switch {
	case models == 0:
		return grokEvidence{}, "probe_no_requests"
	case models > 1 || titles > 2:
		return grokEvidence{}, "probe_request_limit"
	}
	return evidence, ""
}

// grokTitleRequest recognizes Grok's session-title request: one forced
// function, session_title, and no structured output.
func grokTitleRequest(body []byte) bool {
	var request struct {
		Tools []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tools"`
		Choice struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
		Text json.RawMessage `json:"text"`
	}
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	return len(request.Tools) == 1 && request.Tools[0].Type == "function" && request.Tools[0].Name == "session_title" &&
		request.Choice.Type == "function" && request.Choice.Name == "session_title" && rawjson.Absent(request.Text)
}

// grokRequestKeys are the Responses API fields a proven request may carry.
// Anything else, above all tools, tool_choice and instructions, is refused.
var grokRequestKeys = map[string]bool{
	"include": true, "input": true, "model": true, "prompt_cache_key": true, "reasoning": true,
	"store": true, "stream": true, "text": true, "temperature": true, "top_p": true, "max_output_tokens": true,
}

func judgeGrokRequest(body []byte, want grokExpectation) (grokEvidence, string) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return grokEvidence{}, "probe_invalid_request"
	}
	for key, value := range fields {
		switch {
		case key == "tools":
			var tools []json.RawMessage
			if json.Unmarshal(value, &tools) != nil || len(tools) != 0 {
				return grokEvidence{}, "probe_unexpected_tools"
			}
		case key == "tool_choice" || key == "parallel_tool_calls":
			return grokEvidence{}, "probe_unexpected_tools"
		case key == "instructions":
			return grokEvidence{}, "probe_unexpected_instructions"
		case !grokRequestKeys[key]:
			return grokEvidence{}, "probe_mismatch"
		}
	}
	var request struct {
		Model     string `json:"model"`
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Text struct {
			Format struct {
				Type   string          `json:"type"`
				Strict bool            `json:"strict"`
				Schema json.RawMessage `json:"schema"`
			} `json:"format"`
		} `json:"text"`
		Input []map[string]json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &request) != nil {
		return grokEvidence{}, "probe_invalid_request"
	}
	if request.Model != want.model {
		return grokEvidence{}, "probe_changed_model"
	}
	if want.effort != "" && request.Reasoning.Effort != want.effort {
		return grokEvidence{}, "probe_changed_effort"
	}
	if request.Text.Format.Type != "json_schema" || rawjson.Absent(request.Text.Format.Schema) {
		return grokEvidence{}, "probe_invalid_schema"
	}
	if !request.Text.Format.Strict || !equalJSON(request.Text.Format.Schema, want.schema) {
		return grokEvidence{}, "probe_changed_schema"
	}
	return judgeGrokInput(request.Input, want)
}

// judgeGrokInput pins the conversation: our system text, then Grok's single
// vendor context message, then our prompt, and nothing else.
func judgeGrokInput(input []map[string]json.RawMessage, want grokExpectation) (grokEvidence, string) {
	messages := make([]struct{ role, content string }, 0, len(input))
	for _, item := range input {
		var kind, role, content string
		for key, value := range item {
			var text string
			if json.Unmarshal(value, &text) != nil {
				return grokEvidence{}, "probe_instruction_type"
			}
			switch key {
			case "type":
				kind = text
			case "role":
				role = text
			case "content":
				content = text
			default:
				return grokEvidence{}, "probe_instruction_type"
			}
		}
		if kind != "message" {
			return grokEvidence{}, "probe_instruction_type"
		}
		messages = append(messages, struct{ role, content string }{role, content})
	}
	if len(messages) == 0 || messages[0].role != "system" {
		return grokEvidence{}, "probe_missing_instructions"
	}
	if messages[0].content != codexInstructions {
		return grokEvidence{}, "probe_unexpected_instructions"
	}
	if len(messages) > 3 {
		return grokEvidence{}, "probe_unexpected_instructions"
	}
	if len(messages) < 3 || messages[1].role != "user" || messages[2].role != "user" {
		return grokEvidence{}, "probe_mismatch"
	}
	evidence, ok := parseGrokContext(messages[1].content, want.work)
	if !ok {
		return grokEvidence{}, "probe_unexpected_instructions"
	}
	if messages[2].content != want.prompt {
		return grokEvidence{}, "probe_mismatch"
	}
	return evidence, ""
}

func equalJSON(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	l, _ := json.Marshal(left)
	r, _ := json.Marshal(right)
	return bytes.Equal(l, r)
}

// The vendor context's fixed parts. Grok 1.0.41 sends, as a user message
// before the prompt, a <user_info> block of four stated facts and a <rules>
// block whose only subsection is its built-in user rules. Any other
// subsection (project rules, memories, skills) or any other text is refused.
const (
	grokInfoOpen   = "<user_info>\n"
	grokInfoClose  = "\n</user_info>\n\n"
	grokRulesOpen  = "<rules>\nThe rules section has a number of possible rules/memories/context that you should consider. In each subsection, we provide instructions about what information the subsection contains and how you should consider/follow the contents of the subsection.\n\n\n<user_rules description=\"These are rules set by the user that you should follow if appropriate.\">\n"
	grokRulesClose = "</user_rules>\n</rules>"
	grokRuleOpen   = "<user_rule>\n"
	grokRuleClose  = "\n</user_rule>\n"
)

var grokInfoKeys = []string{"OS Version: ", "Shell: ", "Workspace Path: ", "Today's date: "}

var grokContextMarkers = []string{"<user_rule", "</user_rule", "<rules", "</rules", "<user_info", "</user_info", "<system-reminder", "<skills", "<memories", "<project_rules", "<agents_md"}

// parseGrokContext accepts only the pinned vendor context for a launch in
// work, returning what a real run's transcript must repeat.
func parseGrokContext(text, work string) (grokEvidence, bool) {
	info, rules, ok := strings.Cut(text, grokInfoClose)
	if !ok || !strings.HasPrefix(info, grokInfoOpen) {
		return grokEvidence{}, false
	}
	lines := strings.Split(strings.TrimPrefix(info, grokInfoOpen), "\n")
	if len(lines) != len(grokInfoKeys) {
		return grokEvidence{}, false
	}
	values := make([]string, len(lines))
	for i, line := range lines {
		value, ok := strings.CutPrefix(line, grokInfoKeys[i])
		if !ok || value == "" || strings.ContainsAny(value, "<>") {
			return grokEvidence{}, false
		}
		values[i] = value
	}
	if !sameWorkspace(values[2], work) {
		return grokEvidence{}, false
	}
	if _, err := time.Parse("2006-01-02", values[3]); err != nil {
		return grokEvidence{}, false
	}
	if !grokRules(rules) {
		return grokEvidence{}, false
	}
	return grokEvidence{os: values[0], shell: values[1], rules: rules}, true
}

func sameWorkspace(stated, work string) bool {
	if stated == work {
		return true
	}
	resolved, err := filepath.EvalSymlinks(work)
	return err == nil && stated == resolved
}

// grokRules accepts the rules wrapper holding zero or more user_rule blocks
// and nothing else.
func grokRules(rules string) bool {
	body, ok := strings.CutPrefix(rules, grokRulesOpen)
	if !ok {
		return false
	}
	body, ok = strings.CutSuffix(body, grokRulesClose)
	if !ok {
		return false
	}
	for first := true; body != ""; first = false {
		if !first {
			if body, ok = strings.CutPrefix(body, "\n"); !ok {
				return false
			}
		}
		if body, ok = strings.CutPrefix(body, grokRuleOpen); !ok {
			return false
		}
		end := strings.Index(body, grokRuleClose)
		if end < 0 {
			return false
		}
		for _, marker := range grokContextMarkers {
			if strings.Contains(body[:end], marker) {
				return false
			}
		}
		body = body[end+len(grokRuleClose):]
	}
	return true
}

// grokTranscriptLimit bounds a persisted transcript read.
const grokTranscriptLimit = 64 << 20

// judgeTranscript checks the transcript Grok persisted for one launch: our
// system text, the vendor context the probe saw (only the workspace and date
// may differ), our prompt, and no further system or user entry after it.
func (e grokEvidence) judgeTranscript(transcript []byte, prompt, work string) string {
	if transcript == nil {
		return "missing_native_transcript"
	}
	var entries []struct {
		kind, text string
		ok         bool
	}
	for _, line := range bytes.Split(transcript, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(line, &entry) != nil {
			return "unexpected_native_instructions"
		}
		text, ok := grokTranscriptText(entry.Content)
		entries = append(entries, struct {
			kind, text string
			ok         bool
		}{entry.Type, text, ok})
	}
	if len(entries) < 3 {
		return "unexpected_native_instructions"
	}
	if entries[0].kind != "system" || !entries[0].ok || entries[0].text != codexInstructions {
		return "unexpected_native_instructions"
	}
	if entries[1].kind != "user" || !entries[1].ok {
		return "unexpected_native_instructions"
	}
	seen, ok := parseGrokContext(entries[1].text, work)
	if !ok || seen != e {
		return "unexpected_native_instructions"
	}
	if entries[2].kind != "user" || !entries[2].ok || entries[2].text != prompt {
		return "unexpected_native_instructions"
	}
	for _, entry := range entries[3:] {
		if entry.kind == "system" || entry.kind == "user" {
			return "unexpected_native_instructions"
		}
	}
	return ""
}

// grokTranscriptText reads an entry's content: a string, or exactly one text
// part.
func grokTranscriptText(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) != 1 || len(parts[0]) != 2 {
		return "", false
	}
	var kind string
	if json.Unmarshal(parts[0]["type"], &kind) != nil || kind != "text" || json.Unmarshal(parts[0]["text"], &text) != nil {
		return "", false
	}
	return text, true
}
