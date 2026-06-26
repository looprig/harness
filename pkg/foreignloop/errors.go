package foreignloop

import "fmt"

type SpawnError struct{ Cause error }

func (e *SpawnError) Error() string { return "foreignloop: spawn: " + e.Cause.Error() }
func (e *SpawnError) Unwrap() error { return e.Cause }

type DecodeError struct{ Cause error }

func (e *DecodeError) Error() string { return "foreignloop: decode: " + e.Cause.Error() }
func (e *DecodeError) Unwrap() error { return e.Cause }

type ForeignExitError struct{ Code int }

func (e *ForeignExitError) Error() string { return fmt.Sprintf("foreignloop: agent exited %d", e.Code) }

// ForeignResultError is the typed cause for a foreign turn that ended with a
// result-level error (e.g. error_max_turns) reported on the stream, as opposed to
// a process exit code (ForeignExitError).
type ForeignResultError struct{ Detail string }

func (e *ForeignResultError) Error() string { return "foreignloop: foreign result error: " + e.Detail }

type TranscriptUnavailableError struct {
	Path  string
	Cause error
}

func (e *TranscriptUnavailableError) Error() string {
	return "foreignloop: transcript unavailable: " + e.Path
}
func (e *TranscriptUnavailableError) Unwrap() error { return e.Cause }

type ForeignSessionBusyError struct {
	SID, Cwd string
	PID      int
}

func (e *ForeignSessionBusyError) Error() string {
	return fmt.Sprintf("foreignloop: session %s busy (pid %d holds %s lock)", e.SID, e.PID, e.Cwd)
}

type ConfigError struct{ Field, Reason string }

func (e *ConfigError) Error() string { return "foreignloop: config: " + e.Field + ": " + e.Reason }
