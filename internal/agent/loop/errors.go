package loop

type ConfigErrorKind string

const (
	ConfigMissingClient    ConfigErrorKind = "missing_client"
	ConfigInvalidModel     ConfigErrorKind = "invalid_model"
	ConfigMissingPublisher ConfigErrorKind = "missing_publisher"
)

// ConfigError is returned by New when the supplied Config is invalid.
type ConfigError struct {
	Kind  ConfigErrorKind
	Cause error
}

func (e *ConfigError) Error() string {
	switch e.Kind {
	case ConfigMissingClient:
		return "loop: config error: Config.Client is required"
	case ConfigInvalidModel:
		return "loop: config error: Config.Model invalid"
	case ConfigMissingPublisher:
		return "loop: config error: event publisher is required"
	default:
		return "loop: config error"
	}
}
func (e *ConfigError) Unwrap() error { return e.Cause }

// IDGenerationError is sent on StartTurn.Ack when the actor cannot mint a
// TurnID from crypto/rand while accepting the turn. The turn is not started.
type IDGenerationError struct{ Cause error }

func (e *IDGenerationError) Error() string {
	if e.Cause == nil {
		return "loop: turn id generation failed"
	}
	return "loop: turn id generation failed: " + e.Cause.Error()
}
func (e *IDGenerationError) Unwrap() error { return e.Cause }

// CommitCancelReason distinguishes why a per-step commit handshake did not reach
// the actor's commit point.
type CommitCancelReason string

const (
	// CommitTurnCancelled means the turn context was cancelled (Interrupt/Shutdown)
	// before the actor committed the step. runTurn returns a TurnInterrupted and the
	// in-flight step is discarded; committed steps stay committed.
	CommitTurnCancelled CommitCancelReason = "turn cancelled"
)

// CommitError is returned by turnConfig.commit when the ctx-cancellable commit
// handshake cannot deliver a completed step to the actor. The turn goroutine stops
// and surfaces a terminal without wedging; the already-committed steps remain in
// loopState.msgs. Callers MAY errors.As to distinguish cancel reasons; today the
// only reason is CommitTurnCancelled and callers treat any commit error as an
// interrupt. Reason is reserved for a later phase (e.g. Shutdown-vs-Interrupt).
type CommitError struct {
	Reason CommitCancelReason
	Cause  error
}

func (e *CommitError) Error() string {
	if e.Cause != nil {
		return "loop: commit handshake: " + string(e.Reason) + ": " + e.Cause.Error()
	}
	return "loop: commit handshake: " + string(e.Reason)
}
func (e *CommitError) Unwrap() error { return e.Cause }
