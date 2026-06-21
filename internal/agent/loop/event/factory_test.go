package event_test

import (
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestFactoryStamp asserts NewHeader mints a fresh EventID from the injected
// IDGen and a CreatedAt from the injected Clock, leaving Coordinates/Cause zero
// for the caller to fill. The generators are fixed so the result is
// deterministic (mirrors session's injected idGenerator seam).
func TestFactoryStamp(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	// Two distinct fixed uuids constructed as byte arrays (internal/uuid has no
	// MustParse; tests build fixed UUIDs directly, see uuid_test.go).
	ids := []uuid.UUID{{1}, {2}}
	i := 0
	f := event.NewFactory(func() uuid.UUID { id := ids[i]; i++; return id }, func() time.Time { return ts })

	h := f.NewHeader() // mints EventID + CreatedAt
	if h.EventID != ids[0] || !h.CreatedAt.Equal(ts) {
		t.Fatalf("got %+v", h)
	}

	// A second mint draws the next id and the same clock — proves the IDGen is
	// called per header, not cached.
	h2 := f.NewHeader()
	if h2.EventID != ids[1] || !h2.CreatedAt.Equal(ts) {
		t.Fatalf("second NewHeader got %+v, want EventID %v", h2, ids[1])
	}
}
