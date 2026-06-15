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
