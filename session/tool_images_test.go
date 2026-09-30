package session

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\nfake-image")

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestToolResultTakesImagesOutOfTheText(t *testing.T) {
	claude := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64(pngBytes) + `"}}`
	mcp := `{"type":"image","mimeType":"image/jpeg","data":"` + b64([]byte("jpeg")) + `"}`
	for name, tc := range map[string]struct {
		raw     string
		text    string
		images  int
		omitted int
	}{
		"claude image beside text": {`[{"type":"text","text":"shot taken"},` + claude + `]`, "shot taken", 1, 0},
		"mcp image alone":          {`[` + mcp + `]`, "", 1, 0},
		"svg is not taken":         {`[{"type":"image","mimeType":"image/svg+xml","data":"` + b64([]byte("<svg/>")) + `"}]`, "", 0, 1},
		"not base64":               {`[{"type":"image","mimeType":"image/png","data":"%%%"}]`, "", 0, 1},
		"too large":                {`[{"type":"image","mimeType":"image/png","data":"` + b64(make([]byte, MaxToolImageBytes+1)) + `"}]`, "", 0, 1},
		"too many":                 {`[` + strings.Repeat(mcp+",", MaxToolImages) + mcp + `]`, "", MaxToolImages, 1},
		"other content kept":       {`[{"type":"resource","uri":"x"}]`, `[{"type":"resource","uri":"x"}]`, 0, 0},
	} {
		text, images := toolResult([]byte(tc.raw))
		if text != tc.text || len(images.images) != tc.images || images.omitted != tc.omitted {
			t.Errorf("%s: text %q, %d images, %d omitted", name, text, len(images.images), images.omitted)
		}
	}
	_, images := toolResult([]byte(`[` + claude + `]`))
	if img := images.images[0]; img.MediaType != "image/png" || !bytes.Equal(img.Data, pngBytes) {
		t.Fatalf("decoded %+v", img)
	}
}

func TestToolImageBoundAllowsBase64LineBreaks(t *testing.T) {
	data := bytes.Repeat([]byte("x"), MaxToolImageBytes)
	encoded := b64(data)
	var wrapped strings.Builder
	for len(encoded) > 76 {
		wrapped.WriteString(encoded[:76])
		wrapped.WriteString("\r\n")
		encoded = encoded[76:]
	}
	wrapped.WriteString(encoded)
	var images toolImages
	images.add(toolContentBlock{Type: "image", MimeType: "image/png", Data: wrapped.String()})
	if images.omitted != 0 || len(images.images) != 1 || !bytes.Equal(images.images[0].Data, data) {
		t.Fatal("a valid image at the decoded bound was lost because of line breaks")
	}
}

// The shape is Claude Code 2.1.283's, from a claude-in-chrome screenshot.
func TestClaudeToolEventsCarryImages(t *testing.T) {
	s, _ := fakeSession(t, harness.Claude)
	turn, err := s.StartTurn(testContext(t), Input{"start"})
	if err != nil {
		t.Fatal(err)
	}
	notify(s, `{"type":"assistant","session_id":"session-1","message":{"id":"a","content":[{"type":"tool_use","id":"shot","name":"mcp__claude-in-chrome__computer","input":{"action":"screenshot"}}]}}`)
	notify(s, `{"type":"user","session_id":"session-1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"shot","content":[{"type":"text","text":"Successfully captured screenshot"},{"type":"text","text":"Tab Context: ..."},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"`+b64([]byte("jpeg-bytes"))+`"}}]}]}}`)
	finishClaude(s, false)
	e := toolEvents(t, turn)["tool_completed:shot"]
	if len(e.Images) != 1 || e.Images[0].MediaType != "image/jpeg" || string(e.Images[0].Data) != "jpeg-bytes" || strings.Contains(e.Output, "jpeg-bytes") || !strings.Contains(e.Output, "captured") {
		t.Fatalf("%+v", e)
	}
}

func TestCodexAndGrokToolEventsCarryImages(t *testing.T) {
	s, _, turn := startedCodexTurn(t)
	notify(s, `{"method":"item/completed","params":{"threadId":"session-1","turnId":"turn-1","item":{"type":"mcpToolCall","id":"mcp","server":"browser","tool":"screenshot","status":"completed","arguments":{},"result":{"content":[{"type":"image","mimeType":"image/png","data":"`+b64(pngBytes)+`"}]},"error":null}}}`)
	notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
	if e := toolEvents(t, turn)["tool_completed:mcp"]; len(e.Images) != 1 || e.Output != "" {
		t.Fatalf("codex %+v", e)
	}

	g, gturn, answer := grokFakeTurn(t)
	notify(g, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-1","update":{"sessionUpdate":"tool_call_update","toolCallId":"view","status":"completed","content":[{"type":"content","content":{"type":"text","text":"viewed"}},{"type":"content","content":{"type":"image","mimeType":"image/png","data":"`+b64(pngBytes)+`"}}]}}}`)
	answer()
	if e := toolEvents(t, gturn)["tool_completed:view"]; len(e.Images) != 1 || e.Output != "viewed" {
		t.Fatalf("grok %+v", e)
	}
}

func TestBackgroundIsRefusedWhereItIsNotOffered(t *testing.T) {
	api := Options{Provider: harness.Provider{Engine: harness.OpenAICompatible, API: harness.API{BaseURL: "http://127.0.0.1:1/v1", Dialect: harness.OpenAIChatCompletions, Unauthenticated: true}}, Model: "m", Background: true}
	if _, err := normalize(api); err == nil {
		t.Fatal("an API session accepted background priority")
	}
	claude := Options{Provider: harness.Provider{Engine: harness.Claude}, WorkDir: t.TempDir(), Background: true}
	if _, err := normalize(claude); (err == nil) != harness.Support(harness.Claude, harness.Session, harness.Background).Usable() {
		t.Fatalf("a Claude session's background priority disagrees with Support: %v", err)
	}
	plain := claude
	plain.Background = false
	if reference(claude, "id").ConfigHash != reference(plain, "id").ConfigHash {
		t.Fatal("background priority changed the reference")
	}
}
