package llm

// Capabilities is secret-free gating/informational data about a model: never serialized onto
// the wire, read locally (e.g. a TUI deciding whether to allow image attachments).
type Capabilities struct {
	AcceptsImages bool
	MaxContext    int
	Tools         bool
	Thinking      bool
}
