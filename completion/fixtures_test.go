package completion

// Tools is a synthetic application catalog, with no execution attached.
func Tools() []Tool {
	return []Tool{{Type: "function", Function: Function{Name: "read_state", Parameters: map[string]any{"type": "object"}}}}
}
