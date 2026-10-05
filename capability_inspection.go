package harness

// ProcessInspection requests inspection of a command's own launched tree.
const ProcessInspection Feature = "process_inspection"

const (
	ProcessInspectionSeatbeltReason  = "process_inspection_unenforceable: a same-sandbox process-info and sysctl boundary has not been proved on macOS; inspection is refused"
	ProcessInspectionPlatformReason  = "process inspection is unavailable on this platform"
	ProcessInspectionWindowsReason   = "process inspection is unavailable on Windows; restricted command hosting is not offered"
	ProcessInspectionNativeReason    = "process inspection is offered only by the shared command sandbox, not native engines or runs"
	ProcessInspectionNamespaceReason = "proved per Open in each command's private PID namespace; separate Run and Start trees are invisible"
)

func processInspectionSupport(e Engine, op Operation, goos string) Capability {
	if e.Transport() == "" {
		return Capability{Unsupported, "unrecognized engine"}
	}
	if e != OpenAICompatible || op != Session {
		return Capability{Unsupported, ProcessInspectionNativeReason}
	}
	switch goos {
	case "linux":
		return Capability{Unknown, ProcessInspectionNamespaceReason}
	case "darwin":
		return Capability{Unsupported, ProcessInspectionSeatbeltReason}
	case "windows":
		return Capability{Unsupported, ProcessInspectionWindowsReason}
	default:
		return Capability{Unsupported, ProcessInspectionPlatformReason}
	}
}
