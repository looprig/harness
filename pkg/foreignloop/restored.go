package foreignloop

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// RestoredForeign is the journal-recovered seed for a foreign loop: the recovered
// foreign session id, the committed turn count, and the committed conversation thread.
// A restored loop comes up idle, seeded with this state, and resumes (never re-creates)
// the recorded session on its next turn.
type RestoredForeign struct {
	ForeignSID string
	TurnIndex  event.TurnIndex
	Msgs       content.AgenticMessages
}

// NewRestored is the restore counterpart to New: it constructs a foreign loop SEEDED
// with recovered committed state and starts its actor goroutine IDLE. The foreign
// session id is RECOVERED from the seed (never minted), and hasSpawned starts true so
// the next turn --resumes the existing session rather than starting (and orphaning) a
// new one. It applies the same fail-secure wiring validation as New and ADDITIONALLY
// requires a non-empty seed sid (a restored foreign loop must know its session). There
// is no sid return value — the caller already holds it in the seed.
func NewRestored(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance,
	pub EventPublisher, cfg loop.Config, spec Spec, idGen func() (uuid.UUID, error),
	fac *event.Factory, seed RestoredForeign) (*Loop, error) {
	if err := validateWiring(cfg, spec, idGen, fac, pub); err != nil {
		return nil, err
	}
	if seed.ForeignSID == "" {
		return nil, &ConfigError{Field: "RestoredForeign.ForeignSID", Reason: "required"}
	}
	l := &Loop{
		Commands:   make(chan command.Command),
		Done:       make(chan struct{}),
		snapshots:  make(chan snapshotReq),
		sessionID:  sessionID,
		loopID:     loopID,
		sid:        seed.ForeignSID,
		parent:     parent,
		pub:        pub,
		cfg:        cfg,
		spec:       spec,
		idGen:      idGen,
		fac:        fac,
		msgs:       cloneMessages(seed.Msgs),
		turnIndex:  seed.TurnIndex,
		hasSpawned: true,
		sidBound:   true,
	}
	go l.run(loopCtx)
	return l, nil
}

// RestoredBuilder is the composition-root seam Session uses to RECONSTRUCT a foreign
// loop from journal-recovered state. It mirrors Builder but carries the RestoredForeign
// seed and returns no sid (the seed already holds it).
type RestoredBuilder func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance,
	pub EventPublisher, cfg loop.Config, idGen func() (uuid.UUID, error), fac *event.Factory,
	seed RestoredForeign) (loop.Backend, error)

// BuildRestoredWith adapts NewRestored to the RestoredBuilder seam: it closes over the
// per-agent Spec resolved at the root and returns a closure that reconstructs the
// foreign loop as a loop.Backend. On a construction error it returns a NIL Backend
// (never a non-nil interface wrapping a nil *Loop) so the caller's nil check behaves.
func BuildRestoredWith(spec Spec) RestoredBuilder {
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance,
		pub EventPublisher, cfg loop.Config, idGen func() (uuid.UUID, error), fac *event.Factory,
		seed RestoredForeign) (loop.Backend, error) {
		l, err := NewRestored(loopCtx, sessionID, loopID, parent, pub, cfg, spec, idGen, fac, seed)
		if err != nil {
			return nil, err
		}
		return l, nil
	}
}
