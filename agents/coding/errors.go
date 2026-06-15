package coding

// MissingEnvError is returned by New when a required environment variable is
// unset. In v1 the only required variable is LLM_API_KEY, and only when the
// selected model's provider requires a key.
type MissingEnvError struct{ Var string }

func (e *MissingEnvError) Error() string {
	return "coding: required environment variable " + e.Var + " is not set"
}

// EmptyInputError is returned by Send and Stream when the user text is empty or
// whitespace only.
type EmptyInputError struct{}

func (e *EmptyInputError) Error() string {
	return "coding: input text is empty"
}

// WorkspaceRootError is returned during construction when the workspace root
// (the process working directory) cannot be resolved, so the file tools cannot
// be confined to a known root. Cause carries the underlying os.Getwd error.
type WorkspaceRootError struct{ Cause error }

func (e *WorkspaceRootError) Error() string {
	if e.Cause == nil {
		return "coding: cannot resolve workspace root"
	}
	return "coding: cannot resolve workspace root: " + e.Cause.Error()
}

func (e *WorkspaceRootError) Unwrap() error { return e.Cause }

// SubagentTurnError is returned by a child Subsession.Invoke when the child's
// turn ended in a non-cancellation failure (event.TurnFailed) or an
// unrecognized terminal event. Cause carries the child's typed failure cause
// when one is available. The Subagent tool turns this into a tool-result error
// string; it is never a secret (the cause is a typed, secret-free engine error).
type SubagentTurnError struct{ Cause error }

func (e *SubagentTurnError) Error() string {
	if e.Cause == nil {
		return "coding: subagent turn did not complete"
	}
	return "coding: subagent turn failed: " + e.Cause.Error()
}

func (e *SubagentTurnError) Unwrap() error { return e.Cause }

// SubagentInterruptedError is returned by a child Subsession.Invoke when the
// child's turn was interrupted (its context was cancelled) before completing.
type SubagentInterruptedError struct{}

func (e *SubagentInterruptedError) Error() string {
	return "coding: subagent turn was interrupted"
}
