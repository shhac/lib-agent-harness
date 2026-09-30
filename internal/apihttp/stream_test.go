package apihttp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness"
)

func streamRequest(rt http.RoundTripper, limit int) Request {
	return Request{Method: http.MethodPost, URL: "https://gateway.invalid/v1/chat/completions", Body: []byte(`{}`), Token: "secret-token", Transport: rt, Limit: limit}
}

// piped answers with a body the test writes as it goes.
func piped(t *testing.T) (roundTrip, *io.PipeWriter) {
	t.Helper()
	reader, writer := io.Pipe()
	t.Cleanup(func() { writer.Close() })
	return func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader, Request: r}, nil
	}, writer
}

func collect(events *[]string) func([]byte) error {
	return func(data []byte) error {
		*events = append(*events, string(data))
		return nil
	}
}

func TestStreamFramesEvents(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"comments and fields":      {": keepalive\nevent: chunk\nid: 1\ndata: a\n\n" + "data: [DONE]\n\n", []string{"a"}},
		"multi-line data":          {"data: a\ndata: b\n\ndata: [DONE]\n\n", []string{"a\nb"}},
		"crlf":                     {"data: a\r\n\r\ndata: [DONE]\r\n\r\n", []string{"a"}},
		"terminator at the end":    {"data: a\n\ndata: [DONE]", []string{"a"}},
		"no space after colon":     {"data:a\n\ndata:[DONE]\n\n", []string{"a"}},
		"nothing after terminator": {"data: a\n\ndata: [DONE]\n\ndata: b\n\n", []string{"a"}},
	} {
		t.Run(name, func(t *testing.T) {
			var events []string
			whole, err := Stream(context.Background(), streamRequest(respond(200, "text/event-stream", tc.body), 1<<20), time.Second, collect(&events))
			if err != nil || whole != nil || strings.Join(events, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("%q %s %v", events, whole, err)
			}
		})
	}
}

func TestStreamRefusesWhatIsNotACompleteStream(t *testing.T) {
	var events []string
	_, err := Stream(context.Background(), streamRequest(respond(200, "text/event-stream", "data: a\n\n"), 1<<20), time.Second, collect(&events))
	requireCode(t, err, PhaseResponse, harness.CauseUnknown, "stream_incomplete")

	_, err = Stream(context.Background(), streamRequest(respond(200, "text/html", "secret"), 1<<20), time.Second, collect(&events))
	requireCode(t, err, PhaseResponse, harness.CauseUnknown, "unexpected_media_type")

	failure := requireCode(t, streamErr(respond(429, "application/json", `{"error":{"message":"secret"}}`, "Retry-After", "7")), PhaseResponse, harness.CauseRateLimited, "http_429")
	if failure.RetryAfter != 7*time.Second {
		t.Fatalf("a streaming refusal lost its stated delay: %+v", failure)
	}

	_, err = Stream(context.Background(), streamRequest(respond(200, "application/json", `{"choices":[]}`), 5), time.Second, collect(&events))
	requireCode(t, err, PhaseResponse, harness.CauseUnknown, "output_limit")

	whole, err := Stream(context.Background(), streamRequest(respond(200, "application/json", `{"choices":[]}`), 100), time.Second, collect(&events))
	if err != nil || string(whole) != `{"choices":[]}` {
		t.Fatalf("an endpoint that answered whole was not read whole: %s %v", whole, err)
	}

	broken := roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret dial failure") })
	requireCode(t, streamErr(broken), PhaseTransport, harness.CauseUnknown, "transport_failed")

	var callbackErr = errors.New("stop")
	if _, err = Stream(context.Background(), streamRequest(respond(200, "text/event-stream", "data: a\n\n"), 100), time.Second, func([]byte) error { return callbackErr }); !errors.Is(err, callbackErr) {
		t.Fatalf("a reader's refusal was not returned: %v", err)
	}
}

func streamErr(rt http.RoundTripper) error {
	_, err := Stream(context.Background(), streamRequest(rt, 1<<20), time.Second, func([]byte) error { return nil })
	return err
}

// Every line resets the idle bound, so the byte limit is what stops an
// endpoint that sends keepalives forever.
func TestStreamBoundsAKeepaliveFlood(t *testing.T) {
	rt, writer := piped(t)
	go func() {
		for {
			if _, err := io.WriteString(writer, ": keepalive\n"); err != nil {
				return
			}
		}
	}()
	_, err := Stream(context.Background(), streamRequest(rt, 4096), time.Minute, func([]byte) error { return nil })
	requireCode(t, err, PhaseResponse, harness.CauseUnknown, "output_limit")
}

// A caller who stops waiting is told it was cancelled, not that the endpoint
// stalled; a caller's deadline is a timeout of the request, not of the stream.
func TestStreamKeepsTheCallersCancellation(t *testing.T) {
	rt, writer := piped(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _ = io.WriteString(writer, "data: a\n\n")
	}()
	_, err := Stream(ctx, streamRequest(rt, 1<<20), time.Minute, func([]byte) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation became %v", err)
	}

	rt, _ = piped(t)
	deadline, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	_, err = Stream(deadline, streamRequest(rt, 1<<20), time.Minute, func([]byte) error { return nil })
	requireCode(t, err, PhaseResponse, harness.CauseTimeout, "deadline_exceeded")
}

func TestStreamSendsItsOwnHeaders(t *testing.T) {
	var seen http.Header
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Clone()
		return respond(200, "text/event-stream", "data: [DONE]\n\n")(r)
	})
	if _, err := Stream(context.Background(), streamRequest(rt, 100), time.Second, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if seen.Get("Authorization") != "Bearer secret-token" || seen.Get("Accept") != "text/event-stream" || seen.Get("Content-Type") != "application/json" || seen.Get("User-Agent") != "lib-agent-harness" {
		t.Fatalf("headers %v", seen)
	}
}
