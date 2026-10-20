package session

import (
	"encoding/json"
	"testing"
)

// Each way a Claude init frame can fail the start-up surface check, judged
// without a session: a frame that cannot be read, a hosted or browser server
// that did not load, a browser with none of its admitted tools, a tool beside
// the hosted ones, and the surfaces that pass.
func TestJudgeClaudeInit(t *testing.T) {
	host := &ToolHost{Server: "harness", Tools: []ToolDefinition{{Name: "finish"}}}
	connected := func(names ...string) string {
		out := ""
		for i, name := range names {
			if i > 0 {
				out += ","
			}
			out += `{"name":"` + name + `","status":"connected"}`
		}
		return `[` + out + `]`
	}
	for _, c := range []struct {
		name   string
		o      Options
		host   *ToolHost
		frame  string
		judged bool
		code   string
	}{
		{"unreadable", Options{}, host, `{"tools":"Bash"}`, true, CapabilityProbeUnreadable},
		{"hosted server missing", Options{}, host, `{"tools":["mcp__harness__finish"],"mcp_servers":[]}`, true, CapabilityServerNotLoaded},
		{"hosted server failed", Options{}, host, `{"tools":["mcp__harness__finish"],"mcp_servers":[{"name":"harness","status":"failed"}]}`, true, CapabilityServerNotLoaded},
		{"hosted exactly", Options{}, host, `{"tools":["mcp__harness__finish"],"mcp_servers":` + connected("harness") + `}`, true, ""},
		{"a native tool beside the hosted ones", Options{}, host, `{"tools":["mcp__harness__finish","Bash"],"mcp_servers":` + connected("harness") + `}`, true, CapabilityNativeToolsPresent},
		{"browser server missing", Options{Browser: true}, nil, `{"tools":[],"mcp_servers":[]}`, true, CapabilityBrowserToolsMissing},
		{"browser without its tools", Options{Browser: true}, nil, `{"tools":["Bash"],"mcp_servers":` + connected(claudeBrowserServer) + `}`, true, CapabilityBrowserToolsMissing},
		{"ordinary browser session", Options{Browser: true}, nil, `{"tools":["Bash","mcp__` + claudeBrowserServer + `__navigate"],"mcp_servers":` + connected(claudeBrowserServer) + `}`, false, ""},
		{"sandboxed browser with a foreign server", Options{Browser: true, Sandbox: &Sandbox{}}, nil, `{"tools":["Bash","mcp__` + claudeBrowserServer + `__navigate","mcp__other__x"],"mcp_servers":` + connected(claudeBrowserServer) + `}`, true, CapabilityNativeToolsPresent},
		{"sandboxed browser alone", Options{Browser: true, Sandbox: &Sandbox{}}, nil, `{"tools":["Bash","mcp__` + claudeBrowserServer + `__navigate"],"mcp_servers":` + connected(claudeBrowserServer) + `}`, true, ""},
	} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(c.frame), &m); err != nil {
			t.Fatal(err)
		}
		failure, judged := judgeClaudeInit(c.o, c.host, m)
		code := ""
		if failure != nil {
			code = failure.Code
		}
		if judged != c.judged || code != c.code {
			t.Errorf("%s: judged=%v code=%q", c.name, judged, code)
		}
	}
}
