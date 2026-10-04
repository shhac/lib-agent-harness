package harness

// Descriptor is what an application needs to present an engine and find its
// CLI without knowing the engine: how to name it, how it is reached, and where
// its CLI and home are by default. What the engine can do is Support's job.
type Descriptor struct {
	Engine Engine
	// Label is the engine's name for people, such as "Command Code".
	Label     string
	Transport Transport
	// Binary is the CLI run when Provider.CLI.Binary is empty; empty for an
	// API engine.
	Binary string
	// HomeVariable is the environment variable that selects the CLI's home,
	// empty when the CLI has none and finds its home itself.
	HomeVariable string
	// HomeDir is the CLI's default home, relative to the user's home
	// directory, used when HomeVariable is unset; empty for an API engine.
	HomeDir string
}

var descriptors = map[Engine]Descriptor{
	Codex:            {Engine: Codex, Label: "Codex", Transport: CLITransport, Binary: "codex", HomeVariable: "CODEX_HOME", HomeDir: ".codex"},
	Claude:           {Engine: Claude, Label: "Claude", Transport: CLITransport, Binary: "claude", HomeVariable: "CLAUDE_CONFIG_DIR", HomeDir: ".claude"},
	Grok:             {Engine: Grok, Label: "Grok", Transport: CLITransport, Binary: "grok", HomeVariable: "GROK_HOME", HomeDir: ".grok"},
	CommandCode:      {Engine: CommandCode, Label: "Command Code", Transport: CLITransport, Binary: "cmd", HomeDir: ".commandcode"},
	OpenAICompatible: {Engine: OpenAICompatible, Label: "OpenAI-compatible API", Transport: APITransport},
}

// Describe is e's descriptor, and false for an engine the library does not
// know.
func Describe(e Engine) (Descriptor, bool) {
	d, ok := descriptors[e]
	return d, ok
}
