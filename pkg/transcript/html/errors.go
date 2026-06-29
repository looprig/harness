package html

// RenderError is returned by Render when the transcript cannot be written as
// HTML. It wraps any failure along the render path — markdown rendering
// (goldmark), template execution, or the write to the destination io.Writer;
// Cause is the wrapped underlying error, recoverable via errors.As / Unwrap.
// Reconstruction anomalies never reach the renderer as errors — those are
// surfaced as transcript.Warning on the model and rendered into the document's
// distinct reconstruction-notes section by the full layout.
type RenderError struct {
	Cause error
}

func (e *RenderError) Error() string {
	if e.Cause == nil {
		return "transcript/html: render"
	}
	return "transcript/html: render: " + e.Cause.Error()
}

func (e *RenderError) Unwrap() error { return e.Cause }
