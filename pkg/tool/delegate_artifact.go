package tool

// DelegateRuntime is the dependency-safe representation of a resolved child
// runtime. The delegation package converts the loop catalog's named types into
// these stable aliases at the preparation boundary; pkg/tool deliberately does
// not import pkg/loop.
type DelegateRuntime struct {
	Harness    string
	Profile    string
	Model      string
	SmallModel string
	Effort     string
	Explicit   DelegateRuntimeExplicit
}

// DelegateRuntimeExplicit records which selectors were supplied by the caller.
// Defaults are resolved into the same concrete runtime fields, but remain
// distinguishable for downstream pinning and audit decisions.
type DelegateRuntimeExplicit struct {
	Harness bool
	Model   bool
	Effort  bool
}

// DelegateArtifact is the prepared, fully validated Subagent call. It is
// created once in PrepareCall and consumed once in execution.
type DelegateArtifact struct {
	Request DelegateRequest
	Runtime *DelegateRuntime
}

func (DelegateArtifact) preparedArtifact() {}
