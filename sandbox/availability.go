package sandbox

import "runtime"

// FileToolsDisabledReason explains the temporary content-tool containment policy.
const FileToolsDisabledReason = "workbench file tools are off until their workspace check is verified"

// ToolAvailability returns the temporary containment refusal reason for content
// tools. Empty means this policy does not disable the name; callers must still
// authorize it and verify its other platform requirements.
func ToolAvailability(name string) string {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		switch name {
		case workbenchReadFile, workbenchSearchFiles, workbenchEditFile:
			return FileToolsDisabledReason
		}
	}
	return ""
}
func contentRefusal(tool string) (Result, error) {
	return ToolError(tool, RefusedNotOffered, ""), refusal(tool, RefusedNotOffered, FileToolsDisabledReason)
}
