package loopruntime

import (
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

var errCompactionAttemptID = errors.New("compaction attempt id generation failed")

func TestCompactionControlAdmit(t *testing.T) {
	t.Parallel()
	createdAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	attemptID := event.CompactAttemptID(uuid.UUID{0xa0})
	tests := []struct {
		name        string
		capacity    int
		seed        []command.Compact
		request     command.Compact
		idErr       error
		wantKind    compactionAdmissionKind
		wantAttempt event.CompactAttemptID
		wantErr     bool
	}{
		{
			name:        "manual request opens attempt",
			capacity:    2,
			request:     compactCommand(uuid.UUID{1}, createdAt, identity.AgencyUser),
			wantKind:    compactionAdmissionOpened,
			wantAttempt: attemptID,
		},
		{
			name:        "user joins machine attempt",
			capacity:    2,
			seed:        []command.Compact{compactCommand(uuid.UUID{1}, createdAt, identity.AgencyMachine)},
			request:     compactCommand(uuid.UUID{2}, createdAt.Add(time.Second), identity.AgencyUser),
			wantKind:    compactionAdmissionJoined,
			wantAttempt: attemptID,
		},
		{
			name:        "duplicate is idempotent even at capacity",
			capacity:    1,
			seed:        []command.Compact{compactCommand(uuid.UUID{1}, createdAt, identity.AgencyMachine)},
			request:     compactCommand(uuid.UUID{1}, createdAt, identity.AgencyMachine),
			wantKind:    compactionAdmissionDuplicate,
			wantAttempt: attemptID,
		},
		{
			name:        "new waiter is rejected at capacity",
			capacity:    1,
			seed:        []command.Compact{compactCommand(uuid.UUID{1}, createdAt, identity.AgencyMachine)},
			request:     compactCommand(uuid.UUID{2}, createdAt.Add(time.Second), identity.AgencyUser),
			wantKind:    compactionAdmissionLaneFull,
			wantAttempt: attemptID,
		},
		{
			name:     "invalid request is typed validation error",
			capacity: 1,
			request:  command.Compact{},
			wantErr:  true,
		},
		{
			name:     "attempt id failure is fatal",
			capacity: 1,
			request:  compactCommand(uuid.UUID{1}, createdAt, identity.AgencyUser),
			idErr:    errCompactionAttemptID,
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			control := newCompactionControl(tt.capacity)
			gen := func() (uuid.UUID, error) {
				if tt.idErr != nil {
					return uuid.UUID{}, tt.idErr
				}
				return uuid.UUID(attemptID), nil
			}
			for _, seed := range tt.seed {
				if _, err := control.admit(seed, gen); err != nil {
					t.Fatalf("seed admit: %v", err)
				}
			}
			got, err := control.admit(tt.request, gen)
			if (err != nil) != tt.wantErr {
				t.Fatalf("admit error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.idErr != nil {
					var target *CompactionCoordinationError
					if !errors.As(err, &target) || target.Kind != CompactionCoordinationAttemptID || !errors.Is(err, tt.idErr) {
						t.Fatalf("admit error = %T %v, want attempt-id coordination error", err, err)
					}
				} else {
					var target *command.CommandValidationError
					if !errors.As(err, &target) {
						t.Fatalf("admit error = %T %v, want CommandValidationError", err, err)
					}
				}
				return
			}
			if got.Kind != tt.wantKind || got.AttemptID != tt.wantAttempt {
				t.Errorf("admit = %+v, want kind %v attempt %v", got, tt.wantKind, tt.wantAttempt)
			}
		})
	}
}

func TestCompactionControlCanonicalWaiters(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   []command.Compact
		want []uuid.UUID
	}{
		{
			name: "created time ascending",
			in: []command.Compact{
				compactCommand(uuid.UUID{3}, base.Add(2*time.Second), identity.AgencyMachine),
				compactCommand(uuid.UUID{1}, base, identity.AgencyUser),
				compactCommand(uuid.UUID{2}, base.Add(time.Second), identity.AgencyMachine),
			},
			want: []uuid.UUID{{1}, {2}, {3}},
		},
		{
			name: "uuid bytes break timestamp ties",
			in: []command.Compact{
				compactCommand(uuid.UUID{3}, base, identity.AgencyMachine),
				compactCommand(uuid.UUID{1}, base, identity.AgencyUser),
				compactCommand(uuid.UUID{2}, base, identity.AgencyMachine),
			},
			want: []uuid.UUID{{1}, {2}, {3}},
		},
		{
			name: "duplicate command id appears once",
			in: []command.Compact{
				compactCommand(uuid.UUID{2}, base, identity.AgencyMachine),
				compactCommand(uuid.UUID{1}, base, identity.AgencyUser),
				compactCommand(uuid.UUID{2}, base.Add(time.Second), identity.AgencyUser),
			},
			want: []uuid.UUID{{1}, {2}},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			control := newCompactionControl(len(tt.in))
			for _, request := range tt.in {
				if _, err := control.admit(request, func() (uuid.UUID, error) { return uuid.UUID{0xa0}, nil }); err != nil {
					t.Fatalf("admit: %v", err)
				}
			}
			got := control.pendingAttempt()
			if got == nil {
				t.Fatal("pendingAttempt = nil")
			}
			if len(got.WaiterCommandIDs) != len(tt.want) {
				t.Fatalf("waiters = %v, want %v", got.WaiterCommandIDs, tt.want)
			}
			for i := range tt.want {
				if got.WaiterCommandIDs[i] != tt.want[i] {
					t.Errorf("waiters[%d] = %v, want %v", i, got.WaiterCommandIDs[i], tt.want[i])
				}
			}
		})
	}
}

func TestCompactionControlBoundaryPriority(t *testing.T) {
	t.Parallel()
	createdAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		prepare    func(*compactionControl)
		boundary   compactionBoundaryKind
		wantKind   compactionDispositionKind
		wantReject event.CompactRejectReason
	}{
		{
			name: "safe boundary starts pending attempt once",
			prepare: func(control *compactionControl) {
				_, _ = control.admit(compactCommand(uuid.UUID{1}, createdAt, identity.AgencyUser), fixedCompactionID)
			},
			boundary: compactionBoundaryStep,
			wantKind: compactionDispositionStart,
		},
		{
			name: "second boundary does not duplicate start",
			prepare: func(control *compactionControl) {
				_, _ = control.admit(compactCommand(uuid.UUID{1}, createdAt, identity.AgencyUser), fixedCompactionID)
				_ = control.atBoundary(compactionBoundaryStep)
			},
			boundary: compactionBoundaryTurn,
			wantKind: compactionDispositionNone,
		},
		{
			name: "interrupt outranks pending compaction",
			prepare: func(control *compactionControl) {
				_, _ = control.admit(compactCommand(uuid.UUID{1}, createdAt, identity.AgencyUser), fixedCompactionID)
				control.interrupt()
			},
			boundary:   compactionBoundaryStep,
			wantKind:   compactionDispositionReject,
			wantReject: event.CompactRejectInterrupted,
		},
		{
			name: "shutdown outranks earlier interrupt",
			prepare: func(control *compactionControl) {
				_, _ = control.admit(compactCommand(uuid.UUID{1}, createdAt, identity.AgencyUser), fixedCompactionID)
				control.interrupt()
				control.shutdown()
			},
			boundary:   compactionBoundaryTurn,
			wantKind:   compactionDispositionReject,
			wantReject: event.CompactRejectShuttingDown,
		},
		{
			name:     "empty boundary is a no-op",
			prepare:  func(*compactionControl) {},
			boundary: compactionBoundaryTurn,
			wantKind: compactionDispositionNone,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			control := newCompactionControl(4)
			tt.prepare(control)
			got := control.atBoundary(tt.boundary)
			if got.Kind != tt.wantKind || got.RejectReason != tt.wantReject {
				t.Errorf("atBoundary = %+v, want kind %v reject %v", got, tt.wantKind, tt.wantReject)
			}
		})
	}
}

func compactCommand(commandID uuid.UUID, createdAt time.Time, agency identity.Agency) command.Compact {
	return command.Compact{
		Header:      command.Header{CommandID: commandID, CreatedAt: createdAt, Agency: agency},
		Coordinates: identity.Coordinates{SessionID: uuid.UUID{0x10}, LoopID: uuid.UUID{0x20}},
	}
}

func fixedCompactionID() (uuid.UUID, error) { return uuid.UUID{0xa0}, nil }
