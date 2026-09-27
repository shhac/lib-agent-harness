package completion

import "github.com/shhac/lib-agent-harness"

// Tools is a synthetic application catalog, with no execution attached.
func Tools() []Tool {
	return []Tool{{Type: "function", Function: Function{Name: "read_state", Parameters: map[string]any{"type": "object"}}}}
}

func cliProvider(engine harness.Engine, binary, home string) harness.Provider {
	return harness.Provider{Engine: engine, CLI: harness.CLI{Binary: binary, Home: home}}
}

// cliEngines are the engines whose completion runs an installed CLI.
var cliEngines = []harness.Engine{harness.Codex, harness.Claude}

// messageAndUsage lets a test read the two parts of a Result it asserts on.
func messageAndUsage(result Result, err error) (Message, harness.Usage, error) {
	return result.Message, result.Usage, err
}
