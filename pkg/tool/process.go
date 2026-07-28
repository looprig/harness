package tool

import (
	"context"
	"io"
	"time"

	"github.com/looprig/core/uuid"
)

// AsyncProcessRunner prepares enforcement for an asynchronous process without
// spawning it. A successful preparation reserves any enforcement resources
// needed for the process lifetime.
type AsyncProcessRunner interface {
	PrepareProcess(context.Context, ProcessRequest) (PreparedProcess, error)
}

// PreparedProcess is a validated, reserved process start. Its effective
// workspace access is authoritative and immutable. Start consumes the
// preparation at most once; Close releases an unstarted preparation.
// EffectiveWorkspaceAccess returns a deep value that shares no mutable backing
// storage with the preparation or with values returned by earlier calls.
//
// The Start context governs setup through process handoff only. Once Start
// returns a Process, that process lives until Wait, Close, its deadline, or
// runner shutdown independently of the Start context.
type PreparedProcess interface {
	EffectiveWorkspaceAccess() WorkspaceAccess
	Start(context.Context) (Process, error)
	Close() error
}

// Process is a running asynchronous process. Streams are available immediately
// after Start. StreamMode reports a valid, unambiguous pipe or PTY shape and
// must match the admitted ProcessRequest; implementations must never silently
// fall back to a different stream mode.
//
// Methods other than Wait are safe to call concurrently with each other and
// with stream I/O. Signal targets the complete process tree. Close is
// idempotent. The supervising owner is the sole Wait caller and calls it
// exactly once; Wait confirms process-tree exit before returning.
//
// The returned stdin supports concurrent Write and Close calls. Closing it is
// idempotent, delivers EOF to the process at most once, and causes later writes
// to fail.
//
// Process deliberately exposes no OS process identifier. A model-facing
// process handle belongs in the supervising tool layer, not this runner layer.
type Process interface {
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Stdin() io.WriteCloser
	StreamMode() ProcessStreamMode
	Wait(context.Context) (ProcessResult, error)
	Resize(context.Context, uint16, uint16) error
	Signal(context.Context, ProcessSignal) error
	Close(context.Context) error
}

// ProcessStreamMode describes the running process's stream topology.
type ProcessStreamMode uint8

const (
	// ProcessStreamModePipes exposes distinct non-nil Stdout and Stderr pipe
	// readers.
	ProcessStreamModePipes ProcessStreamMode = iota + 1
	// ProcessStreamModePTY exposes combined terminal bytes through Stdout.
	// Stderr remains non-nil but is closed and empty.
	ProcessStreamModePTY
)

// Valid reports whether m is a recognized process stream mode.
func (m ProcessStreamMode) Valid() bool {
	return m >= ProcessStreamModePipes && m <= ProcessStreamModePTY
}

// ProcessActivitySource is an optional capability implemented by a Process
// that can report workspace activity. The activity channel must close before
// Process.Wait returns.
type ProcessActivitySource interface {
	Activities() <-chan ProcessActivity
}

// ProcessRequest describes one shell-agnostic asynchronous process admission.
// Grants are opaque, execution-bound tokens. Deadline is the process lifetime
// deadline. A zero Deadline means no process deadline and must never be
// replaced by a runner default; session or runner shutdown still terminates the
// process.
type ProcessRequest struct {
	Command           string
	Directory         string
	Grants            []string
	OriginExecutionID uuid.UUID
	Deadline          time.Time
	PTY               bool
}

// HasDeadline reports whether the request defines a process lifetime deadline.
func (r ProcessRequest) HasDeadline() bool {
	return !r.Deadline.IsZero()
}

// Clone returns a deep copy sharing no slice backing storage with the receiver.
func (r ProcessRequest) Clone() ProcessRequest {
	out := r
	out.Grants = cloneStrings(r.Grants)
	return out
}

// WorkspaceAccessKind classifies authoritative process workspace access.
type WorkspaceAccessKind uint8

const (
	// WorkspaceAccessReadOnly may coexist with readers and structured writes.
	WorkspaceAccessReadOnly WorkspaceAccessKind = iota + 1
	// WorkspaceAccessScopedWrite writes only the canonical paths and trees
	// declared by WorkspaceAccess.
	WorkspaceAccessScopedWrite
	// WorkspaceAccessBroadWrite may write anywhere in the bound workspace.
	WorkspaceAccessBroadWrite
)

// Valid reports whether k is a recognized workspace access classification.
func (k WorkspaceAccessKind) Valid() bool {
	return k >= WorkspaceAccessReadOnly && k <= WorkspaceAccessBroadWrite
}

// WorkspaceAccess is the runner's authoritative, immutable description of a
// prepared process's workspace access. WritePaths are canonical individual
// paths and WriteTrees are canonical directory trees. They are meaningful only
// for WorkspaceAccessScopedWrite.
type WorkspaceAccess struct {
	Kind       WorkspaceAccessKind
	writePaths []string
	writeTrees []string
}

// NewWorkspaceAccess constructs an access description without retaining the
// caller's path slices.
func NewWorkspaceAccess(
	kind WorkspaceAccessKind,
	writePaths []string,
	writeTrees []string,
) WorkspaceAccess {
	return WorkspaceAccess{
		Kind:       kind,
		writePaths: cloneStrings(writePaths),
		writeTrees: cloneStrings(writeTrees),
	}
}

// WritePaths returns a defensive copy of the canonical individual write paths.
func (a WorkspaceAccess) WritePaths() []string {
	return cloneStrings(a.writePaths)
}

// WriteTrees returns a defensive copy of the canonical write directory trees.
func (a WorkspaceAccess) WriteTrees() []string {
	return cloneStrings(a.writeTrees)
}

// Clone returns a deep copy sharing no slice backing storage with the receiver.
func (a WorkspaceAccess) Clone() WorkspaceAccess {
	return NewWorkspaceAccess(a.Kind, a.writePaths, a.writeTrees)
}

// ProcessSignal is a portable process-tree signal request.
type ProcessSignal uint8

const (
	ProcessSignalInterrupt ProcessSignal = iota + 1
	ProcessSignalTerminate
	ProcessSignalKill
)

// Valid reports whether s is a recognized portable signal.
func (s ProcessSignal) Valid() bool {
	return s >= ProcessSignalInterrupt && s <= ProcessSignalKill
}

// ProcessTerminalReason classifies why a process reached a terminal state.
type ProcessTerminalReason uint8

const (
	ProcessTerminalExited ProcessTerminalReason = iota + 1
	ProcessTerminalTimedOut
	ProcessTerminalInterrupted
	ProcessTerminalTerminated
	ProcessTerminalKilled
	ProcessTerminalRunnerShutdown
)

// Valid reports whether r is a recognized terminal reason.
func (r ProcessTerminalReason) Valid() bool {
	return r >= ProcessTerminalExited && r <= ProcessTerminalRunnerShutdown
}

// ProcessResult is the terminal result of an asynchronous process. ExitCode is
// the portable executable exit status. StartedAt and FinishedAt are runner
// timestamps. OS process identifiers are intentionally excluded.
type ProcessResult struct {
	ExitCode   int
	Reason     ProcessTerminalReason
	StartedAt  time.Time
	FinishedAt time.Time
}

// WorkspaceActivityKind classifies process-reported filesystem activity.
type WorkspaceActivityKind uint8

const (
	// WorkspaceActivityWrite reports filesystem activity within the immutable
	// access reserved by the prepared process.
	WorkspaceActivityWrite WorkspaceActivityKind = iota + 1
	// WorkspaceActivityBroadWrite requests conservative broad invalidation.
	WorkspaceActivityBroadWrite
)

// Valid reports whether k is a recognized workspace activity kind.
func (k WorkspaceActivityKind) Valid() bool {
	return k >= WorkspaceActivityWrite && k <= WorkspaceActivityBroadWrite
}

// ProcessActivity reports workspace activity from a running process. Every
// activity invalidates the complete observation cache bound to that process;
// scoped observation paths are intentionally not represented.
type ProcessActivity struct {
	Kind WorkspaceActivityKind
}

// EffectiveKind returns the conservative activity classification. Invalid
// activity always maps to broad invalidation and can never narrow the immutable
// lifetime workspace lease.
func (a ProcessActivity) EffectiveKind() WorkspaceActivityKind {
	if !a.Kind.Valid() {
		return WorkspaceActivityBroadWrite
	}
	return a.Kind
}

// ProcessErrorCode is a stable asynchronous-runner failure classification.
type ProcessErrorCode string

const (
	ProcessErrorLifetimeEnforcementUnavailable ProcessErrorCode = "lifetime_enforcement_unavailable"
	ProcessErrorSpawnFailed                    ProcessErrorCode = "spawn_failed"
	ProcessErrorSetupFailed                    ProcessErrorCode = "process_setup_failed"
	ProcessErrorPTYUnavailable                 ProcessErrorCode = "pty_unavailable"
	ProcessErrorSignalFailed                   ProcessErrorCode = "signal_failed"
	ProcessErrorWaitFailed                     ProcessErrorCode = "wait_failed"
	ProcessErrorTeardownFailed                 ProcessErrorCode = "teardown_failed"
)

// Valid reports whether c is a recognized asynchronous-runner error code.
func (c ProcessErrorCode) Valid() bool {
	switch c {
	case ProcessErrorLifetimeEnforcementUnavailable,
		ProcessErrorSpawnFailed,
		ProcessErrorSetupFailed,
		ProcessErrorPTYUnavailable,
		ProcessErrorSignalFailed,
		ProcessErrorWaitFailed,
		ProcessErrorTeardownFailed:
		return true
	default:
		return false
	}
}

// ProcessError reports a classified asynchronous-runner failure. Cause carries
// implementation detail for programmatic inspection and trusted logs; callers
// should expose only Code at untrusted boundaries.
type ProcessError struct {
	Code  ProcessErrorCode
	Cause error
}

func (e *ProcessError) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := "tool: process " + string(e.Code)
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

// Unwrap returns the underlying runner failure, if any.
func (e *ProcessError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is matches ProcessError values by stable code.
func (e *ProcessError) Is(target error) bool {
	other, ok := target.(*ProcessError)
	return ok && e != nil && other != nil && e.Code == other.Code
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

var _ error = (*ProcessError)(nil)
