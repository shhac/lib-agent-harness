package sandboxbridge

// EdgeConfig is internal session/command-boundary plumbing, not a second
// permission model. The sandbox package normalizes its shared Options.
type EdgeConfig struct {
	Work, Runtime, Binary, CLIHome, Socket, Bridge, ToolState, Secret string
	Read                                                              []string
	Env                                                               []string
	// Preferences are the macOS preference domains the CLI reads at start-up,
	// such as Codex's managed configuration. No other domain is readable.
	Preferences     []string
	Write, Loopback bool
	ProxyPort       int
}
type EdgePlan struct {
	Binary              string
	Args, Env           []string
	Key, PathDiagnostic string
}

var BuildEdge func(EdgeConfig) (EdgePlan, error)
