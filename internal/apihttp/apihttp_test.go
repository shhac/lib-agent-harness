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

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, contentType, body string, header ...string) roundTrip {
	return func(r *http.Request) (*http.Response, error) {
		response := &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
		for i := 0; i+1 < len(header); i += 2 {
			response.Header.Set(header[i], header[i+1])
		}
		return response, nil
	}
}

func requireCode(t *testing.T, err error, phase Phase, cause harness.Cause, code string) *Failure {
	t.Helper()
	var failure *Failure
	if !errors.As(err, &failure) || failure.Phase != phase || failure.Cause != cause || failure.Code != code {
		t.Fatalf("want %s/%s/%s, got %#v (%v)", phase, cause, code, failure, err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
	return failure
}

func TestDefaultTransportIgnoresAmbientProxy(t *testing.T) {
	transport, ok := DefaultTransport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("the API transport must not route credentials through ambient proxy settings")
	}
}

func TestDoBoundsAndClassifies(t *testing.T) {
	request := func(rt http.RoundTripper, limit int) ([]byte, error) {
		return Do(context.Background(), Request{Method: http.MethodGet, URL: "https://gateway.invalid/v1/models", Token: "secret-token", Transport: rt, Limit: limit})
	}
	if data, err := request(respond(200, "application/json; charset=utf-8", `{"ok":true}`), 11); err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("%s %v", data, err)
	}
	_, err := request(respond(200, "application/json", `{"ok":true}`), 10)
	requireCode(t, err, PhaseResponse, harness.CauseUnknown, "output_limit")
	_, err = request(respond(200, "text/plain", `secret`), 100)
	requireCode(t, err, PhaseResponse, harness.CauseUnknown, "unexpected_media_type")
	_, err = request(respond(307, "application/json", ``, "Location", "https://elsewhere.invalid/"), 100)
	requireCode(t, err, PhaseResponse, harness.CauseUnknown, "redirect_refused")
	_, err = request(respond(404, "application/json", `{"error":{"code":"model_not_found","message":"secret"}}`), 100)
	requireCode(t, err, PhaseResponse, harness.CauseModelUnavailable, "model_not_found")
	_, err = request(respond(503, "application/json", `{}`, "Retry-After", "99999"), 100)
	if failure := requireCode(t, err, PhaseResponse, harness.CauseUnavailable, "http_503"); failure.RetryAfter != 0 || !failure.Retryable() {
		t.Fatalf("%+v", failure)
	}
	_, err = request(respond(529, "application/json", `{}`, "Retry-After", "3"), 100)
	if failure := requireCode(t, err, PhaseResponse, harness.CauseOverloaded, "http_529"); failure.RetryAfter != 3*time.Second {
		t.Fatalf("%+v", failure)
	}
	_, err = request(roundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret dial") }), 100)
	requireCode(t, err, PhaseTransport, harness.CauseUnknown, "transport_failed")
}

func TestDoSendsOnlyTheLibraryHeaders(t *testing.T) {
	var seen http.Header
	capture := func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Clone()
		return respond(200, "application/json", `{}`)(r)
	}
	if _, err := Do(context.Background(), Request{Method: http.MethodPost, URL: "https://gateway.invalid/v1/x", Body: []byte(`{}`), Token: "t", Transport: roundTrip(capture), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if seen.Get("Authorization") != "Bearer t" || seen.Get("Content-Type") != "application/json" || len(seen) != 4 {
		t.Fatal(seen)
	}
	if _, err := Do(context.Background(), Request{Method: http.MethodGet, URL: "https://gateway.invalid/v1/x", Transport: roundTrip(capture), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if _, ok := seen["Authorization"]; ok || seen.Get("Content-Type") != "" {
		t.Fatal(seen)
	}
}

func TestCredentialNeverRetainsTheSourceError(t *testing.T) {
	api := harness.API{Credentials: func(context.Context) (string, error) { return "", errors.New("secret store") }}
	_, err := Credential(context.Background(), api)
	requireCode(t, err, PhasePreflight, harness.CauseAuthentication, "credential_unavailable")
	ctx, cancel := context.WithCancel(context.Background())
	api.Credentials = func(context.Context) (string, error) { cancel(); return "secret", nil }
	if _, err := Credential(ctx, api); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if token, err := Credential(context.Background(), harness.API{Unauthenticated: true}); token != "" || err != nil {
		t.Fatal(token, err)
	}
}

func TestEchoes(t *testing.T) {
	if Echoes("anything", "") || !Echoes("xx-token-xx", "token") {
		t.Fatal("echo detection")
	}
}
