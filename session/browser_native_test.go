package session

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

func TestCodexBrowserArgsAndReference(t *testing.T) {
	base := Options{Provider: harness.Provider{Engine: harness.Codex, CLI: harness.CLI{Home: t.TempDir()}}, WorkDir: t.TempDir()}
	o, err := normalize(base)
	if err != nil {
		t.Fatal(err)
	}
	plain := reference(o, "thread-1")
	if args := commandArgs(o, plain.ID, false, nil); !reflect.DeepEqual(args, []string{"app-server", "--listen", "stdio://"}) {
		t.Fatalf("ordinary launch changed: %q", args)
	}
	base.Browser = true
	o, err = normalize(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, resume := range []bool{false, true} {
		args := commandArgs(o, plain.ID, resume, nil)
		want := []string{"app-server", "--listen", "stdio://", "-c", "features.browser_use=true", "-c", "features.browser_use_external=true"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("resume %t: %q", resume, args)
		}
	}
	if reference(o, plain.ID).ConfigHash == plain.ConfigHash {
		t.Fatal("browser opt-in did not change the resume contract")
	}
	for _, restricted := range []bool{false, true} {
		limited := base
		if restricted {
			limited.Restriction = &Restriction{}
		} else {
			limited.Sandbox = &Sandbox{}
		}
		_, err := normalize(limited)
		var refusal *UnsupportedError
		if !errors.As(err, &refusal) || refusal.Code != RefusedConflict {
			t.Fatalf("restricted %t: %v", restricted, err)
		}
	}
}

func TestCodexBrowserStartupInventory(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		err  error
		code string
	}{
		"deferred bridge":          {body: `{"data":[{"name":"node_repl","tools":{"js":{"name":"js"},"js_reset":{"name":"js_reset"}}}]}`},
		"connected bridge":         {body: `{"data":[{"name":"node_repl","runtimeStatus":"connected","tools":{"js":{"name":"js"},"js_reset":{"name":"js_reset"}}}]}`},
		"missing":                  {body: `{"data":[]}`, code: CapabilityBrowserToolsMissing},
		"empty":                    {body: `{"data":[{"name":"node_repl","tools":{}}]}`, code: CapabilityBrowserToolsMissing},
		"foreign":                  {body: `{"data":[{"name":"other","tools":{"js":{"name":"js"},"js_reset":{"name":"js_reset"}}}]}`, code: CapabilityBrowserToolsMissing},
		"failed with cached tools": {body: `{"data":[{"name":"node_repl","runtimeStatus":"failed","toolsError":"secret diagnostic","tools":{"js":{"name":"js"},"js_reset":{"name":"js_reset"}}}]}`, code: CapabilityBrowserToolsMissing},
		"wrong identity":           {body: `{"data":[{"name":"node_repl","tools":{"js":{"name":"other"},"js_reset":{"name":"js_reset"}}}]}`, code: CapabilityBrowserToolsMissing},
		"old protocol":             {err: ErrRejected, code: CapabilityBrowserToolsMissing},
		"malformed":                {body: `{"data":"secret diagnostic"}`, code: CapabilityProbeUnreadable},
		"absent data":              {body: `{}`, code: CapabilityProbeUnreadable},
	} {
		for _, resume := range []bool{false, true} {
			t.Run(name+map[bool]string{false: " start", true: " resume"}[resume], func(t *testing.T) {
				s, w := fakeSession(t, harness.Codex)
				s.options.Browser = true
				w.requestFn = func(method string, p map[string]any) (json.RawMessage, error) {
					switch method {
					case "initialize":
						return json.RawMessage(`{}`), nil
					case "thread/start", "thread/resume":
						return json.RawMessage(`{"thread":{"id":"session-1"}}`), nil
					case "mcpServerStatus/list":
						want := map[string]any{"threadId": "session-1", "serverName": "node_repl", "detail": "toolsAndAuthOnly"}
						if !reflect.DeepEqual(p, want) {
							t.Fatalf("inventory parameters: %+v", p)
						}
						return json.RawMessage(tc.body), tc.err
					default:
						t.Fatalf("unexpected request: %s", method)
						return nil, ErrProtocol
					}
				}
				err := s.initialize(testContext(t), resume)
				if tc.code == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var failure *CapabilityError
					if !errors.As(err, &failure) || failure.Code != tc.code || failure.Phase != BeforeFirstPrompt || !errors.Is(err, ErrUnsupported) {
						t.Fatalf("failure: %v", err)
					}
					if strings.Contains(err.Error(), "secret diagnostic") {
						t.Fatal("provider text in error")
					}
					if tc.code == CapabilityBrowserToolsMissing && !strings.Contains(err.Error(), "https://chromewebstore.google.com/detail/chatgpt/hehggadaopoacecdllhhajmbjkdcmajg") {
						t.Fatalf("missing setup URL: %v", err)
					}
				}
				if slices.Contains(w.calls, "turn/start") {
					t.Fatal("startup check sent a prompt")
				}
			})
		}
	}
}

func TestOrdinaryClaudeBrowserStartupKeepsNativeTools(t *testing.T) {
	for _, browser := range []bool{false, true} {
		for _, available := range []bool{false, true} {
			s, _ := fakeSession(t, harness.Claude)
			s.options.Browser = browser
			frame := `{"type":"system","subtype":"init","mcp_servers":[],"tools":["Bash","mcp__other__read"]}`
			if available {
				frame = `{"type":"system","subtype":"init","mcp_servers":[{"name":"claude-in-chrome","status":"connected"}],"tools":["Bash","mcp__other__read","mcp__claude-in-chrome__read_page"]}`
			}
			notify(s, frame)
			if browser && !available {
				var failure *CapabilityError
				if !errors.As(s.failure, &failure) || failure.Code != CapabilityBrowserToolsMissing || !strings.Contains(failure.Error(), "https://chromewebstore.google.com/detail/claude/fcoeoabgfenejglbffodgkkbkcdhcgfn") {
					t.Fatalf("failure: %v", s.failure)
				}
			} else if s.failure != nil {
				t.Fatalf("ordinary tools rejected: %v", s.failure)
			}
		}
	}
}
