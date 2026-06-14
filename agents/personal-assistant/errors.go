package personalassistant

// MissingEnvError is returned by New when a required environment variable is
// unset. In v1 the only required variable is LLM_API_KEY, and only when the
// selected model's provider requires a key.
type MissingEnvError struct{ Var string }

func (e *MissingEnvError) Error() string {
	return "personalassistant: required environment variable " + e.Var + " is not set"
}

// EmptyInputError is returned by Send and Stream when the user text is empty or
// whitespace only.
type EmptyInputError struct{}

func (e *EmptyInputError) Error() string {
	return "personalassistant: input text is empty"
}
