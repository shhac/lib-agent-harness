package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// imageGeneration is the installed Codex 0.159.2 ThreadItem shape, checked by
// generating an image through the library and reading back that native item.
// Tests use only synthetic payloads and paths, with no local file reads.
func TestCodexGeneratedImageCompletion(t *testing.T) {
	for name, tc := range map[string]struct {
		status  string
		result  string
		images  int
		omitted int
	}{
		"png":         {"completed", b64(pngBytes), 1, 0},
		"jpeg":        {"completed", b64([]byte("\xff\xd8\xffsynthetic-jpeg")), 1, 0},
		"gif":         {"completed", b64([]byte("GIF89asynthetic-gif")), 1, 0},
		"webp":        {"completed", b64([]byte("RIFF\x00\x00\x00\x00WEBPVP8 synthetic-webp")), 1, 0},
		"no bytes":    {"completed", "", 0, 0},
		"invalid":     {"completed", "not!base64", 0, 1},
		"unsupported": {"completed", b64([]byte("<svg/>")), 0, 1},
		"failed":      {"failed", b64(pngBytes), 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, turn := startedCodexTurn(t)
			path := "/not-read/generated.png"
			item := map[string]any{"type": "imageGeneration", "id": "image", "status": tc.status, "result": tc.result,
				"savedPath": path, "revisedPrompt": "a synthetic image", "transparentBackground": false, "failure": nil}
			frame := map[string]any{"method": "item/started", "params": map[string]any{"threadId": "session-1", "turnId": "turn-1", "item": item}}
			notify(s, string(mustMarshal(frame)))
			frame["method"] = "item/completed"
			notify(s, string(mustMarshal(frame)))
			notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
			events := toolEvents(t, turn)
			started := events["tool_started:image"]
			if started.Output != "" || len(started.Images) != 0 || started.ImagesOmitted != 0 {
				t.Fatalf("start published a result: %+v", started)
			}
			done := events["tool_completed:image"]
			if done.Tool != "imageGeneration" || done.Status != tc.status || done.Output != path || done.OutputTruncated ||
				len(done.Images) != tc.images || done.ImagesOmitted != tc.omitted {
				t.Fatalf("completion: %+v", done)
			}
			if tc.images > 0 {
				want, _ := base64.StdEncoding.DecodeString(tc.result)
				mediaType := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}[name]
				if !bytes.Equal(done.Images[0].Data, want) || done.Images[0].MediaType != mediaType {
					t.Fatalf("image payload: %+v", done.Images[0])
				}
			}
			if (tc.result != "" && strings.Contains(string(done.Input), tc.result)) || strings.Contains(done.Output, "base64") {
				t.Fatal("image encoding leaked into text")
			}
		})
	}
}

func TestCodexGeneratedImageBoundsKeepPath(t *testing.T) {
	for _, size := range []int{MaxToolImageBytes, MaxToolImageBytes + 1, MaxToolImageBytes + 3} {
		t.Run(string(mustMarshal(size)), func(t *testing.T) {
			s, _, turn := startedCodexTurn(t)
			data := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte("x"), size-len(pngBytes))...)
			item := map[string]any{"type": "imageGeneration", "id": "image", "status": "completed", "result": b64(data), "savedPath": "/generated/image.png"}
			notify(s, string(mustMarshal(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "session-1", "turnId": "turn-1", "item": item}})))
			notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
			// Four megabytes through the race detector can outlast the usual
			// three seconds while the rest of the suite runs alongside.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := turn.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			e := toolEvents(t, turn)["tool_completed:image"]
			if e.Output != "/generated/image.png" || e.OutputTruncated {
				t.Fatalf("image size lost the path: %+v", e)
			}
			if size <= MaxToolImageBytes {
				if len(e.Images) != 1 || e.ImagesOmitted != 0 || !bytes.Equal(e.Images[0].Data, data) {
					t.Fatal("image at the bound was lost")
				}
			} else if len(e.Images) != 0 || e.ImagesOmitted != 1 {
				t.Fatal("oversized image not counted as omitted")
			}
		})
	}
}

func TestCodexGeneratedImageMissingAndBoundedPath(t *testing.T) {
	for _, path := range []any{nil, strings.Repeat("é", MaxToolPayloadBytes)} {
		s, _, turn := startedCodexTurn(t)
		item := map[string]any{"type": "imageGeneration", "id": "image", "status": "completed", "result": b64(pngBytes), "savedPath": path}
		notify(s, string(mustMarshal(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": "session-1", "turnId": "turn-1", "item": item}})))
		notify(s, `{"method":"turn/completed","params":{"threadId":"session-1","turn":{"id":"turn-1","status":"completed"}}}`)
		e := toolEvents(t, turn)["tool_completed:image"]
		if len(e.Images) != 1 || len(e.Output) > MaxToolPayloadBytes || e.OutputTruncated != (path != nil) {
			t.Fatal("path/image bounds changed")
		}
		if path == nil && e.Output != "" {
			t.Fatal("invented a path")
		}
		// Ensure the resulting event remains serializable with binary image bytes.
		if _, err := json.Marshal(e); err != nil {
			t.Fatal(err)
		}
	}
}
