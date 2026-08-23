package tool

// MutationPreview is the display-and-review payload for a pending filesystem
// mutation: what the human is being asked to authorize, rendered by the tool
// that prepared it.
// Its zero value means no preview.
//
// UnifiedDiff is OPAQUE to harness. Harness carries it to the live event and to
// a permission classifier's review context and never parses, trims, or
// interprets it. Bounding is the producing tool's job, because only the tool
// knows where a hunk ends.
//
// A MutationPreview is never journaled and never reaches the agent model. See
// docs/plans/2026-08-23-gate-diff-preview-design.md.
type MutationPreview struct {
	Path        string
	Creates     bool
	UnifiedDiff string
}

// MutationPreviewer is implemented by a prepared artifact that can describe its
// pending change.
//
// The implementation performs I/O and is called ONLY at gate-open, never during
// PrepareCall: a PrepareCall error reaches the model with no gate opening, so
// preview failures raised there would be an ungated oracle over arbitrary host
// files. ok is false for ANY failure, and a false ok must leave the prepared
// request, the tool result, and the model-visible error exactly as they were.
type MutationPreviewer interface {
	MutationPreview() (MutationPreview, bool)
}
