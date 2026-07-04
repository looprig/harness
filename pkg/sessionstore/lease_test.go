package sessionstore

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/core/uuid"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// fakeLease is a controllable storekit.Lease for unit-testing the adapter's
// derived Valid() and its passthroughs in isolation from any backend.
type fakeLease struct {
	epoch      uint64
	lost       chan struct{}
	releaseErr error
	released   bool
}

func (f *fakeLease) Epoch() uint64         { return f.epoch }
func (f *fakeLease) Lost() <-chan struct{} { return f.lost }
func (f *fakeLease) Release(context.Context) error {
	f.released = true
	return f.releaseErr
}

// Compile-time proof that the test double honors the storekit.Lease contract it
// stands in for; a drift in storekit.Lease breaks here rather than silently.
var _ storekit.Lease = (*fakeLease)(nil)

// TestSessionLeaseValid covers the Valid() derivation: it is a non-blocking read of
// the wrapped Lost() channel — true while the channel is open, false once it closes.
func TestSessionLeaseValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		closed bool
		want   bool
	}{
		{name: "open lost channel is valid", closed: false, want: true},
		{name: "closed lost channel is invalid", closed: true, want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lost := make(chan struct{})
			if tt.closed {
				close(lost)
			}
			l := &sessionLease{inner: &fakeLease{epoch: 7, lost: lost}, sessionID: uuid.UUID{}}
			if got := l.Valid(); got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSessionLeasePassthrough covers the passthroughs: Epoch, Lost, and Release
// delegate to the wrapped storekit.Lease, and SessionID returns the id the adapter
// carries (storekit.Lease has no session identity of its own).
func TestSessionLeasePassthrough(t *testing.T) {
	t.Parallel()
	id := mustUUID(t, "0123abcd-4567-4890-8abc-def012345678")
	tests := []struct {
		name       string
		releaseErr error
	}{
		{name: "release ok", releaseErr: nil},
		{name: "release surfaces inner error", releaseErr: errors.New("boom")},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lost := make(chan struct{})
			inner := &fakeLease{epoch: 42, lost: lost, releaseErr: tt.releaseErr}
			l := &sessionLease{inner: inner, sessionID: id}

			if got := l.Epoch(); got != 42 {
				t.Errorf("Epoch() = %d, want 42", got)
			}
			if l.Lost() != (<-chan struct{})(lost) {
				t.Error("Lost() did not return the wrapped channel")
			}
			if got := l.SessionID(); got != id {
				t.Errorf("SessionID() = %v, want %v", got, id)
			}
			err := l.Release(context.Background())
			if !errors.Is(err, tt.releaseErr) {
				t.Errorf("Release() err = %v, want %v", err, tt.releaseErr)
			}
			if !inner.released {
				t.Error("Release() did not delegate to the wrapped lease")
			}
		})
	}
}

// TestAcquireLease covers the acquire/release lifecycle against the memstore oracle:
// a grant yields a live, epoch-stamped, id-tagged journal.Lease; Release invalidates
// it and closes Lost; a second acquire while held is refused with a translated
// *journal.LeaseHeldError; and a re-acquire after release advances the epoch.
func TestAcquireLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(t *testing.T, st *Store, id uuid.UUID)
	}{
		{
			name: "grants a live lease",
			run: func(t *testing.T, st *Store, id uuid.UUID) {
				l, err := st.AcquireLease(context.Background(), id)
				if err != nil {
					t.Fatalf("AcquireLease() err = %v", err)
				}
				if !l.Valid() {
					t.Error("Valid() = false on fresh lease, want true")
				}
				if l.Epoch() == 0 {
					t.Error("Epoch() = 0 on fresh lease, want > 0")
				}
				if l.SessionID() != id {
					t.Errorf("SessionID() = %v, want %v", l.SessionID(), id)
				}
				select {
				case <-l.Lost():
					t.Error("Lost() closed on fresh lease")
				default:
				}
			},
		},
		{
			name: "release invalidates and closes Lost",
			run: func(t *testing.T, st *Store, id uuid.UUID) {
				l, err := st.AcquireLease(context.Background(), id)
				if err != nil {
					t.Fatalf("AcquireLease() err = %v", err)
				}
				if err := l.Release(context.Background()); err != nil {
					t.Fatalf("Release() err = %v", err)
				}
				if l.Valid() {
					t.Error("Valid() = true after Release, want false")
				}
				select {
				case <-l.Lost():
				default:
					t.Error("Lost() not closed after Release")
				}
			},
		},
		{
			name: "held session is refused with journal.LeaseHeldError",
			run: func(t *testing.T, st *Store, id uuid.UUID) {
				first, err := st.AcquireLease(context.Background(), id)
				if err != nil {
					t.Fatalf("first AcquireLease() err = %v", err)
				}
				defer func() { _ = first.Release(context.Background()) }()

				_, err = st.AcquireLease(context.Background(), id)
				var held *journal.LeaseHeldError
				if !errors.As(err, &held) {
					t.Fatalf("second AcquireLease() err = %v, want *journal.LeaseHeldError", err)
				}
				if held.SessionID != id {
					t.Errorf("held.SessionID = %v, want %v", held.SessionID, id)
				}
				if held.Epoch != first.Epoch() {
					t.Errorf("held.Epoch = %d, want live holder epoch %d", held.Epoch, first.Epoch())
				}
			},
		},
		{
			name: "re-acquire after release advances the epoch",
			run: func(t *testing.T, st *Store, id uuid.UUID) {
				first, err := st.AcquireLease(context.Background(), id)
				if err != nil {
					t.Fatalf("first AcquireLease() err = %v", err)
				}
				firstEpoch := first.Epoch()
				if err := first.Release(context.Background()); err != nil {
					t.Fatalf("Release() err = %v", err)
				}
				second, err := st.AcquireLease(context.Background(), id)
				if err != nil {
					t.Fatalf("re-acquire err = %v", err)
				}
				if second.Epoch() <= firstEpoch {
					t.Errorf("re-acquired Epoch() = %d, want > %d", second.Epoch(), firstEpoch)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(memstore.New())
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			id, err := uuid.New()
			if err != nil {
				t.Fatalf("uuid.New() err = %v", err)
			}
			tt.run(t, st, id)
		})
	}
}

// _ pins the compile-time contract that a *sessionLease satisfies journal.Lease; a
// drift in either interface breaks the build here rather than at a call site.
var _ journal.Lease = (*sessionLease)(nil)
