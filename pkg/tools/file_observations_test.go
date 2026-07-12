package tools

import (
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
)

// TestFileObservationsRecord covers the per-loop observation map's record/absent/
// invalidate transitions and the get-or-create state lookup. Each row asserts the
// resulting per-path state (observed flag + present/hash) after a sequence of
// operations, plus that distinct canonical keys never alias.
func TestFileObservationsRecord(t *testing.T) {
	t.Parallel()

	hashA := sha256.Sum256([]byte("A"))
	hashB := sha256.Sum256([]byte("B"))

	tests := []struct {
		name         string
		apply        func(o *fileObservations)
		key          canonicalObservationKey
		wantObserved bool
		wantPresent  bool
		wantHash     [sha256.Size]byte
	}{
		{
			name:         "unseen key has no observation",
			apply:        func(o *fileObservations) {},
			key:          "/ws/f.txt",
			wantObserved: false,
		},
		{
			name:         "record present stores hash",
			apply:        func(o *fileObservations) { o.recordPresent("/ws/f.txt", hashA) },
			key:          "/ws/f.txt",
			wantObserved: true,
			wantPresent:  true,
			wantHash:     hashA,
		},
		{
			name:         "record absent stores absence",
			apply:        func(o *fileObservations) { o.recordAbsent("/ws/f.txt") },
			key:          "/ws/f.txt",
			wantObserved: true,
			wantPresent:  false,
		},
		{
			name: "latest record wins",
			apply: func(o *fileObservations) {
				o.recordPresent("/ws/f.txt", hashA)
				o.recordPresent("/ws/f.txt", hashB)
			},
			key:          "/ws/f.txt",
			wantObserved: true,
			wantPresent:  true,
			wantHash:     hashB,
		},
		{
			name: "invalidate clears a prior observation",
			apply: func(o *fileObservations) {
				o.recordPresent("/ws/f.txt", hashA)
				o.invalidate("/ws/f.txt")
			},
			key:          "/ws/f.txt",
			wantObserved: false,
		},
		{
			name: "distinct keys are independent",
			apply: func(o *fileObservations) {
				o.recordPresent("/ws/a.txt", hashA)
			},
			key:          "/ws/b.txt",
			wantObserved: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := newFileObservations()
			tt.apply(o)
			st := o.state(tt.key)
			st.mu.Lock()
			defer st.mu.Unlock()
			if st.observed != tt.wantObserved {
				t.Fatalf("observed = %v, want %v", st.observed, tt.wantObserved)
			}
			if !tt.wantObserved {
				return
			}
			if st.obs.present != tt.wantPresent {
				t.Fatalf("present = %v, want %v", st.obs.present, tt.wantPresent)
			}
			if tt.wantPresent && st.obs.hash != tt.wantHash {
				t.Fatalf("hash = %x, want %x", st.obs.hash, tt.wantHash)
			}
		})
	}
}

// TestFileObservationsStateStable asserts state() returns the SAME per-path record
// for the same key (so a per-path mutex is shared) and DISTINCT records for
// different keys.
func TestFileObservationsStateStable(t *testing.T) {
	t.Parallel()
	o := newFileObservations()
	if a, b := o.state("/ws/x"), o.state("/ws/x"); a != b {
		t.Fatalf("state() for the same key returned different records (%p, %p)", a, b)
	}
	if a, b := o.state("/ws/x"), o.state("/ws/y"); a == b {
		t.Fatalf("state() for different keys returned the same record (%p)", a)
	}
}

// TestFileObservationsConcurrentGetOrCreate hammers state() and recordPresent on
// one key from many goroutines: get-or-create must be race-free and converge on a
// single shared record. Run under -race.
func TestFileObservationsConcurrentGetOrCreate(t *testing.T) {
	t.Parallel()
	o := newFileObservations()
	const goroutines = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	records := make(chan *filePathState, goroutines)
	hash := sha256.Sum256([]byte("x"))
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			o.recordPresent("/ws/shared", hash)
			records <- o.state("/ws/shared")
		}()
	}
	close(start)
	wg.Wait()
	close(records)
	first := <-records
	for st := range records {
		if st != first {
			t.Fatalf("concurrent state() returned distinct records for one key")
		}
	}
}

// TestFileObservationErrorsAreTyped pins the concrete mutation-refusal errors so
// callers can errors.As them, and confirms neither message carries a hash.
func TestFileObservationErrorsAreTyped(t *testing.T) {
	t.Parallel()
	var stale error = &StaleFileError{Path: "a/b.txt"}
	var got *StaleFileError
	if !errors.As(stale, &got) || got.Path != "a/b.txt" {
		t.Fatalf("StaleFileError not errors.As-able: %v", stale)
	}
	var conflict error = &FileCreateConflictError{Path: "a/b.txt"}
	var gotC *FileCreateConflictError
	if !errors.As(conflict, &gotC) || gotC.Path != "a/b.txt" {
		t.Fatalf("FileCreateConflictError not errors.As-able: %v", conflict)
	}
	var anchor error = &editAnchorError{message: "not found"}
	var gotA *editAnchorError
	if !errors.As(anchor, &gotA) {
		t.Fatalf("editAnchorError not errors.As-able: %v", anchor)
	}
	// A StaleFileError must never coincide with an anchor error type.
	if errors.As(stale, &gotA) {
		t.Fatalf("StaleFileError wrongly matched *editAnchorError")
	}
}
