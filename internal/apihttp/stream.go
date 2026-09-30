package apihttp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/shhac/lib-agent-harness"
)

// defaultIdleTimeout bounds the silence between streamed events when the
// caller chose none. A reasoning model can send nothing while it thinks, and
// not every endpoint sends keepalives meanwhile, so it is as long as the whole
// of a non-streaming request's default deadline: streaming never fails on
// silence sooner than not streaming would.
const defaultIdleTimeout = 5 * time.Minute

// errStreamDone ends a read at the stream's terminator.
var errStreamDone = errors.New("stream done")

// Stream sends the request asking for server-sent events and hands each
// event's data to onEvent, in order, until the stream's [DONE]. The silence
// between lines, keepalive comments included, is bounded by idle; the whole
// exchange stays bounded by ctx and by r.Limit bytes.
//
// An endpoint may ignore the request to stream and answer with one JSON body.
// That body is returned instead, for the caller to read as it would any
// non-streaming response.
//
// A stream that ends without [DONE] is incomplete, whatever it carried: the
// provider did not say it had finished.
func Stream(ctx context.Context, r Request, idle time.Duration, onEvent func([]byte) error) ([]byte, error) {
	if idle <= 0 {
		idle = defaultIdleTimeout
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	timer := time.AfterFunc(idle, func() {
		stalled.Store(true)
		cancel()
	})
	defer timer.Stop()
	request, err := newRequest(ctx, r, "text/event-stream")
	if err != nil {
		return nil, err
	}
	response, err := client(r.Transport).Do(request)
	if err != nil {
		return nil, streamInterrupted(ctx, &stalled, PhaseTransport, err)
	}
	defer response.Body.Close()
	// net/http's own transport unblocks a read on cancellation; closing the
	// body makes a stall stop a caller-supplied transport's read too.
	stopClosing := context.AfterFunc(ctx, func() { _ = response.Body.Close() })
	defer stopClosing()
	if response.StatusCode != http.StatusOK {
		return nil, StatusFailure(response)
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	switch mediaType {
	case "application/json":
		return readBounded(response.Body, r.Limit, func(err error) error { return streamInterrupted(ctx, &stalled, PhaseResponse, err) })
	case "text/event-stream":
	default:
		return nil, ResponseFailure("unexpected_media_type")
	}
	err = readEvents(io.LimitReader(response.Body, int64(r.Limit)+1), r.Limit, func() { timer.Reset(idle) }, onEvent)
	switch {
	case errors.Is(err, errStreamDone):
		return nil, nil
	case err == nil:
		return nil, ResponseFailure("stream_incomplete")
	}
	var reader eventError
	if errors.As(err, &reader) {
		return nil, reader.err
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return nil, failure
	}
	return nil, streamInterrupted(ctx, &stalled, PhaseResponse, err)
}

// eventError carries the reader's own refusal of an event out of the read, so
// it is not mistaken for the stream failing.
type eventError struct{ err error }

func (e eventError) Error() string { return e.err.Error() }

// readEvents splits a server-sent event stream into events' data. It returns
// errStreamDone at [DONE], nil at an end with no terminator, and any error
// onEvent returns as an eventError.
func readEvents(body io.Reader, limit int, progressed func(), onEvent func([]byte) error) error {
	reader := bufio.NewReaderSize(body, 64<<10)
	read := 0
	var data []byte
	pending := false
	for {
		line, err := reader.ReadBytes('\n')
		read += len(line)
		if read > limit {
			return ResponseFailure("output_limit")
		}
		if len(line) > 0 {
			progressed()
		}
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0 && pending:
			if bytes.Equal(data, []byte("[DONE]")) {
				return errStreamDone
			}
			if callbackErr := onEvent(data); callbackErr != nil {
				return eventError{callbackErr}
			}
			data, pending = nil, false
		case bytes.HasPrefix(line, []byte("data:")):
			value := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
			if pending {
				data = append(data, '\n')
			}
			data, pending = append(data, value...), true
		}
		// Comments (":"), event names, ids and retry hints carry nothing to
		// read; a comment is still progress, which is what a keepalive is for.
		if err == io.EOF {
			if pending && bytes.Equal(data, []byte("[DONE]")) {
				return errStreamDone
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// streamInterrupted tells a stalled stream from the caller's cancellation.
func streamInterrupted(ctx context.Context, stalled *atomic.Bool, phase Phase, err error) error {
	if stalled.Load() {
		return &Failure{Cause: harness.CauseTimeout, Phase: phase, Code: "stream_idle"}
	}
	return Interrupted(ctx, phase, err)
}
