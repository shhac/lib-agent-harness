package sandboxbridge

// EdgeConfig is internal session/command-boundary plumbing, not a second
// permission model. The sandbox package normalizes its shared Options.
type EdgeConfig struct {
	Work, Runtime, Binary, CLIHome, Socket, Bridge, ToolState, Secret string
	Read                                                              []string
	Env                                                               []string
	// Preferences are the macOS preference domains the CLI reads at start-up,
	// such as Codex's managed configuration. No other domain is readable.
	Preferences []string
	// ProtectRuntime and ProtectWork are an engine's own configuration and
	// instruction loaders, relative to the runtime home and the workspace,
	// which descendants may read but never write. ReadFiles are single public
	// files the engine reads, such as a CA bundle, granted literally.
	ProtectRuntime, ProtectWork, ReadFiles []string
	// Terminals lets the CLI open pseudo-terminals for the commands it runs,
	// reaching only terminals created inside the sandbox.
	Terminals       bool
	Write, Loopback bool
	ProxyPort       int
}
type EdgePlan struct {
	Binary              string
	Args, Env           []string
	Key, PathDiagnostic string
}

var BuildEdge func(EdgeConfig) (EdgePlan, error)
