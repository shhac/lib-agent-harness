package session

// An OpenAI-compatible session's durable conversation: an append-only JSON
// Lines transcript, synced after every record, under an exclusive lock.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/completion"
)

// Record types, in the order a turn writes them.
const (
	recordSession    = "session"
	recordTurnStart  = "turn_start"
	recordUser       = "user"
	recordAssistant  = "assistant"
	recordToolCall   = "tool_call"
	recordToolResult = "tool_result"
	recordUsage      = "usage"
	recordTurnEnd    = "turn_end"
)

// Outcomes a tool result records beside the text the model saw.
const (
	outcomeRan     = ""
	outcomeRefused = "refused"
	outcomeNotRun  = "not_run"
	outcomeUnknown = "unknown"
)

const (
	unknownOutcomeText = "The outcome of this call is unknown: the session stopped while it was running, so its effects may or may not have happened. It was not run again; establish what it did from evidence before relying on it."
	notRunText         = "This call was not run: the turn ended before it started. Nothing was executed."
)

// record is one transcript line. Response numbers a model response within the
// session, from 1, so calls are keyed by response and call ID: endpoints do not
// all make call identifiers unique across a conversation.
type record struct {
	Type     string                `json:"type"`
	At       time.Time             `json:"at"`
	Turn     string                `json:"turn,omitempty"`
	Header   *transcriptHeader     `json:"session,omitempty"`
	Text     string                `json:"text,omitempty"`
	Response int                   `json:"response,omitempty"`
	Calls    []completion.ToolCall `json:"calls,omitempty"`
	// Replay is the provider state an assistant response carried, kept so a
	// resumed conversation sends it back as the endpoint requires.
	Replay  json.RawMessage `json:"replay,omitempty"`
	Call    string          `json:"call,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	Outcome string          `json:"outcome,omitempty"`
	Usage   *harness.Usage  `json:"usage,omitempty"`
	Status  string          `json:"status,omitempty"`
	Code    string          `json:"code,omitempty"`
}

// transcriptHeader binds a transcript to the reference that created it.
type transcriptHeader struct {
	Version    int            `json:"version"`
	Engine     harness.Engine `json:"engine"`
	ID         string         `json:"id"`
	ConfigHash string         `json:"config_hash"`
}

func (r record) key() string { return strconv.Itoa(r.Response) + ":" + r.Call }

// validSessionID admits only the identifiers this library generates, which
// are safe as a directory name on every platform.
var validSessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString

type transcript struct {
	mu     sync.Mutex
	file   *os.File
	lock   *os.File
	closed bool
}

func (t *transcript) append(r record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return stateError(StateUnwritable)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return ErrClosed
	}
	if _, err = t.file.Write(append(line, '\n')); err == nil {
		err = t.file.Sync()
	}
	if err != nil {
		return stateError(StateUnwritable)
	}
	return nil
}

func (t *transcript) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	_ = t.file.Close()
	_ = t.lock.Close()
}

func sessionDir(home, id string) string { return filepath.Join(home, "sessions", id) }
func transcriptPath(dir string) string  { return filepath.Join(dir, "transcript.jsonl") }

// createTranscript makes a new session's directory, locks it and writes the
// header that binds it to ref.
func createTranscript(home string, ref Ref) (*transcript, []record, error) {
	sessions := filepath.Join(home, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, nil, stateError(StateUnusable)
	}
	if info, err := os.Lstat(sessions); err != nil || !info.IsDir() || !privateStateDir(info) {
		return nil, nil, stateError(StateUnusable)
	}
	dir := sessionDir(home, ref.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, nil, stateError(StateUnusable)
	}
	lock, err := lockSession(dir)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.OpenFile(transcriptPath(dir), os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = lock.Close()
		return nil, nil, stateError(StateUnusable)
	}
	t := &transcript{file: file, lock: lock}
	header := record{Type: recordSession, At: time.Now().UTC(), Header: &transcriptHeader{Version: 1, Engine: ref.Engine, ID: ref.ID, ConfigHash: ref.ConfigHash}}
	if err = t.append(header); err != nil {
		t.close()
		return nil, nil, err
	}
	syncDir(dir)
	syncDir(sessions)
	return t, []record{header}, nil
}

// openTranscript locks an existing session and reads its records back. A
// record cut short by a crash is dropped: every record is written whole and
// synced before anything that depends on it happens, so a partial one is a
// write that had not yet taken effect.
func openTranscript(home string, ref Ref) (*transcript, []record, error) {
	dir := sessionDir(home, ref.ID)
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil, stateError(StateMissing)
	case err != nil || !info.IsDir():
		return nil, nil, stateError(StateUnusable)
	}
	lock, err := lockSession(dir)
	if err != nil {
		return nil, nil, err
	}
	fail := func(code string) (*transcript, []record, error) {
		_ = lock.Close()
		return nil, nil, stateError(code)
	}
	path := transcriptPath(dir)
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fail(StateMissing)
	case err != nil:
		return fail(StateUnusable)
	}
	complete := bytes.LastIndexByte(data, '\n') + 1
	records, ok := parseRecords(data[:complete], ref)
	if !ok {
		return fail(StateCorrupt)
	}
	if complete < len(data) {
		if err = os.Truncate(path, int64(complete)); err != nil {
			return fail(StateUnwritable)
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail(StateUnusable)
	}
	return &transcript{file: file, lock: lock}, records, nil
}

func parseRecords(data []byte, ref Ref) ([]record, bool) {
	var records []record
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r record
		if json.Unmarshal(line, &r) != nil || r.Type == "" {
			return nil, false
		}
		records = append(records, r)
	}
	if len(records) == 0 || records[0].Type != recordSession || records[0].Header == nil {
		return nil, false
	}
	h := records[0].Header
	if h.Version != 1 || h.Engine != ref.Engine || h.ID != ref.ID || h.ConfigHash != ref.ConfigHash {
		return nil, false
	}
	return records, true
}

// conversation is the history a request carries: the instructions as the
// leading system message, then every user input and response in order. Chat
// Completions requires each call's tool message directly after the response
// that made it, so every call is answered there: with its recorded result, or,
// where there is none, as not run or of unknown outcome.
func conversation(records []record, system string) []completion.Message {
	results, started := callStates(records)
	var out []completion.Message
	if system != "" {
		out = append(out, completion.Message{Role: "system", Content: system})
	}
	for _, r := range records {
		switch r.Type {
		case recordUser:
			out = append(out, completion.Message{Role: "user", Content: r.Text})
		case recordAssistant:
			out = append(out, completion.Message{Role: "assistant", Content: r.Text, ToolCalls: r.Calls, Replay: r.Replay})
			for _, call := range r.Calls {
				key := record{Response: r.Response, Call: call.ID}.key()
				text := notRunText
				switch result, ok := results[key]; {
				case ok:
					text = result.Text
				case started[key]:
					text = unknownOutcomeText
				}
				out = append(out, completion.Message{Role: "tool", ToolCallID: call.ID, Content: text})
			}
		}
	}
	return out
}

func callStates(records []record) (map[string]record, map[string]bool) {
	results, started := map[string]record{}, map[string]bool{}
	for _, r := range records {
		switch r.Type {
		case recordToolCall:
			started[r.key()] = true
		case recordToolResult:
			results[r.key()] = r
		}
	}
	return results, started
}

// recoverTranscript finds what the last process left unfinished and returns
// the records that settle it: a started call with no result is answered as
// having an unknown outcome, never re-run; a call that never started, as not
// run; and a turn with no end, as interrupted.
func recoverTranscript(records []record) ([]record, Recovery) {
	results, started := callStates(records)
	var additions []record
	var recovery Recovery
	ended := map[string]bool{}
	open := ""
	for _, r := range records {
		switch r.Type {
		case recordTurnStart:
			open = r.Turn
		case recordTurnEnd:
			ended[r.Turn] = true
		case recordAssistant:
			for _, call := range r.Calls {
				key := record{Response: r.Response, Call: call.ID}.key()
				if _, ok := results[key]; ok {
					continue
				}
				result := record{Type: recordToolResult, Turn: r.Turn, Response: r.Response, Call: call.ID, Tool: call.Function.Name, IsError: true, Outcome: outcomeNotRun, Text: notRunText}
				if started[key] {
					result.Outcome, result.Text = outcomeUnknown, unknownOutcomeText
					recovery.UnknownOutcomes = append(recovery.UnknownOutcomes, RecoveredCall{ID: call.ID, Tool: call.Function.Name})
				}
				additions = append(additions, result)
			}
		}
	}
	if open != "" && !ended[open] {
		recovery.TurnID = open
		additions = append(additions, record{Type: recordTurnEnd, Turn: open, Status: "interrupted", Code: "session_interrupted"})
	}
	return additions, recovery
}

func countResponses(records []record) int {
	n := 0
	for _, r := range records {
		if r.Type == recordAssistant && r.Response > n {
			n = r.Response
		}
	}
	return n
}

// conversationBegun reports whether any turn carried input, which is when a
// caller's "started" context was delivered.
func conversationBegun(records []record) bool {
	for _, r := range records {
		if r.Type == recordUser {
			return true
		}
	}
	return false
}

func sessionLockPath(dir string) string { return filepath.Join(dir, "session.lock") }
