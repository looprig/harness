package event

import (
	"time"

	"github.com/looprig/core/uuid"
)

// Clock and IDGen are injected so tests are deterministic (mirrors session's
// injected idGenerator seam): the Factory mints from these rather than calling
// time.Now/uuid.New directly, so a test can pin both. IDGen returns an error so a
// crypto/rand failure propagates (matching session's idGenerator
// func() (uuid.UUID, error)) rather than being swallowed.
type Clock func() time.Time
type IDGen func() (uuid.UUID, error)

// Factory mints fresh event Headers, stamping each with a new EventID and the
// current CreatedAt at creation time. It is the single creation seam every
// Enduring event flows through so the journal sees a stable idempotency key and
// creation timestamp.
type Factory struct {
	newID IDGen
	now   Clock
}

// NewFactory wires the id generator and clock the Factory mints from.
func NewFactory(newID IDGen, now Clock) *Factory { return &Factory{newID: newID, now: now} }

// Stamp returns a COPY of h with a fresh EventID and CreatedAt, preserving the
// caller's existing Coordinates and Cause (the producer set those before
// stamping). A crypto/rand failure from newID is propagated, never swallowed, and
// no partial Header escapes — the returned Header carries a zero EventID on error.
func (f *Factory) Stamp(h Header) (Header, error) {
	id, err := f.newID()
	if err != nil {
		return Header{}, err
	}
	h.EventID = id
	h.CreatedAt = f.now()
	return h, nil
}

// NewHeader mints a fresh EventID + CreatedAt onto an empty Header. Callers fill
// Coordinates/Cause. It is Stamp of the zero Header.
func (f *Factory) NewHeader() (Header, error) { return f.Stamp(Header{}) }
