package session

// The MCP protocol the tool channel speaks to a harness through its bridge.
// Nothing here decides whether a call runs; every tools/call goes through
// admission first.

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// serve reads the bridge's authentication line, then the protocol stream. A
// connection that cannot prove it holds the channel credential is dropped
// without a reply.
func (h *toolHost) serve(conn net.Conn) {
	h.mu.Lock()
	h.connections++
	connection := h.connections
	h.mu.Unlock()
	go func() {
		<-h.done
		_ = conn.Close()
	}()
	reader := bufio.NewReaderSize(conn, 4096)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), MaxToolRequestBytes)
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return
	}
	if !scanner.Scan() {
		return
	}
	var hello struct {
		Secret string `json:"secret"`
	}
	if json.Unmarshal(scanner.Bytes(), &hello) != nil {
		return
	}
	if subtle.ConstantTimeCompare([]byte(hello.Secret), h.secret) != 1 {
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	writeMu := sync.Mutex{}
	reply := func(v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			return
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = conn.Write(append(raw, '\n'))
	}
	var pending sync.WaitGroup
	defer pending.Wait()
	for scanner.Scan() {
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			reply(rpcError(nil, -32700, "parse error"))
			return
		}
		if len(frame.ID) == 0 || string(frame.ID) == "null" {
			// A notification needs no reply, but it is not therefore uninteresting:
			// a harness cancels a tool call this way, and ignoring it leaves work
			// running after the turn that asked for it has been interrupted.
			if frame.Method == "notifications/cancelled" {
				h.cancelRequested(connection, frame.Params)
			}
			continue
		}
		switch frame.Method {
		case "initialize":
			reply(rpcResult(frame.ID, h.initialize(frame.Params)))
		case "ping":
			reply(rpcResult(frame.ID, map[string]any{}))
		case "tools/list":
			h.mu.Lock()
			h.listed = true
			h.mu.Unlock()
			reply(rpcResult(frame.ID, h.list()))
		case "tools/call":
			// Admitted here, in the order the harness sent it, and only then
			// dispatched on its own goroutine so the connection keeps reading.
			// Registering inside the goroutine would let a cancellation that
			// arrives in the very next frame find nothing to cancel, and the call
			// it named would run anyway.
			//
			// The turn is sampled now, when the harness asked, rather than when
			// the handler eventually runs. A call that waited behind another one
			// belongs to the turn that requested it, and labelling it with a later
			// turn would attribute work to the wrong piece of the conversation.
			id, params := frame.ID, frame.Params
			turn := ""
			if h.activeTurn != nil {
				turn = h.activeTurn()
			}
			ready, refusal := h.prepare(callKey(connection, string(id)), params)
			if refusal != nil {
				reply(rpcResult(id, refusal))
				continue
			}
			pending.Add(1)
			go func() {
				defer pending.Done()
				reply(rpcResult(id, h.execute(ready, turn)))
			}()
		default:
			// Every other method, including the resource methods a harness's own
			// MCP helpers use, is refused. A restricted session hosts tools and
			// nothing else, so those helpers have nothing here to enumerate or
			// read even on a harness that offers them.
			reply(rpcError(frame.ID, -32601, "method not supported"))
		}
	}
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	out := map[string]any{"jsonrpc": "2.0", "error": map[string]any{"code": code, "message": message}}
	if len(id) > 0 {
		out["id"] = id
	}
	return out
}

// initialize echoes a protocol version the harness asked for when it is one we
// recognize, so an older or newer installed CLI still negotiates successfully.
func (h *toolHost) initialize(params json.RawMessage) map[string]any {
	version := "2025-06-18"
	var in struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &in) == nil {
		switch in.ProtocolVersion {
		case "2024-11-05", "2025-03-26", "2025-06-18":
			version = in.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": h.cfg.Server, "version": "1"},
	}
}

func (h *toolHost) list() map[string]any {
	tools := make([]map[string]any, 0, len(h.cfg.Tools))
	for _, t := range h.cfg.Tools {
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
	}
	return map[string]any{"tools": tools}
}

func toolPayload(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

// cancelRequested stops a call the harness has withdrawn. Request identifiers
// are per connection, so the key includes the connection that sent it —
// otherwise one harness's identifier would cancel another's call.
func (h *toolHost) cancelRequested(connection uint64, params json.RawMessage) {
	var in struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &in) != nil {
		return
	}
	h.mu.Lock()
	call := h.pending[callKey(connection, string(in.RequestID))]
	h.mu.Unlock()
	if call != nil {
		call.cancel()
	}
}

func callKey(connection uint64, request string) string {
	return strconv.FormatUint(connection, 10) + ":" + request
}

func quoteName(name string) string {
	name = bound(name, 64)
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range name {
		if r < 0x20 || r == '"' || r == '\\' || r == 0x7f {
			out.WriteByte('?')
			continue
		}
		out.WriteRune(r)
	}
	out.WriteByte('"')
	return out.String()
}

// bound truncates on a rune boundary and says so. Silent truncation would let a
// tool result look complete when it is not.
func bound(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end] + "\n[truncated: " + strconv.Itoa(len(text)-end) + " further bytes omitted]"
}
