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

// SnapshotErrorReason classifies why a Snapshot could not return a consistent view
// of the foreign loop's committed state (mirrors loop.SnapshotErrorReason).
type SnapshotErrorReason string

const (
	// SnapshotLoopExited means the actor goroutine has exited (Loop.Done closed), so
	// there is no live state to read.
	SnapshotLoopExited SnapshotErrorReason = "loop_exited"
	// SnapshotContextDone means the caller's context was cancelled before the actor
	// replied.
	SnapshotContextDone SnapshotErrorReason = "context_done"
)

// SnapshotError is returned by (*Loop).Snapshot when it cannot obtain a consistent
// view of the loop's committed state. Cause chains the underlying ctx error when
// present. It mirrors loop.SnapshotError's shape so a caller depending on
// loop.Backend gets the same typed failure surface from either engine.
type SnapshotError struct {
	Reason SnapshotErrorReason
	Cause  error
}

func (e *SnapshotError) Error() string {
	switch e.Reason {
	case SnapshotLoopExited:
		return "foreignloop: snapshot failed: loop exited"
	case SnapshotContextDone:
		if e.Cause != nil {
			return "foreignloop: snapshot failed: context done: " + e.Cause.Error()
		}
		return "foreignloop: snapshot failed: context done"
	default:
		return "foreignloop: snapshot failed"
	}
}

func (e *SnapshotError) Unwrap() error { return e.Cause }
