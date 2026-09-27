package native

import (
	"errors"
	"io/fs"
	"os/exec"
	"strconv"

	harness "github.com/shhac/lib-agent-harness"
)

// Codes a RunError carries. A provider problem code from harness.Provider
// Problem is reported as it is, under FailureCapability.
const (
	CodeUnsupportedEngine    = "unsupported_engine"
	CodeOptionForOtherEngine = "option_for_other_engine"
	CodeUnsupportedOption    = "unsupported_option"
	CodeManagedFlagInArgs    = "managed_flag_in_args"
	CodeInvalidSchema        = "invalid_schema"
	CodeStreamMismatch       = "stream_mismatch"
	CodeReportDirUnavailable = "report_dir_unavailable"
	CodeExecutableNotFound   = "executable_not_found"
	CodeProcessStartFailed   = "process_start_failed"
	CodeProcessExited        = "process_exited"
	CodeProcessSignalled     = "process_signalled"
	CodeNoTerminalResult     = "no_terminal_result"
	CodeTurnFailed           = "turn_failed"
	CodeReportUnavailable    = "report_unavailable"
	CodeNoResponse           = "no_response"
	CodeMalformedReport      = "malformed_report"
	CodeProcessFailed        = "process_failed"
)

// RunError is every failure Run, Version and Stream.Report classify. Its text
// and facts come from a fixed vocabulary and never carry provider or harness
// output: the provider's own account of a failed turn is Result.Failure.
type RunError struct {
	Engine   harness.Engine
	Family   harness.Family
	Code     string
	ExitCode *int
	cause    error
}

var runErrorText = map[string]string{
	CodeUnsupportedEngine:    "engine is not supported for native runs",
	CodeOptionForOtherEngine: "options for a different engine were set",
	CodeUnsupportedOption:    "an option this engine cannot honour was set",
	CodeManagedFlagInArgs:    "Args repeats a flag the library manages",
	CodeInvalidSchema:        "the schema is not valid JSON",
	CodeStreamMismatch:       "the stream does not match the invocation",
	CodeReportDirUnavailable: "no private report directory outside the workspace is available",
	CodeExecutableNotFound:   "harness executable not found; install it or set Provider.CLI.Binary",
	CodeProcessStartFailed:   "harness could not be started",
	CodeProcessExited:        "harness exited",
	CodeProcessSignalled:     "harness was terminated by a signal",
	CodeNoTerminalResult:     "harness ended without a terminal result",
	CodeTurnFailed:           "turn failed; see Result.Failure",
	CodeReportUnavailable:    "the report could not be read",
	CodeNoResponse:           "harness ended without a response",
	CodeMalformedReport:      "harness returned a malformed report",
	CodeProcessFailed:        "harness execution failed",
}

func (e *RunError) Error() string {
	text, ok := runErrorText[e.Code]
	if !ok {
		text = "provider configuration refused: " + e.Code
	}
	if e.ExitCode != nil && *e.ExitCode >= 0 {
		text += " with status " + strconv.Itoa(*e.ExitCode)
	}
	engine := string(e.Engine)
	if engine == "" {
		engine = "native"
	}
	return engine + " run: " + text
}

// Unwrap exposes the process error behind a process failure, such as
// exec.ErrNotFound, without putting its text in Error.
func (e *RunError) Unwrap() error { return e.cause }

// HarnessFacts reports the failure in the vocabulary every mode shares. Nothing
// is retryable on its own terms: a refusal repeats, and a native turn may
// already have run tools.
func (e *RunError) HarnessFacts() harness.Facts {
	return harness.Facts{Engine: e.Engine, Operation: harness.Run, Family: e.Family, Code: e.Code, ExitCode: e.ExitCode}
}

// capabilityError drops an engine the library does not know, since the
// caller's spelling of it is not fixed vocabulary.
func capabilityError(engine harness.Engine, code string) *RunError {
	if engine.Transport() == "" {
		engine = ""
	}
	return &RunError{Engine: engine, Family: harness.FailureCapability, Code: code}
}

func turnError(engine harness.Engine, code string) *RunError {
	return &RunError{Engine: engine, Family: harness.FailureTurn, Code: code}
}

// processError classifies what running the harness returned. A caller-supplied
// RunCommand may return anything; what is neither an exit status nor a failure
// to start stays unclassified rather than guessed.
func processError(engine harness.Engine, err error) *RunError {
	out := &RunError{Engine: engine, Family: harness.FailureProcess, Code: CodeProcessFailed, cause: err}
	var status *exec.ExitError
	var execErr *exec.Error
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &status):
		code := status.ExitCode()
		out.ExitCode = &code
		out.Code = CodeProcessExited
		if code < 0 {
			out.Code = CodeProcessSignalled
		}
	case errors.Is(err, exec.ErrNotFound), errors.As(err, &pathErr) && pathErr.Op != "chdir" && errors.Is(err, fs.ErrNotExist):
		// Nothing ran, so this is local setup, as completion reports it.
		out.Family, out.Code = harness.FailurePreflight, CodeExecutableNotFound
	case errors.As(err, &execErr), errors.As(err, &pathErr):
		out.Code = CodeProcessStartFailed
	}
	return out
}
