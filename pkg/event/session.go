package event

// SessionResidencyReleased records that ONE process gave up its resident runtime for
// the session — subscriptions, actors, processes, leases, local contexts — while
// leaving the logical session restorable elsewhere.
//
// It is NOT SessionStopped and must never be folded as if it were. SessionStopped is
// the terminal transition a Shutdown appends; this one is nonterminal, so a projection
// that reads it learns the session is cold (no process is resident) and restorable,
// not that it ended. The distinction is carried by the concrete TYPE: both are
// Enduring, session-scoped and Public, so a fold that switched on anything coarser
// than the type would read a release as an end.
//
// CheckpointSeq and LeaseEpoch are what a successor needs before it restores:
// which durable checkpoint this residency's work is anchored to, and which
// single-writer epoch produced it.
type SessionResidencyReleased struct {
	enduring
	sessionScoped
	Header
	// CheckpointSeq is the journal sequence of the WorkspaceCheckpointed this release
	// is anchored to. Zero when the releasing session had no managed workspace to
	// checkpoint, so a reader must treat zero as "no workspace anchor", never as
	// "sequence 0".
	CheckpointSeq uint64 `json:"checkpoint_seq,omitempty"`
	// LeaseEpoch is the single-writer lease epoch the releasing process held. Zero
	// when the session was not wired to a lease that reports an epoch.
	LeaseEpoch uint64 `json:"lease_epoch,omitempty"`
}

func (SessionResidencyReleased) isEvent() {}
