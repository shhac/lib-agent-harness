//go:build !windows

package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
	"github.com/shhac/lib-agent-harness/internal/testenv"
)

type fakeLoopbackDelivery struct{ Network, Endpoint, Nonce string }

// Fixture transport only: it never runs the returned shell command. Scripted
// socket deliveries are restricted to disposable test-owned loopback sockets.
func fakeClaudeLoopbackResult(base string) int {
	logInvocation("canary-provider:" + base)
	logInvocation("canary-home:" + os.Getenv("CLAUDE_CONFIG_DIR"))
	client := &http.Client{Timeout: 10 * time.Second}
	body := []byte(`{"stream":true,"tools":[{"name":"Bash"}],"messages":[]}`)
	response, err := client.Post(base+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		return 2
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes)
	command := ""
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var event struct {
			Delta struct {
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &event) != nil || event.Delta.PartialJSON == "" {
			continue
		}
		var call struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(event.Delta.PartialJSON), &call) != nil {
			return 2
		}
		command = call.Command
	}
	if scanner.Err() != nil || command == "" {
		return 2
	}
	logInvocation("canary:" + string(mustMarshal(command)))
	if os.Getenv(fakeLoopbackWaitEnv) == "1" {
		logInvocation("canary-wait")
		time.Sleep(time.Minute)
		return 2
	}
	var deliveries []fakeLoopbackDelivery
	if raw := os.Getenv(fakeLoopbackWitnessEnv); raw != "" && json.Unmarshal([]byte(raw), &deliveries) != nil {
		return 2
	}
	for _, d := range deliveries {
		host, _, err := net.SplitHostPort(d.Endpoint)
		if err != nil || !net.ParseIP(host).IsLoopback() || (d.Network != "tcp" && d.Network != "udp") {
			return 2
		}
		conn, err := net.DialTimeout(d.Network, d.Endpoint, time.Second)
		if err != nil {
			return 2
		}
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		_, err = io.WriteString(conn, d.Nonce)
		conn.Close()
		if err != nil {
			return 2
		}
	}
	reply, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": loopbackCanaryID, "content": os.Getenv(fakeLoopbackOutputEnv)}}}}})
	return fakePost(base+"/v1/messages", reply)
}

func TestClaudeLinuxCandidateTransportAndSettlement(t *testing.T) {
	testenv.RequireLoopback(t)
	testenv.RequireProcessGroup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	binary, log := fakeHarness(t, fakeLoopbackFixture, fakeLoopbackOutputEnv+"=bound\ncanary-ran\n")
	raw := sandboxOptions(t, harness.Claude, binary, true)
	raw.Sandbox.Loopback = true
	o, err := normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	l := &launch{extra: sandboxArgs(o)}
	output, received, err := runClaudeLoopbackCanaryObserved(ctx, o, l, loopbackCanary(1, "192.0.2.1", 443, 2))
	if err != nil || !received {
		t.Fatalf("tool-result=%t err=%v", received, err)
	}
	evidence, err := judgeClaudeLoopbackForPlatform(ctx, "linux", "per-command", output, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := "fixture-linux-scope-" + newID()
	if err := verified.settleSandbox(ctx, harness.Claude, key, evidence, nil); err != nil {
		t.Fatal(err)
	}
	verified.mu.Lock()
	recorded := verified.loopback[key].scope
	delete(verified.loopback, key)
	delete(verified.seen, key)
	verified.mu.Unlock()
	if recorded != "per-command" {
		t.Fatal("scope not recorded")
	}
	if invocations(t, log, "probe") != 1 {
		t.Fatal("fixture did not drive native transport")
	}
	assertClaudeProbeCleanup(t, log)

	_, witnesses, payload, cleanup, err := ncInterfaceCandidate([]string{"127.0.0.1"}, t.TempDir(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	nonce, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	a := sandboxprobe.InterfaceAttempts([]string{"127.0.0.1"}, false, true)
	var deliveries []fakeLoopbackDelivery
	var endpoints []string
	for _, w := range witnesses {
		network := "udp"
		if w.operation == "bind" {
			network = "tcp"
		}
		endpoint := net.JoinHostPort(w.destination, strconv.Itoa(w.port))
		endpoints = append(endpoints, endpoint)
		deliveries = append(deliveries, fakeLoopbackDelivery{network, endpoint, string(nonce)})
	}
	encoded, _ := json.Marshal(deliveries)
	result := "interface-result:0:0\ninterface-result:1:0\ninterface-result:2:0\ninterface-canary-ran\nnamespace-interfaces:[\"lo\"]\ninterface-witness-ran\n"
	binary, _ = fakeHarness(t, fakeLoopbackFixture, fakeLoopbackOutputEnv+"="+result, fakeLoopbackWitnessEnv+"="+string(encoded))
	o.Provider.CLI.Binary = binary
	script := claudeLinuxInterfaceCanary(a, witnesses, string(nonce))
	if !flatCanary(script) {
		t.Fatal("exact script is not flat")
	}
	output, received, err = runClaudeLoopbackCanaryObserved(ctx, o, l, script)
	if err != nil || !received {
		t.Fatal(err)
	}
	// Wait only for actual listener observation, bounded by the test deadline.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, w := range witnesses {
			w.mu.Lock()
			all = all && w.matched
			w.mu.Unlock()
		}
		if all {
			break
		}
		time.Sleep(time.Millisecond)
	}
	rows, summary, err := judgeClaudeLinuxInterfaces(output, a, claudeWitnessMatched(witnesses), "host-shared")
	if err != nil || len(rows) != 3 || summary != "all-interfaces" {
		t.Fatalf("%s %v", summary, err)
	}
	cleanup()
	for i, w := range witnesses {
		w.mu.Lock()
		matched := w.matched
		w.mu.Unlock()
		if !matched {
			t.Fatalf("nonce witness %d not reached", i)
		}
		if w.operation == "bind" {
			tcp, e := net.Listen("tcp", endpoints[i])
			if e != nil {
				t.Fatal("TCP listener left open", e)
			}
			tcp.Close()
		} else {
			listener, e := net.ListenPacket("udp", endpoints[i])
			if e != nil {
				t.Fatal("UDP listener left open", e)
			}
			listener.Close()
		}
	}
}

func assertClaudeProbeCleanup(t *testing.T, log string) {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if base, ok := strings.CutPrefix(line, "canary-provider:http://"); ok {
			c, e := net.DialTimeout("tcp", base, time.Second)
			if e == nil {
				c.Close()
				t.Fatal("local provider listener left open")
			}
		}
		if home, ok := strings.CutPrefix(line, "canary-home:"); ok && home != "" {
			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatal("disposable home remains", err)
			}
		}
	}
}

func TestClaudeLinuxCandidateCancellationPublishesNothing(t *testing.T) {
	testenv.RequireLoopback(t)
	testenv.RequireProcessGroup(t)
	binary, log := fakeHarness(t, fakeLoopbackFixture, fakeLoopbackWaitEnv+"=1")
	raw := sandboxOptions(t, harness.Claude, binary, true)
	raw.Sandbox.Loopback = true
	o, err := normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			raw, _ := os.ReadFile(log)
			if strings.Contains(string(raw), "canary-wait") {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	output, _, err := runClaudeLoopbackCanaryObserved(ctx, o, &launch{extra: sandboxArgs(o)}, "echo canary-ran\n")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := judgeClaudeLoopbackForPlatform(ctx, "linux", "per-command", output, false, nil)
	cache := &verificationCache{seen: map[string]bool{}}
	err = cache.settleSandbox(ctx, harness.Claude, "cancelled", evidence, err)
	if e, ok := err.(*CapabilityError); !ok || e.Code != CapabilityProbeTimeout || cache.holds("cancelled") || cache.loopback != nil {
		t.Fatalf("cancelled proof published: %v", err)
	}
	assertClaudeProbeCleanup(t, log)
}

func TestClaudeLinuxNonceWitnessRequiresExactPayload(t *testing.T) {
	testenv.RequireLoopback(t)
	_, ws, payload, cleanup, err := ncInterfaceCandidate([]string{"127.0.0.1"}, t.TempDir(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	nonce, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		network := "udp"
		if w.operation == "bind" {
			network = "tcp"
		}
		for _, data := range []string{"incorrect", string(nonce) + "extra", string(nonce)} {
			c, err := net.DialTimeout(network, net.JoinHostPort(w.destination, strconv.Itoa(w.port)), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.WriteString(c, data)
			c.Close()
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for {
				w.mu.Lock()
				matched := w.matched
				if network == "tcp" {
					matched = w.payloadMatched
				}
				w.mu.Unlock()
				if data != string(nonce) {
					time.Sleep(50 * time.Millisecond)
					w.mu.Lock()
					matched = w.matched
					if network == "tcp" {
						matched = w.payloadMatched
					}
					w.mu.Unlock()
					if matched {
						t.Fatal("wrong nonce matched")
					}
					break
				}
				if matched {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("nonce not observed")
				}
				time.Sleep(time.Millisecond)
			}
		}
	}
}
