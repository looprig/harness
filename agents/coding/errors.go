package coding

// MissingEnvError is returned by New when a required environment variable is
// unset. In v1 the only required variable is LLM_API_KEY, and only when the
// selected model's provider requires a key.
type MissingEnvError struct{ Var string }

func (e *MissingEnvError) Error() string {
	return "coding: required environment variable " + e.Var + " is not set"
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
