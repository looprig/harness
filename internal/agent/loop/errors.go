package loop

type ConfigErrorKind string

const (
	ConfigMissingClient ConfigErrorKind = "missing_client"
	ConfigInvalidModel  ConfigErrorKind = "invalid_model"
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
	default:
		return "loop: config error"
	}
}
func (e *ConfigError) Unwrap() error { return e.Cause }

type TurnBusyReason string

const (
	TurnAlreadyRunning  TurnBusyReason = "turn already running"
	SessionShuttingDown TurnBusyReason = "session shutting down"
)

// TurnBusyError is returned on StartTurn.Ack when the loop cannot accept a turn.
type TurnBusyError struct{ Reason TurnBusyReason }

func (e *TurnBusyError) Error() string { return "loop: " + string(e.Reason) }

// EmptyResponseError is the TurnFailed.Err cause when a provider returns a
// successful stream that contains no text or thinking content.
type EmptyResponseError struct{}

func (e *EmptyResponseError) Error() string { return "loop: empty response from provider" }

// TurnPanicError is the TurnFailed.Err cause when the turn goroutine panics.
// Detail is the recovered value rendered as a string; the raw value is not
// retained so no untyped `any` escapes the recovery site.
type TurnPanicError struct{ Detail string }

func (e *TurnPanicError) Error() string { return "loop: panic in turn goroutine: " + e.Detail }

type CommandName string
type CommandField string

const (
	CommandStartTurn CommandName = "StartTurn"

	StartTurnCtx       CommandField = "Ctx"
	StartTurnEvents    CommandField = "Events"
	StartTurnAbandoned CommandField = "Abandoned"
	StartTurnAck       CommandField = "Ack"
)

// LoopTerminatedError is sent on Shutdown.Ack when the loop's root context was
// cancelled before the actor finished cleanup. It wraps the root context error
// so internal callers can errors.As to this type rather than receiving a raw
// context.Canceled or context.DeadlineExceeded.
type LoopTerminatedError struct{ Cause error }

func (e *LoopTerminatedError) Error() string {
	return "loop: terminated by context: " + e.Cause.Error()
}
func (e *LoopTerminatedError) Unwrap() error { return e.Cause }

// InvalidCommandError is returned when an internal caller violates a command contract.
type InvalidCommandError struct {
	Command CommandName
	Field   CommandField
}

func (e *InvalidCommandError) Error() string {
	return "loop: invalid command: " + string(e.Command) + "." + string(e.Field) + " is required"
}
