package journal

import (
	"context"
	"errors"
	"testing"
)

// TestAppendResultZeroValue pins the zero value: an unset AppendResult reports no
// sequence and Appended=false, so a caller that forgets to check an error cannot
// mistake a zero value for a successful fresh append.
func TestAppendResultZeroValue(t *testing.T) {
	t.Parallel()
	var r AppendResult
	if r.Sequence != 0 || r.Appended {
		t.Errorf("zero AppendResult = %+v, want {Sequence:0 Appended:false}", r)
	}
}

// TestIdempotencyIndexCheckUnseenID proves a fresh index reports every id as unseen:
// the caller should proceed to append it as new.
func TestIdempotencyIndexCheckUnseenID(t *testing.T) {
	t.Parallel()
	idx := NewIdempotencyIndex()
	seq, dup, err := idx.Check("unseen", NewFingerprint("event", []byte(`{"a":1}`)))
	if err != nil {
		t.Fatalf("Check() err = %v, want nil", err)
	}
	if dup {
		t.Error("Check() duplicate = true for an unseen id, want false")
	}
	if seq != 0 {
		t.Errorf("Check() seq = %d for an unseen id, want 0", seq)
	}
}

// TestIdempotencyIndexCheckIdenticalRetry proves a retry carrying the SAME id and the
// SAME persisted (kind, body) is detected as a duplicate and reports the ORIGINAL
// sequence, not an error.
func TestIdempotencyIndexCheckIdenticalRetry(t *testing.T) {
	t.Parallel()
	idx := NewIdempotencyIndex()
	fp := NewFingerprint("event", []byte(`{"a":1}`))
	idx.Observe("id-1", 5, fp)

	seq, dup, err := idx.Check("id-1", fp)
	if err != nil {
		t.Fatalf("Check() err = %v, want nil", err)
	}
	if !dup {
		t.Error("Check() duplicate = false for an identical retry, want true")
	}
	if seq != 5 {
		t.Errorf("Check() seq = %d, want the original seq 5", seq)
	}
}

// TestIdempotencyIndexCheckCollision proves a SAME id carrying a DIFFERENT persisted
// kind or payload is rejected with a typed *IdempotencyCollisionError rather than
// silently accepted as a duplicate or a new record — covering both a body change and
// a kind change (kind is part of the fingerprint too).
func TestIdempotencyIndexCheckCollision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fp   Fingerprint
	}{
		{name: "same kind, different body", fp: NewFingerprint("event", []byte(`{"a":2}`))},
		{name: "different kind, same body", fp: NewFingerprint("command", []byte(`{"a":1}`))},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			idx := NewIdempotencyIndex()
			idx.Observe("id-1", 5, NewFingerprint("event", []byte(`{"a":1}`)))

			seq, dup, err := idx.Check("id-1", tt.fp)
			if seq != 0 || dup {
				t.Errorf("Check() = (%d, %v), want (0, false) on collision", seq, dup)
			}
			var collision *IdempotencyCollisionError
			if !errors.As(err, &collision) {
				t.Fatalf("Check() err = %v, want *IdempotencyCollisionError", err)
			}
			if collision.ID != "id-1" || collision.Seq != 5 {
				t.Errorf("collision = %+v, want {ID:id-1 Seq:5}", collision)
			}
			if collision.Error() == "" {
				t.Error("IdempotencyCollisionError.Error() is empty")
			}
		})
	}
}

// TestIdempotencyIndexObserveOverwrites proves Observe replaces any prior entry for
// an id — the shape a backend relies on both while hydrating from history (later
// records win over earlier hydration passes are never repeated, but re-Observing the
// SAME id after a fresh successful append must supersede whatever hydration saw) and
// immediately after a fresh append lands.
func TestIdempotencyIndexObserveOverwrites(t *testing.T) {
	t.Parallel()
	idx := NewIdempotencyIndex()
	fp := NewFingerprint("event", []byte("body"))
	idx.Observe("id", 1, fp)
	idx.Observe("id", 2, fp)

	seq, dup, err := idx.Check("id", fp)
	if err != nil || !dup || seq != 2 {
		t.Fatalf("Check() = (%d, %v, %v), want (2, true, nil)", seq, dup, err)
	}
}

// TestNewFingerprintEquality proves Fingerprint equality is exactly (kind, body)
// byte-identity: same kind+body compares equal; a different kind or a different body
// compares unequal, even when the other component is held constant.
func TestNewFingerprintEquality(t *testing.T) {
	t.Parallel()
	base := NewFingerprint("event", []byte("body"))
	tests := []struct {
		name string
		fp   Fingerprint
		want bool
	}{
		{name: "identical kind+body", fp: NewFingerprint("event", []byte("body")), want: true},
		{name: "different body", fp: NewFingerprint("event", []byte("other")), want: false},
		{name: "different kind", fp: NewFingerprint("command", []byte("body")), want: false},
		{name: "empty body distinct from non-empty", fp: NewFingerprint("event", nil), want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := base == tt.fp; got != tt.want {
				t.Errorf("base == fp = %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeIdempotentJournal is a minimal SessionJournal double that also implements
// IdempotentJournal, used only to pin the compile-time contract: an IdempotentJournal
// value is usable anywhere a plain SessionJournal is expected (interface embedding),
// so the optional seam never narrows or replaces the existing one.
type fakeIdempotentJournal struct{}

func (fakeIdempotentJournal) Append(context.Context, JournalRecord) (uint64, error) {
	return 0, nil
}

func (fakeIdempotentJournal) AppendIdempotent(context.Context, JournalRecord) (AppendResult, error) {
	return AppendResult{}, nil
}

// TestIdempotentJournalSatisfiesSessionJournal proves IdempotentJournal embeds
// SessionJournal: any IdempotentJournal implementation is assignable to the plain,
// narrower SessionJournal interface without adapting it.
func TestIdempotentJournalSatisfiesSessionJournal(t *testing.T) {
	t.Parallel()
	var idem IdempotentJournal = fakeIdempotentJournal{}
	var plain SessionJournal = idem
	if plain == nil {
		t.Fatal("IdempotentJournal value is not assignable to SessionJournal")
	}
}
