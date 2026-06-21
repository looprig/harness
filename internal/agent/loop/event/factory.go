package event

import (
	"time"

	"github.com/inventivepotter/urvi/internal/uuid"
)

// Clock and IDGen are injected so tests are deterministic (mirrors session's
// injected idGenerator seam): the Factory mints from these rather than calling
// time.Now/uuid.New directly, so a test can pin both.
type Clock func() time.Time
type IDGen func() uuid.UUID

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

// NewHeader mints a fresh EventID + CreatedAt. Callers fill Coordinates/Cause.
func (f *Factory) NewHeader() Header { return Header{EventID: f.newID(), CreatedAt: f.now()} }
