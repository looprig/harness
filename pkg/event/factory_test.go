package event_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// TestFactoryNewHeader asserts NewHeader mints a fresh EventID from the injected
// IDGen and a CreatedAt from the injected Clock, leaving Coordinates/Cause zero
// for the caller to fill. The generators are fixed so the result is
// deterministic (mirrors session's injected idGenerator seam).
func TestFactoryNewHeader(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	// Two distinct fixed uuids constructed as byte arrays (internal/uuid has no
	// MustParse; tests build fixed UUIDs directly, see uuid_test.go).
	ids := []uuid.UUID{{1}, {2}}
	i := 0
	f := event.NewFactory(func() (uuid.UUID, error) { id := ids[i]; i++; return id, nil }, func() time.Time { return ts })

	h, err := f.NewHeader() // mints EventID + CreatedAt
	if err != nil {
		t.Fatalf("NewHeader: %v", err)
	}
	if h.EventID != ids[0] || !h.CreatedAt.Equal(ts) {
		t.Fatalf("got %+v", h)
	}

	// A second mint draws the next id and the same clock — proves the IDGen is
	// called per header, not cached.
	h2, err := f.NewHeader()
	if err != nil {
		t.Fatalf("second NewHeader: %v", err)
	}
	if h2.EventID != ids[1] || !h2.CreatedAt.Equal(ts) {
		t.Fatalf("second NewHeader got %+v, want EventID %v", h2, ids[1])
	}
}

// TestFactoryStamp asserts Stamp mints a fresh EventID + CreatedAt onto a COPY of
// the supplied Header, preserving the caller's existing Coordinates and Cause (the
// fields the producer set before stamping). The input Header is not mutated.
func TestFactoryStamp(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	id := uuid.UUID{7}
	sessionID := uuid.UUID{10}
	loopID := uuid.UUID{11}
	turnID := uuid.UUID{12}
	commandID := uuid.UUID{13}

	f := event.NewFactory(func() (uuid.UUID, error) { return id, nil }, func() time.Time { return ts })

	in := event.Header{
		Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID},
		Cause:       identity.Cause{CommandID: commandID, Agency: identity.AgencyUser},
	}
	got, err := f.Stamp(in)
	if err != nil {
		t.Fatalf("Stamp: %v", err)
	}
	if got.EventID != id {
		t.Errorf("EventID = %v, want %v", got.EventID, id)
	}
	if !got.CreatedAt.Equal(ts) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, ts)
	}
	// Existing Coordinates/Cause must be preserved on the copy.
	if got.Coordinates != in.Coordinates {
		t.Errorf("Coordinates = %+v, want preserved %+v", got.Coordinates, in.Coordinates)
	}
	if got.Cause != in.Cause {
		t.Errorf("Cause = %+v, want preserved %+v", got.Cause, in.Cause)
	}
	// The input Header must NOT be mutated (Stamp works on a copy).
	if !in.EventID.IsZero() {
		t.Errorf("input EventID = %v, want unchanged (zero)", in.EventID)
	}
	if !in.CreatedAt.IsZero() {
		t.Errorf("input CreatedAt = %v, want unchanged (zero)", in.CreatedAt)
	}
}

// TestFactoryIDGenError asserts a crypto/rand failure from the injected IDGen is
// propagated (never swallowed) by BOTH Stamp and NewHeader, and that no partial
// Header escapes — the returned EventID stays zero on error.
func TestFactoryIDGenError(t *testing.T) {
	t.Parallel()
	genErr := errors.New("rand source exhausted")
	ts := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	f := event.NewFactory(func() (uuid.UUID, error) { return uuid.UUID{}, genErr }, func() time.Time { return ts })

	tests := []struct {
		name string
		call func() (event.Header, error)
	}{
		{name: "NewHeader propagates", call: f.NewHeader},
		{name: "Stamp propagates", call: func() (event.Header, error) {
			return f.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: uuid.UUID{1}}})
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, err := tt.call()
			if !errors.Is(err, genErr) {
				t.Fatalf("err = %v, want it to wrap %v", err, genErr)
			}
			if !h.EventID.IsZero() {
				t.Errorf("EventID = %v on error, want zero (no partial header)", h.EventID)
			}
		})
	}
}
