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

type oneShotCompactionStartFailurePublisher struct {
	*recordingPublisher
	mu     sync.Mutex
	err    error
	failed bool
}

func (p *oneShotCompactionStartFailurePublisher) PublishEventChecked(ctx context.Context, value event.Event) error {
	p.mu.Lock()
	if _, started := value.(event.CompactionStarted); started && !p.failed {
		p.failed = true
		err := p.err
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	return p.recordingPublisher.PublishEventChecked(ctx, value)
}

type fatalCompactionPublicationError struct{ cause error }

func (e *fatalCompactionPublicationError) Error() string {
	return "test: fatal publication: " + e.cause.Error()
}
func (e *fatalCompactionPublicationError) Unwrap() error { return e.cause }
func (*fatalCompactionPublicationError) FatalPublication() bool {
	return true
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
				awaitCompactionWaiterRejection(t, rec, uuid.UUID{1}, event.CompactRejectInterrupted)
			},
		},
		{
			name: "shutdown outranks pending running request",
			run: func(t *testing.T) {
				l, rec, _, sessionID, loopID := newCompactionTestLoop(t, &fakeLLM{blockUntilCancel: true})
				startTurn(t, l, rec, textBlocks("run"))
				sendCompact(t, l, sessionID, loopID, uuid.UUID{1}, identity.AgencyUser)
				syncLoopActor(t, l)
				ack := make(chan error, 1)
				l.Commands <- command.Shutdown{Header: command.Header{CommandID: uuid.UUID{3}}, Ack: ack}
				awaitCompactionWaiterRejection(t, rec, uuid.UUID{1}, event.CompactRejectShuttingDown)
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

func TestLoopPreStartRejectionPublishesCanonicalWaiterOrder(t *testing.T) {
	tests := []struct{ name string }{{name: "coalesced waiters retain canonical order and deterministic ids"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, rec, sink, sessionID, loopID := newCompactionTestLoop(t, &fakeLLM{blockUntilCancel: true})
			startTurn(t, l, rec, textBlocks("run"))
			base := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
			requests := []command.Compact{
				compactCommand(uuid.UUID{3}, base.Add(2*time.Second), identity.AgencyUser),
				compactCommand(uuid.UUID{1}, base, identity.AgencyUser),
				compactCommand(uuid.UUID{2}, base.Add(time.Second), identity.AgencyUser),
			}
			for i := range requests {
				requests[i].Coordinates = identity.Coordinates{SessionID: sessionID, LoopID: loopID}
				l.Commands <- requests[i]
			}
			syncLoopActor(t, l)
			ack := make(chan bool, 1)
			l.Commands <- command.Interrupt{Header: command.Header{CommandID: uuid.UUID{4}}, Ack: ack}
			if !<-ack {
				t.Fatal("Interrupt did not cancel active turn")
			}
			blockUntilEvents(t, rec, func(events []event.Event) bool {
				count := 0
				for _, published := range events {
					if _, ok := published.(event.CompactWaiterRejected); ok {
						count++
					}
				}
				return count == len(requests)
			})
			var waiters []event.CompactWaiterRejected
			for _, published := range rec.events() {
				if waiter, ok := published.(event.CompactWaiterRejected); ok {
					waiters = append(waiters, waiter)
				}
			}
			wantOrder := []uuid.UUID{{1}, {2}, {3}}
			if len(waiters) != len(wantOrder) {
				t.Fatalf("waiters = %d, want %d", len(waiters), len(wantOrder))
			}
			attemptID := waiters[0].AttemptID
			for index, waiter := range waiters {
				if waiter.Cause.CommandID != wantOrder[index] || waiter.AttemptID != attemptID || waiter.Reason != event.CompactRejectInterrupted {
					t.Fatalf("waiter[%d] = %+v, want command %v shared Interrupted attempt", index, waiter, wantOrder[index])
				}
				if waiter.EventID != event.CompactWaiterReplyID(attemptID, wantOrder[index], false) {
					t.Fatalf("waiter[%d] EventID = %v, want deterministic reply id", index, waiter.EventID)
				}
			}
			if dispositions, _ := sink.snapshot(); len(dispositions) != 0 {
				t.Fatalf("sink dispositions = %v, want actor-owned projection", dispositions)
			}
		})
	}
}

func TestLoopPreStartRejectionCheckedFailureReportsInfrastructure(t *testing.T) {
	tests := []struct{ name string }{{name: "failed waiter append reports without false outcome"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, rec, sink, sessionID, loopID := newCompactionTestLoop(t, &fakeLLM{blockUntilCancel: true})
			startTurn(t, l, rec, textBlocks("run"))
			sendCompact(t, l, sessionID, loopID, uuid.UUID{1}, identity.AgencyUser)
			syncLoopActor(t, l)
			appendErr := errors.New("waiter append failed")
			rec.setCheckedError(appendErr)
			ack := make(chan bool, 1)
			l.Commands <- command.Interrupt{Header: command.Header{CommandID: uuid.UUID{2}}, Ack: ack}
			if !<-ack {
				t.Fatal("Interrupt did not cancel active turn")
			}
			select {
			case <-sink.notify:
			case <-time.After(2 * time.Second):
				t.Fatal("waiter append failure was not reported")
			}
			_, failures := sink.snapshot()
			if len(failures) != 1 || !equalUUIDs(failures[0].WaiterCommandIDs, []uuid.UUID{{1}}) {
				t.Fatalf("failures = %+v, want exact waiter ownership", failures)
			}
			var coordinationErr *CompactionCoordinationError
			if !errors.As(failures[0].Err, &coordinationErr) || coordinationErr.Kind != CompactionCoordinationOutcome || !errors.Is(failures[0].Err, appendErr) {
				t.Fatalf("failure = %T %v, want typed outcome wrapping append error", failures[0].Err, failures[0].Err)
			}
			for _, published := range rec.events() {
				switch published.(type) {
				case event.CompactWaiterRejected, event.CompactionRejected:
					t.Fatalf("failed append published false outcome %T", published)
				}
			}
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

			awaitCompactionWaiterRejection(t, rec, uuid.UUID{1}, tt.wantReject)
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
			if len(dispositions) != 0 {
				t.Fatalf("dispositions = %v, want actor-owned waiter rejection and no sink call", dispositions)
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
			for i := 0; i < compactionControlWaiterCapacity; i++ {
				sendCompact(t, l, sessionID, loopID, uuid.UUID{byte(i + 1)}, identity.AgencyMachine)
			}
			disposition := awaitCompactionDisposition(t, sink)
			rec.setCheckedError(tt.checkedErr)
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

func TestLoopCompactionStartPublicationFailureDoesNotInvokeExecutor(t *testing.T) {
	t.Parallel()
	startErr := errors.New("start publication failed")
	tests := []struct {
		name           string
		publicationErr error
		wantTerminal   bool
		wantFailure    bool
	}{
		{name: "recoverable progress failure finalizes canonical rejection", publicationErr: startErr, wantTerminal: true},
		{name: "fatal journal failure reports infrastructure without false terminal", publicationErr: &fatalCompactionPublicationError{cause: startErr}, wantFailure: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			sessionID, loopID := mustID(t), mustID(t)
			recorder := &recordingPublisher{}
			publisher := &oneShotCompactionStartFailurePublisher{recordingPublisher: recorder, err: tt.publicationErr}
			sink := newRecordingCompactionSink()
			basis := event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{0xc1}}
			actor, err := newLoopWithSeed(ctx, sessionID, loopID, Provenance{}, publisher, runtimeConfig{
				Client: &fakeLLM{}, Model: testModel(), DrainTimeout: 200 * time.Millisecond, compactionSink: sink,
			}, nil, "", &RestoredState{Basis: basis, HasBasis: true})
			if err != nil {
				t.Fatalf("newLoopWithSeed() error = %v", err)
			}

			commandID := uuid.UUID{1}
			sendCompact(t, actor, sessionID, loopID, commandID, identity.AgencyUser)
			syncLoopActor(t, actor)

			dispositions, failures := sink.snapshot()
			if len(dispositions) != 0 {
				t.Fatalf("executor invocations = %d, want zero before successful CompactionStarted", len(dispositions))
			}
			if (len(failures) == 1) != tt.wantFailure {
				t.Fatalf("infrastructure failures = %d, wantFailure %v", len(failures), tt.wantFailure)
			}
			if tt.wantFailure {
				var coordinationErr *CompactionCoordinationError
				if !errors.As(failures[0].Err, &coordinationErr) || coordinationErr.Kind != CompactionCoordinationOutcome || !errors.Is(failures[0].Err, startErr) {
					t.Fatalf("failure = %T %v, want typed coordination failure wrapping start error", failures[0].Err, failures[0].Err)
				}
			}
			var terminalCount, waiterCount int
			for _, published := range recorder.events() {
				switch value := published.(type) {
				case event.CompactionStarted:
					t.Fatal("failed CompactionStarted was falsely published")
				case event.CompactionRejected:
					terminalCount++
					if value.RejectReason != event.CompactRejectProgressPublication {
						t.Errorf("reject reason = %v, want ProgressPublication", value.RejectReason)
					}
				case event.CompactWaiterRejected:
					waiterCount++
				}
			}
			if tt.wantTerminal {
				if terminalCount != 1 || waiterCount != 1 {
					t.Fatalf("terminal/waiter counts = %d/%d, want exactly 1/1", terminalCount, waiterCount)
				}
			} else if terminalCount != 0 || waiterCount != 0 {
				t.Fatalf("false terminal/waiter counts = %d/%d", terminalCount, waiterCount)
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

func TestLoopCompactionDirectErrorFinalizesAllWaitersAndClearsAttempt(t *testing.T) {
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
			blockUntilEvents(t, rec, func(events []event.Event) bool {
				for _, published := range events {
					if _, ok := published.(event.CompactionRejected); ok {
						return true
					}
				}
				return false
			})
			_, failures := sink.snapshot()
			if len(failures) != 0 {
				t.Fatalf("failure notifications = %d, want canonical rejection instead", len(failures))
			}
			var rejection *event.CompactionRejected
			var waiterRejects int
			for _, published := range rec.events() {
				switch value := published.(type) {
				case event.CompactionRejected:
					copyOfValue := value
					rejection = &copyOfValue
				case event.CompactWaiterRejected:
					waiterRejects++
				}
			}
			if rejection == nil || rejection.RejectReason != event.CompactRejectExecutionFailed || !equalUUIDs(rejection.WaiterCommandIDs, tt.wantWaiters) {
				t.Fatalf("rejection = %+v, want execution-failed canonical waiter set %v", rejection, tt.wantWaiters)
			}
			if waiterRejects != len(tt.wantWaiters) {
				t.Fatalf("waiter rejections = %d, want %d", waiterRejects, len(tt.wantWaiters))
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
	basis := event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{0xb1}}
	l, err := newLoopWithSeed(ctx, sessionID, loopID, Provenance{}, rec, runtimeConfig{
		Client: client, Model: testModel(), Tools: tools, DrainTimeout: 200 * time.Millisecond, compactionSink: sink,
	}, nil, "", &RestoredState{Basis: basis, HasBasis: true})
	if err != nil {
		t.Fatalf("newLoopWithSeed: %v", err)
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

func awaitCompactionWaiterRejection(
	t *testing.T,
	recorder *recordingPublisher,
	commandID uuid.UUID,
	reason event.CompactRejectReason,
) event.CompactWaiterRejected {
	t.Helper()
	var got event.CompactWaiterRejected
	blockUntilEvents(t, recorder, func(events []event.Event) bool {
		for _, published := range events {
			waiter, ok := published.(event.CompactWaiterRejected)
			if ok && waiter.Cause.CommandID == commandID && waiter.Reason == reason {
				got = waiter
				return true
			}
		}
		return false
	})
	return got
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
