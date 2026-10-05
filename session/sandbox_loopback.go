package session

// Proving loopback networking: Claude Code's sandbox status reports nothing
// about the network, so the only evidence is a command that ran inside the
// sandbox. The harness is driven against a local provider that performs no
// inference: it answers the first turn with one scripted shell call, the
// canary, reads the call's result from the next request, and refuses every
// other request. The canary connects only to listeners this probe owns and to
// one off-machine address the probe has just connected to itself.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

const loopbackCanaryID = "toolu_harness_loopback_canary"

const loopbackReached = sandboxprobe.LoopbackReached
const loopbackBound = sandboxprobe.LoopbackBound
const loopbackOutside = sandboxprobe.LoopbackOutside

func loopbackCanary(a int, b string, c, d int) string { return sandboxprobe.LoopbackCanary(a, b, c, d) }
func offMachineWitness(ctx context.Context) (string, bool) {
	return sandboxprobe.OffMachineWitness(ctx)
}
func countingListener(a string) (net.Listener, *sync.WaitGroup, func() bool, error) {
	return sandboxprobe.CountingListener(a)
}
func freeLoopbackPort() (int, error) { return sandboxprobe.FreeLoopbackPort() }

// probeClaudeLoopback runs the canary through the session's own arguments in
// a disposable home with a dummy credential.
func probeClaudeLoopback(ctx context.Context, o Options, l *launch) (loopbackEvidence, error) {
	unavailable := &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	witness, ok := offMachineWitness(ctx)
	if !ok {
		return loopbackEvidence{}, unavailable
	}
	loop, loopWait, loopReached, err := countingListener("127.0.0.1:0")
	if err != nil {
		return loopbackEvidence{}, unavailable
	}
	defer func() { loop.Close(); loopWait.Wait() }()
	bindPort, err := freeLoopbackPort()
	if err != nil {
		return loopbackEvidence{}, unavailable
	}
	canary := loopbackCanary(loop.Addr().(*net.TCPAddr).Port, witness, 443, bindPort)

	output, err := runClaudeLoopbackCanary(ctx, o, l, canary)
	if err != nil {
		return loopbackEvidence{}, err
	}
	// An observed escape survives cancellation; positive output never does.
	if err := judgeLoopback(output, true); err != nil {
		if capability, ok := err.(*CapabilityError); ok && capability.Code == CapabilitySandboxNotEnforced {
			return loopbackEvidence{}, err
		}
	}
	time.Sleep(300 * time.Millisecond)
	evidence, err := settleClaudeLoopback(ctx, output, loopReached())
	if err == nil && runtime.GOOS == "darwin" {
		evidence.interfaces = probeClaudeInterfaceDiagnostics(ctx, o, l)
	}
	return evidence, err
}

// Settle negative escape evidence before deadlines; cancellation can never
// promote complete positive output into a verification success.
func settleClaudeLoopback(ctx context.Context, output string, reached bool) (loopbackEvidence, error) {
	return judgeClaudeLoopbackForPlatform(ctx, runtime.GOOS, "per-command", output, reached, nil)
}

// Shared transport for the installed-runtime nc candidate and the live Linux proof.
// No inference, native login or verification-cache success is produced here.
func runClaudeLoopbackCanary(ctx context.Context, o Options, l *launch, canary string, payload ...string) (string, error) {
	output, _, err := runClaudeLoopbackCanaryObserved(ctx, o, l, canary, payload...)
	return output, err
}

func runClaudeLoopbackCanaryObserved(ctx context.Context, o Options, l *launch, canary string, payload ...string) (string, bool, error) {
	if err := checkClaudeSandboxPrerequisites(runtime.GOOS, o, exec.LookPath); err != nil {
		return "", false, err
	}
	unavailable := &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	dir, err := os.MkdirTemp("", "agent-harness-loopback-")
	if err != nil {
		return "", false, unavailable
	}
	defer os.RemoveAll(dir)
	// Keep the UDP payload inside the native probe working directory. No
	// read permission or working-directory allowlist needs expansion.
	if len(payload) > 0 {
		data, err := os.ReadFile(payload[0])
		if err != nil {
			return "", false, unavailable
		}
		path := filepath.Join(dir, "udp-payload")
		if err := os.WriteFile(path, data, 0600); err != nil {
			return "", false, unavailable
		}
		canary = strings.ReplaceAll(canary, sandboxprobe.ShellQuote(payload[0]), sandboxprobe.ShellQuote(path))
	}
	provider, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", false, unavailable
	}
	result := make(chan string, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: canaryProvider(canary, result)}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(provider) }()
	defer func() { _ = server.Close(); <-done }()

	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var output string
	received := false
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		select {
		case output = <-result:
			received = true
		case <-probeCtx.Done():
		}
		cancel()
	}()
	_ = driveProbe(probeCtx, o, l, dir, provider.Addr().String())
	cancel()
	<-settled
	// A buffered result already delivered by the provider wins a cancellation race.
	if !received {
		select {
		case output = <-result:
			received = true
		default:
		}
	}
	return output, received, nil
}

// loopbackEvidence is private, process-local evidence of a settled whole proof.
type loopbackEvidence struct {
	// Observed Linux scope. Empty until a scope-aware whole proof settles.
	scope        string
	observations []sandboxprobe.InterfaceObservation
	// Optional nc observations, including unavailable measurements. These
	// describe diagnostics; only the base proof authorizes a session.
	interfaces []ncInterfaceObservation
	detail     string
}

func judgeLoopback(output string, reached bool) error {
	_, err := judgeClaudeLoopback(output, reached, nil)
	return err
}

// judgeClaudeLoopback settles escape evidence first, then completeness, then
// the advertised macOS bind contract. Send observations never prove binds.
// The errno branch is not a live Claude client: it retains the original judge
// regression cases. Production diagnostics use nc host observations instead.
func judgeClaudeLoopback(output string, loopReached bool, attempts []sandboxprobe.InterfaceAttempt) (loopbackEvidence, error) {
	lines := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		lines[strings.TrimSpace(line)] = true
	}
	if lines[loopbackOutside] {
		return loopbackEvidence{}, &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxNotEnforced, Phase: BeforeLaunch}
	}
	if !lines[canaryRan] || lines[canaryNoClient] || !lines[loopbackReached] || !lines[loopbackBound] || !loopReached {
		return loopbackEvidence{}, &CapabilityError{Engine: harness.Claude, Code: CapabilitySandboxUnavailable, Phase: BeforeLaunch}
	}
	evidence := loopbackEvidence{}
	if attempts != nil {
		observed, outcome := sandboxprobe.JudgeInterfaceAttempts(output, attempts, sandboxprobe.InterfaceAllLocal)
		if outcome == "" {
			outcome = sandboxprobe.RequireInterfaceExposure(observed)
		}
		if outcome != "" {
			return loopbackEvidence{}, &CapabilityError{Engine: harness.Claude, Code: string(outcome), Phase: BeforeLaunch}
		}
		evidence.observations = observed
		evidence.detail = sandboxprobe.InterfaceExposureDetail(observed, false)
	}
	return evidence, nil
}

// canaryProvider answers the turn's first request with the canary as one shell
// call, sends the call's result to result, and refuses everything else.
func canaryProvider(canary string, result chan<- string) http.Handler {
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			refuseInference(w)
			return
		}
		var request struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &request) != nil {
			refuseInference(w)
			return
		}
		if text, found := canaryResult(request.Messages); found {
			select {
			case result <- text:
			default:
			}
			refuseInference(w)
			return
		}
		shell := false
		for _, tool := range request.Tools {
			shell = shell || tool.Name == "Bash"
		}
		if !shell || !request.Stream {
			refuseInference(w)
			return
		}
		served := false
		once.Do(func() { served = true; writeCanaryCall(w, canary) })
		if !served {
			refuseInference(w)
		}
	})
}

func refuseInference(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `{"error":{"message":"local capability check; no inference performed"}}`)
}

// canaryResult finds the canary call's result among the request's messages.
func canaryResult(messages []struct {
	Content json.RawMessage `json:"content"`
}) (string, bool) {
	for _, message := range messages {
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block.Type != "tool_result" || block.ToolUseID != loopbackCanaryID {
				continue
			}
			var text string
			if json.Unmarshal(block.Content, &text) == nil {
				return text, true
			}
			var parts []struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(block.Content, &parts)
			var out strings.Builder
			for _, part := range parts {
				out.WriteString(part.Text + "\n")
			}
			return out.String(), true
		}
	}
	return "", false
}

// writeCanaryCall streams one assistant message whose only content is the
// canary as a Bash call.
func writeCanaryCall(w http.ResponseWriter, canary string) {
	input, _ := json.Marshal(map[string]any{"command": canary, "description": "Sandbox loopback check"})
	partial, _ := json.Marshal(string(input))
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	events := []struct{ name, data string }{
		{"message_start", `{"type":"message_start","message":{"id":"msg_harness_canary","type":"message","role":"assistant","model":"harness-canary","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"` + loopbackCanaryID + `","name":"Bash","input":{}}}`},
		{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + string(partial) + `}}`},
		{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":1}}`},
		{"message_stop", `{"type":"message_stop"}`},
	}
	for _, event := range events {
		_, _ = io.WriteString(w, "event: "+event.name+"\ndata: "+event.data+"\n\n")
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
