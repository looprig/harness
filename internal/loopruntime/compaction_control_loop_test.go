package loopruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/runtimecontract"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

type recordingCompactionSink struct {
	mu            sync.Mutex
	dispositions  []compactionDisposition
	failures      []compactionFailure
	notify        chan struct{}
	coordinateErr error
}

func newRecordingCompactionSink() *recordingCompactionSink {
	return &recordingCompactionSink{notify: make(chan struct{}, compactionControlWaiterCapacity+8)}
}

func (s *recordingCompactionSink) CoordinateCompaction(_ context.Context, disposition compactionDisposition) error {
	s.mu.Lock()
	s.dispositions = append(s.dispositions, disposition)
	err := s.coordinateErr
	s.coordinateErr = nil
	s.mu.Unlock()
	s.notify <- struct{}{}
	return err
}

func (s *recordingCompactionSink) ReportCompactionFailure(_ context.Context, failure compactionFailure) {
	s.mu.Lock()
	s.failures = append(s.failures, failure)
	s.mu.Unlock()
	s.notify <- struct{}{}
}

func (s *recordingCompactionSink) snapshot() ([]compactionDisposition, []compactionFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]compactionDisposition(nil), s.dispositions...), append([]compactionFailure(nil), s.failures...)
}

func TestLoopCompactionControlBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "idle turn boundary starts once",
			run: func(t *testing.T) {
				l, _, sink, sessionID, loopID := newCompactionTestLoop(t, &fakeLLM{})
				sendCompact(t, l, sessionID, loopID, uuid.UUID{1}, identity.AgencyMachine)
				got := awaitCompactionDisposition(t, sink)
				if got.Kind != compactionDispositionStart || got.Attempt == nil || got.Attempt.Reason != event.CompactionReasonAutomatic {
					t.Fatalf("disposition = %+v, want automatic start", got)
				}
				sendCompact(t, l, sessionID, loopID, uuid.UUID{2}, identity.AgencyUser)
				syncLoopActor(t, l)
				dispositions, _ := sink.snapshot()
				if len(dispositions) != 1 {
					t.Fatalf("dispositions = %d, want one shared start", len(dispositions))
				}
			},
		},
		{
			name: "tool continuation consumes at safe step boundary",
			run: func(t *testing.T) {
				blocking := newBlockingTool()
				tools := agenticToolSet([]tool.InvokableTool{blocking}, 25, 100)
				client := &scriptedLLM{scripts: [][]content.Chunk{
					{toolUseChunk(0, "id-1", "Block", `{}`)},
					{textChunk("done")},
				}}
				l, rec, sink, sessionID, loopID := newCompactionTestLoopWithTools(t, client, tools)
				startTurn(t, l, rec, textBlocks("run"))
				<-blocking.started
				sendCompact(t, l, sessionID, loopID, uuid.UUID{1}, identity.AgencyUser)
				syncLoopActor(t, l)
				if dispositions, _ := sink.snapshot(); len(dispositions) != 0 {
					t.Fatalf("pre-boundary dispositions = %v, want none", dispositions)
				}
				close(blocking.release)
				got := awaitCompactionDisposition(t, sink)
				if got.Kind != compactionDispositionStart || got.Attempt == nil {
					t.Fatalf("disposition = %+v, want step-boundary start", got)
				}
			},
		},
		{
			name: "interrupt outranks pending running request",
			run: func(t *testing.T) {
				l, rec, sink, sessionID, loopID := newCompactionTestLoop(t, &fakeLLM{blockUntilCancel: true})
				startTurn(t, l, rec, textBlocks("run"))
				// Fill the ordinary input queue to its independent capacity. Compact still
				// enters the control slot; the later interrupted disposition proves it was
				// not refused by UserInput fullness.
				for i := 0; i < runtimecontract.ManagedInputQueueCapacity; i++ {
					l.Commands <- command.UserInput{Header: command.Header{CommandID: uuid.UUID{byte(i + 10)}}}
				}
				sendCompact(t, l, sessionID, loopID, uuid.UUID{1}, identity.AgencyUser)
				syncLoopActor(t, l)
				if dispositions, _ := sink.snapshot(); len(dispositions) != 0 {
					t.Fatalf("pre-boundary dispositions = %v, want none", dispositions)
				}
				ack := make(chan bool, 1)
				l.Commands <- command.Interrupt{Header: command.Header{CommandID: uuid.UUID{3}}, Ack: ack}
				if !<-ack {
					t.Fatal("Interrupt did not cancel active turn")
				}
				got := awaitCompactionDisposition(t, sink)
				if got.Kind != compactionDispositionReject || got.RejectReason != event.CompactRejectInterrupted {
					t.Fatalf("disposition = %+v, want interrupted rejection", got)
				}
			},
		},
		{
			name: "shutdown outranks pending running request",
			run: func(t *testing.T) {
				l, rec, sink, sessionID, loopID := newCompactionTestLoop(t, &fakeLLM{blockUntilCancel: true})
				startTurn(t, l, rec, textBlocks("run"))
				sendCompact(t, l, sessionID, loopID, uuid.UUID{1}, identity.AgencyUser)
				syncLoopActor(t, l)
				ack := make(chan error, 1)
				l.Commands <- command.Shutdown{Header: command.Header{CommandID: uuid.UUID{3}}, Ack: ack}
				got := awaitCompactionDisposition(t, sink)
				if got.Kind != compactionDispositionReject || got.RejectReason != event.CompactRejectShuttingDown {
					t.Fatalf("disposition = %+v, want shutdown rejection", got)
				}
				if err := <-ack; err != nil {
					t.Fatalf("Shutdown error = %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
}

func TestLoopActorPriorityLaneOutranksOrdinaryContentionAtBoundary(t *testing.T) {
	t.Parallel()
	type priorityControl uint8
	const (
		priorityInterrupt priorityControl = iota
		priorityShutdown
	)
	tests := []struct {
		name             string
		controls         []priorityControl
		wantReject       event.CompactRejectReason
		wantInterruptAck bool
	}{
		{
			name:             "interrupt priority lane",
			controls:         []priorityControl{priorityInterrupt},
			wantReject:       event.CompactRejectInterrupted,
			wantInterruptAck: true,
		},
		{
			name:       "shutdown priority lane",
			controls:   []priorityControl{priorityShutdown},
			wantReject: event.CompactRejectShuttingDown,
		},
		{
			name:             "interrupt then shutdown gives shutdown precedence",
			controls:         []priorityControl{priorityInterrupt, priorityShutdown},
			wantReject:       event.CompactRejectShuttingDown,
			wantInterruptAck: true,
		},
		{
			name:       "shutdown then interrupt preserves order and shutdown precedence",
			controls:   []priorityControl{priorityShutdown, priorityInterrupt},
			wantReject: event.CompactRejectShuttingDown,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			normal := make(chan command.Command, 2)
			priority := make(chan command.Command, compactionPriorityCommandCapacity)
			drains := make(chan drainRequest, 1)
			boundaryEntered := make(chan struct{})
			boundaryRelease := make(chan struct{})
			minted := make(chan struct{})
			var mintOnce sync.Once
			gen := func() (uuid.UUID, error) {
				mintOnce.Do(func() { close(minted) })
				return uuid.UUID{0xa0}, nil
			}
			rec := &recordingPublisher{}
			sink := newRecordingCompactionSink()
			done := make(chan struct{})
			internal := make(chan turnResult, 1)
			cfg := loopConfig{
				loopCtx:          ctx,
				cfg:              runtimeConfig{Client: &fakeLLM{}, Model: testModel(), DrainTimeout: 20 * time.Millisecond, idGen: gen, eventFactory: workingFactory(), compactionSink: sink, beforeCompactionBoundary: func(compactionBoundaryKind) { close(boundaryEntered); <-boundaryRelease }},
				commands:         normal,
				priorityCommands: priority,
				gateReg:          make(chan gateRegistration),
				snapshots:        make(chan snapshotRequest),
				internal:         internal,
				commits:          make(chan commitRequest),
				drains:           drains,
				admissions:       make(chan admissionResult),
				done:             done,
				events:           rec,
				eventFactory:     workingFactory(),
				gates:            nopGateRegistrar{},
			}
			state := newLoopState(uuid.UUID{0x10}, uuid.UUID{0x20}, Provenance{})
			state.status = loopRunning
			state.turnID = uuid.UUID{0x30}
			state.cancelTurn = func() {}
			go runLoop(cfg, state)

			normal <- compactCommand(uuid.UUID{1}, time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC), identity.AgencyUser)
			<-minted
			drainReply := make(chan []queuedInput, 1)
			drains <- drainRequest{reply: drainReply}
			<-boundaryEntered
			ordinaryID := uuid.UUID{2}
			normal <- command.UserInput{Header: command.Header{CommandID: ordinaryID}}
			var interruptAck chan bool
			var shutdownAck chan error
			for i, control := range tt.controls {
				switch control {
				case priorityInterrupt:
					interruptAck = make(chan bool, 1)
					priority <- command.Interrupt{Header: command.Header{CommandID: uuid.UUID{byte(i + 3)}}, Ack: interruptAck}
				case priorityShutdown:
					shutdownAck = make(chan error, 1)
					priority <- command.Shutdown{Header: command.Header{CommandID: uuid.UUID{byte(i + 3)}}, Ack: shutdownAck}
				}
			}
			close(boundaryRelease)

			got := awaitCompactionDisposition(t, sink)
			if got.Kind != compactionDispositionReject || got.RejectReason != tt.wantReject {
				t.Fatalf("priority disposition = %+v, want rejection %v", got, tt.wantReject)
			}
			if _, ok := <-drainReply; !ok {
				t.Fatal("drain reply channel closed")
			}
			ordinaryReply := awaitReply(t, rec, ordinaryID)
			switch tt.wantReject {
			case event.CompactRejectInterrupted:
				if _, ok := ordinaryReply.(event.InputQueued); !ok {
					t.Fatalf("ordinary reply = %T, want InputQueued", ordinaryReply)
				}
			case event.CompactRejectShuttingDown:
				if rejected, ok := ordinaryReply.(event.TurnRejected); !ok || rejected.Reason != event.RejectShuttingDown {
					t.Fatalf("ordinary reply = %+v, want shutting-down TurnRejected", ordinaryReply)
				}
			}
			dispositions, _ := sink.snapshot()
			if len(dispositions) != 1 {
				t.Fatalf("dispositions = %v, want one rejection and no start", dispositions)
			}
			internal <- turnResult{terminal: event.TurnInterrupted{}}
			if tt.wantReject == event.CompactRejectInterrupted {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("loop actor did not stop")
			}
			if interruptAck != nil {
				select {
				case got := <-interruptAck:
					if got != tt.wantInterruptAck {
						t.Errorf("interrupt ack = %v, want %v (control order not preserved)", got, tt.wantInterruptAck)
					}
				default:
					t.Fatal("priority interrupt was not processed")
				}
				if len(interruptAck) != 0 {
					t.Fatalf("interrupt processed more than once: %d extra acks", len(interruptAck))
				}
			}
			if shutdownAck != nil {
				select {
				case err := <-shutdownAck:
					if err != nil {
						t.Errorf("shutdown ack = %v, want nil", err)
					}
				default:
					t.Fatal("priority shutdown was not processed")
				}
				if len(shutdownAck) != 0 {
					t.Fatalf("shutdown processed more than once: %d extra acks", len(shutdownAck))
				}
			}
		})
	}
}

func TestLoopCompactionLaneFullOutcome(t *testing.T) {
	t.Parallel()
	errChecked := errors.New("checked publication failed")
	tests := []struct {
		name       string
		checkedErr error
		wantEvent  bool
		wantFatal  bool
	}{
		{name: "writable journal emits immediate durable waiter rejection", wantEvent: true},
		{name: "checked publication failure propagates without false event", checkedErr: errChecked, wantFatal: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, rec, sink, sessionID, loopID := newCompactionTestLoop(t, &fakeLLM{})
			rec.setCheckedError(tt.checkedErr)
			for i := 0; i < compactionControlWaiterCapacity; i++ {
				sendCompact(t, l, sessionID, loopID, uuid.UUID{byte(i + 1)}, identity.AgencyMachine)
			}
			disposition := awaitCompactionDisposition(t, sink)
			overflowID := uuid.UUID{0xff}
			sendCompact(t, l, sessionID, loopID, overflowID, identity.AgencyUser)
			syncLoopActor(t, l)

			var got *event.CompactWaiterRejected
			for _, published := range rec.events() {
				if value, ok := published.(event.CompactWaiterRejected); ok && value.Cause.CommandID == overflowID {
					copyOfValue := value
					got = &copyOfValue
				}
			}
			if (got != nil) != tt.wantEvent {
				t.Fatalf("lane-full event = %+v, wantEvent %v", got, tt.wantEvent)
			}
			if got != nil {
				if got.AttemptID != disposition.Attempt.AttemptID || got.Reason != event.CompactRejectControlLaneFull {
					t.Errorf("lane-full event = %+v, want attempt %v reason ControlLaneFull", got, disposition.Attempt.AttemptID)
				}
				if got.EventID != event.CompactWaiterReplyID(got.AttemptID, overflowID, false) || got.CreatedAt.IsZero() {
					t.Errorf("lane-full event identity = %v/%v, want deterministic id and stamped time", got.EventID, got.CreatedAt)
				}
			}
			_, failures := sink.snapshot()
			if (len(failures) == 1) != tt.wantFatal {
				t.Fatalf("failures = %+v, wantFatal %v", failures, tt.wantFatal)
			}
			if tt.wantFatal {
				var coordinationErr *CompactionCoordinationError
				if !equalUUIDs(failures[0].WaiterCommandIDs, []uuid.UUID{overflowID}) || !errors.As(failures[0].Err, &coordinationErr) || coordinationErr.Kind != CompactionCoordinationOutcome || !errors.Is(failures[0].Err, errChecked) {
					t.Fatalf("failure = %+v, want typed outcome failure for overflow command", failures[0])
				}
			}
		})
	}
}

func TestLoopCompactionAttemptIDFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{name: "fatal mint failure has no false durable outcome", err: errCompactionAttemptID},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			sessionID, loopID := mustID(t), mustID(t)
			rec := &recordingPublisher{}
			sink := newRecordingCompactionSink()
			l, err := newWithConfig(ctx, sessionID, loopID, Provenance{}, rec, runtimeConfig{
				Client:         &fakeLLM{},
				Model:          testModel(),
				idGen:          func() (uuid.UUID, error) { return uuid.UUID{}, tt.err },
				eventFactory:   workingFactory(),
				compactionSink: sink,
			})
			if err != nil {
				t.Fatalf("newWithConfig: %v", err)
			}
			sendCompact(t, l, sessionID, loopID, uuid.UUID{1}, identity.AgencyUser)
			syncLoopActor(t, l)
			dispositions, failures := sink.snapshot()
			if len(dispositions) != 0 || len(failures) != 1 {
				t.Fatalf("dispositions/failures = %v/%v, want 0/1", dispositions, failures)
			}
			var coordinationErr *CompactionCoordinationError
			if !equalUUIDs(failures[0].WaiterCommandIDs, []uuid.UUID{{1}}) || !errors.As(failures[0].Err, &coordinationErr) || coordinationErr.Kind != CompactionCoordinationAttemptID || !errors.Is(failures[0].Err, tt.err) {
				t.Fatalf("failure = %+v, want typed attempt id failure", failures[0])
			}
			for _, published := range rec.events() {
				switch published.(type) {
				case event.CompactionRejected, event.CompactWaiterRejected:
					t.Fatalf("published false durable outcome %T", published)
				}
			}
		})
	}
}

func TestLoopCompactionSinkFailureNotifiesAllWaitersAndClearsAttempt(t *testing.T) {
	t.Parallel()
	errSink := errors.New("compaction sink unavailable")
	tests := []struct {
		name        string
		requests    []command.Compact
		wantWaiters []uuid.UUID
	}{
		{
			name: "canonical full waiter set is failed once and next attempt can start",
			requests: []command.Compact{
				compactCommand(uuid.UUID{3}, time.Date(2026, 7, 14, 12, 0, 2, 0, time.UTC), identity.AgencyMachine),
				compactCommand(uuid.UUID{1}, time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC), identity.AgencyUser),
				compactCommand(uuid.UUID{2}, time.Date(2026, 7, 14, 12, 0, 1, 0, time.UTC), identity.AgencyMachine),
			},
			wantWaiters: []uuid.UUID{{1}, {2}, {3}},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			blocking := newBlockingTool()
			tools := agenticToolSet([]tool.InvokableTool{blocking}, 25, 100)
			client := &scriptedLLM{scripts: [][]content.Chunk{
				{toolUseChunk(0, "id-1", "Block", `{}`)},
				{textChunk("done")},
			}}
			l, rec, sink, _, _ := newCompactionTestLoopWithTools(t, client, tools)
			sink.coordinateErr = errSink
			startTurn(t, l, rec, textBlocks("run"))
			<-blocking.started
			for _, request := range tt.requests {
				l.Commands <- request
			}
			syncLoopActor(t, l)
			close(blocking.release)
			failure := awaitCompactionFailure(t, sink)
			if !equalUUIDs(failure.WaiterCommandIDs, tt.wantWaiters) {
				t.Fatalf("failure waiters = %v, want %v", failure.WaiterCommandIDs, tt.wantWaiters)
			}
			var coordinationErr *CompactionCoordinationError
			if !errors.As(failure.Err, &coordinationErr) || coordinationErr.Kind != CompactionCoordinationOutcome || !errors.Is(failure.Err, errSink) {
				t.Fatalf("failure = %T %v, want typed sink infrastructure failure", failure.Err, failure.Err)
			}
			_, failures := sink.snapshot()
			if len(failures) != 1 {
				t.Fatalf("failure notifications = %d, want exactly one", len(failures))
			}
			for _, published := range rec.events() {
				switch published.(type) {
				case event.CompactionCommitted, event.CompactionRejected, event.CompactWaiterResolved, event.CompactWaiterRejected:
					t.Fatalf("published false durable outcome %T", published)
				}
			}
			if _, ok := drainToTerminal(t, rec).(event.TurnDone); !ok {
				t.Fatal("turn terminal != TurnDone")
			}
			sendCompact(t, l, tt.requests[0].SessionID, tt.requests[0].LoopID, uuid.UUID{4}, identity.AgencyUser)
			got := awaitCompactionDispositionCount(t, sink, 2)
			if got.Kind != compactionDispositionStart || got.Attempt == nil || !equalUUIDs(got.Attempt.WaiterCommandIDs, []uuid.UUID{{4}}) {
				t.Fatalf("subsequent disposition = %+v, want fresh one-waiter start", got)
			}
		})
	}
}

func newCompactionTestLoop(t *testing.T, client inference.Client) (*Loop, *recordingPublisher, *recordingCompactionSink, uuid.UUID, uuid.UUID) {
	t.Helper()
	return newCompactionTestLoopWithTools(t, client, ToolSet{})
}

func newCompactionTestLoopWithTools(t *testing.T, client inference.Client, tools ToolSet) (*Loop, *recordingPublisher, *recordingCompactionSink, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID, loopID := mustID(t), mustID(t)
	rec := &recordingPublisher{}
	sink := newRecordingCompactionSink()
	l, err := newWithConfig(ctx, sessionID, loopID, Provenance{}, rec, runtimeConfig{
		Client: client, Model: testModel(), Tools: tools, DrainTimeout: 200 * time.Millisecond, compactionSink: sink,
	})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	return l, rec, sink, sessionID, loopID
}

func sendCompact(t *testing.T, l *Loop, sessionID, loopID, commandID uuid.UUID, agency identity.Agency) {
	t.Helper()
	l.Commands <- command.Compact{
		Header:      command.Header{CommandID: commandID, CreatedAt: time.Now(), Agency: agency},
		Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID},
	}
}

func syncLoopActor(t *testing.T, l *Loop) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := l.Snapshot(ctx); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
}

func awaitCompactionDisposition(t *testing.T, sink *recordingCompactionSink) compactionDisposition {
	t.Helper()
	select {
	case <-sink.notify:
		dispositions, _ := sink.snapshot()
		if len(dispositions) == 0 {
			t.Fatal("notification carried no compaction disposition")
		}
		return dispositions[len(dispositions)-1]
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for compaction disposition")
		return compactionDisposition{}
	}
}

func awaitCompactionDispositionCount(t *testing.T, sink *recordingCompactionSink, count int) compactionDisposition {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		dispositions, _ := sink.snapshot()
		if len(dispositions) >= count {
			return dispositions[count-1]
		}
		select {
		case <-sink.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %d compaction dispositions", count)
			return compactionDisposition{}
		}
	}
}

func awaitCompactionFailure(t *testing.T, sink *recordingCompactionSink) compactionFailure {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		_, failures := sink.snapshot()
		if len(failures) > 0 {
			return failures[0]
		}
		select {
		case <-sink.notify:
		case <-deadline:
			t.Fatal("timed out waiting for compaction failure")
			return compactionFailure{}
		}
	}
}

func equalUUIDs(left, right []uuid.UUID) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
