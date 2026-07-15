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
			if got.Kind == compactionAdmissionLaneFull {
				if control.pending == nil || control.pending.attemptID != attemptID || len(control.pending.waiters) != 1 || control.pending.waiters[0].commandID != tt.seed[0].CommandID {
					t.Fatalf("lane-full mutated owning attempt: %+v", control.pending)
				}
			}
		})
	}
}

func TestArbitrateCompactionBoundaryPrioritizesReadyControl(t *testing.T) {
	t.Parallel()
	createdAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		command    command.Command
		wantReject event.CompactRejectReason
	}{
		{
			name:       "interrupt ready with boundary",
			command:    command.Interrupt{Header: command.Header{CommandID: uuid.UUID{2}}, Ack: make(chan bool, 1)},
			wantReject: event.CompactRejectInterrupted,
		},
		{
			name:       "shutdown ready with boundary",
			command:    command.Shutdown{Header: command.Header{CommandID: uuid.UUID{3}}, Ack: make(chan error, 1)},
			wantReject: event.CompactRejectShuttingDown,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			control := newCompactionControl(4)
			if _, err := control.admit(compactCommand(uuid.UUID{1}, createdAt, identity.AgencyUser), fixedCompactionID); err != nil {
				t.Fatalf("admit: %v", err)
			}
			commands := make(chan command.Command, 1)
			commands <- tt.command
			var got compactionDisposition
			exit := arbitrateCompactionBoundary(
				commands,
				func(cmd command.Command) bool {
					switch cmd.(type) {
					case command.Interrupt:
						control.interrupt()
					case command.Shutdown:
						control.shutdown()
					}
					return false
				},
				func() { got = control.atBoundary(compactionBoundaryStep) },
			)
			if exit {
				t.Fatal("arbitration unexpectedly requested actor exit")
			}
			if got.Kind != compactionDispositionReject || got.RejectReason != tt.wantReject {
				t.Errorf("boundary disposition = %+v, want rejection %v", got, tt.wantReject)
			}
		})
	}
}

func TestArbitrateCompactionBoundaryBoundsReadySnapshot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		queued        int
		wantHandled   int
		wantRemaining int
	}{
		{name: "empty lane dispatches", queued: 0, wantHandled: 0, wantRemaining: 0},
		{name: "ready controls preserve fifo", queued: 2, wantHandled: 2, wantRemaining: 0},
		{name: "snapshot is capped", queued: compactionPriorityCommandCapacity + 2, wantHandled: compactionPriorityCommandCapacity, wantRemaining: 2},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			priority := make(chan command.Command, compactionPriorityCommandCapacity+2)
			for i := 0; i < tt.queued; i++ {
				priority <- command.Interrupt{Header: command.Header{CommandID: uuid.UUID{byte(i + 1)}}, Ack: make(chan bool, 1)}
			}
			var handled []uuid.UUID
			dispatched := 0
			exit := arbitrateCompactionBoundary(priority, func(cmd command.Command) bool {
				handled = append(handled, cmd.CommandHeader().CommandID)
				return false
			}, func() { dispatched++ })
			if exit {
				t.Fatal("arbitration unexpectedly requested actor exit")
			}
			if len(handled) != tt.wantHandled || len(priority) != tt.wantRemaining {
				t.Fatalf("handled = %d remaining = %d, want %d and %d", len(handled), len(priority), tt.wantHandled, tt.wantRemaining)
			}
			if dispatched != 1 {
				t.Fatalf("dispatch count = %d, want 1", dispatched)
			}
			for i, id := range handled {
				if want := (uuid.UUID{byte(i + 1)}); id != want {
					t.Fatalf("handled[%d] = %v, want FIFO id %v", i, id, want)
				}
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
