package sessionstore

import (
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// mustUUID parses the canonical 8-4-4-4-12 form or fails the test. It gives the
// name-derivation tests a fixed, readable id instead of a random one.
func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := id.UnmarshalText([]byte(s)); err != nil {
		t.Fatalf("UnmarshalText(%q) = %v", s, err)
	}
	return id
}

// TestOpen covers the backend-validation boundary: a fully-wired composite opens,
// and a nil composite or any nil primitive field is rejected with a typed
// *InvalidBackendError naming the missing piece (fail closed, no panic).
func TestOpen(t *testing.T) {
	t.Parallel()
	full := memstore.New()
	tests := []struct {
		name     string
		backend  *storage.Composite
		wantErr  bool
		wantMiss string
	}{
		{name: "full composite opens", backend: full, wantErr: false},
		{name: "nil composite rejected", backend: nil, wantErr: true, wantMiss: "composite"},
		{
			name:     "nil ledger rejected",
			backend:  &storage.Composite{Ledger: nil, Leaser: full.Leaser, KV: full.KV, Blobs: full.Blobs},
			wantErr:  true,
			wantMiss: "Ledger",
		},
		{
			name:     "nil leaser rejected",
			backend:  &storage.Composite{Ledger: full.Ledger, Leaser: nil, KV: full.KV, Blobs: full.Blobs},
			wantErr:  true,
			wantMiss: "Leaser",
		},
		{
			name:     "nil kv rejected",
			backend:  &storage.Composite{Ledger: full.Ledger, Leaser: full.Leaser, KV: nil, Blobs: full.Blobs},
			wantErr:  true,
			wantMiss: "KV",
		},
		{
			name:     "nil blobs rejected",
			backend:  &storage.Composite{Ledger: full.Ledger, Leaser: full.Leaser, KV: full.KV, Blobs: nil},
			wantErr:  true,
			wantMiss: "Blobs",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(tt.backend)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Open() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var ibe *InvalidBackendError
				if !errors.As(err, &ibe) {
					t.Fatalf("Open() err = %v, want *InvalidBackendError", err)
				}
				if ibe.Missing != tt.wantMiss {
					t.Errorf("Missing = %q, want %q", ibe.Missing, tt.wantMiss)
				}
				if st != nil {
					t.Errorf("Open() store = %v, want nil on error", st)
				}
				return
			}
			if st == nil {
				t.Fatal("Open() store = nil, want non-nil on success")
			}
		})
	}
}

// TestOpenOptions covers option resolution: Open applies the 512 KiB default and a
// positive WithOffloadThreshold overrides it, while a non-positive value is ignored
// (the option owns its invariant, mirroring the journal lease options).
func TestOpenOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
		want int
	}{
		{name: "default threshold", opts: nil, want: defaultOffloadThreshold},
		{name: "positive override applied", opts: []Option{WithOffloadThreshold(4096)}, want: 4096},
		{name: "zero override ignored", opts: []Option{WithOffloadThreshold(0)}, want: defaultOffloadThreshold},
		{name: "negative override ignored", opts: []Option{WithOffloadThreshold(-1)}, want: defaultOffloadThreshold},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(memstore.New(), tt.opts...)
			if err != nil {
				t.Fatalf("Open() err = %v", err)
			}
			if st.opts.OffloadThreshold != tt.want {
				t.Errorf("OffloadThreshold = %d, want %d", st.opts.OffloadThreshold, tt.want)
			}
		})
	}
}

// TestSessionName covers the ledger-name derivation: "sessions/<uuid>" for any id
// (including the zero uuid), and that every derived name passes storage.ValidateName
// so a session can never address an invalid backend location.
func TestSessionName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   uuid.UUID
		want string
	}{
		{
			name: "zero uuid",
			id:   uuid.UUID{},
			want: "sessions/00000000-0000-0000-0000-000000000000",
		},
		{
			name: "fixed uuid",
			id:   mustUUID(t, "0123abcd-4567-4890-8abc-def012345678"),
			want: "sessions/0123abcd-4567-4890-8abc-def012345678",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ledgerName(tt.id); got != tt.want {
				t.Errorf("ledgerName() = %q, want %q", got, tt.want)
			}
			name, err := sessionName(tt.id)
			if err != nil {
				t.Fatalf("sessionName() err = %v, want nil", err)
			}
			if name != tt.want {
				t.Errorf("sessionName() = %q, want %q", name, tt.want)
			}
			if err := storage.ValidateName(name); err != nil {
				t.Errorf("ValidateName(%q) = %v, want nil", name, err)
			}
		})
	}
}
