package sessionruntime

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
)

type turnStartCheckedAppender struct {
	mu              sync.Mutex
	events          []event.Event
	failTurnStarted bool
	err             error
}

func (a *turnStartCheckedAppender) AppendEvent(_ context.Context, value event.Event) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, started := value.(event.TurnStarted); started && a.failTurnStarted {
		return 0, a.err
	}
	a.events = append(a.events, value)
	return uint64(len(a.events)), nil
}

func (a *turnStartCheckedAppender) snapshot() []event.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]event.Event(nil), a.events...)
}

func waitTurnStartOutcome(t *testing.T, appender *turnStartCheckedAppender) []event.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events := appender.snapshot()
		for _, value := range events {
			switch value.(type) {
			case event.TurnRejected, event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return events
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("turn start produced no terminal input outcome")
	return nil
}

func TestSessionTurnStartReservationUsesCheckedPublication(t *testing.T) {
	t.Parallel()
	publishErr := errors.New("TurnStarted durable append failed")
	tests := []struct {
		name            string
		failTurnStarted bool
		wantRejected    bool
		wantTurnStarted int
		wantStepDone    int
		wantTurnDone    int
		wantMessages    int
		wantTurnIndex   event.TurnIndex
	}{
		{name: "checked failure rejects without live or durable turn", failTurnStarted: true, wantRejected: true},
		{name: "successful reservation publishes and runs once", wantTurnStarted: 1, wantStepDone: 1, wantTurnDone: 1, wantMessages: 2, wantTurnIndex: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appender := &turnStartCheckedAppender{failTurnStarted: tt.failTurnStarted, err: publishErr}
			session, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("answer")}}), WithEventAppender(appender))
			if err != nil {
				t.Fatalf("newTestSession() error = %v", err)
			}
			t.Cleanup(func() { _ = session.Shutdown(context.Background()) })
			inputID, err := session.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "start"}})
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			events := waitTurnStartOutcome(t, appender)
			turnStarted, stepDone, turnDone, laterFailure := 0, 0, 0, 0
			var rejected *event.TurnRejected
			for _, value := range events {
				switch typed := value.(type) {
				case event.TurnStarted:
					turnStarted++
				case event.StepDone:
					stepDone++
				case event.TurnDone:
					turnDone++
				case event.TurnFailed, event.TurnInterrupted:
					laterFailure++
				case event.TurnRejected:
					if typed.Cause.CommandID == inputID {
						copyOfRejected := typed
						rejected = &copyOfRejected
					}
				}
			}
			if turnStarted != tt.wantTurnStarted || stepDone != tt.wantStepDone || turnDone != tt.wantTurnDone || laterFailure != 0 {
				t.Fatalf("TurnStarted/StepDone/TurnDone/later failure = %d/%d/%d/%d, want %d/%d/%d/0", turnStarted, stepDone, turnDone, laterFailure, tt.wantTurnStarted, tt.wantStepDone, tt.wantTurnDone)
			}
			if (rejected != nil) != tt.wantRejected {
				t.Fatalf("TurnRejected present = %v, want %v", rejected != nil, tt.wantRejected)
			}
			if rejected != nil && rejected.Reason != event.RejectInternal {
				t.Fatalf("TurnRejected reason = %v, want RejectInternal", rejected.Reason)
			}
			handle, ok := session.loopFor(session.ActiveLoopID())
			if !ok {
				t.Fatal("primary loop missing")
			}
			messages, turnIndex, err := handle.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if len(messages) != tt.wantMessages || turnIndex != tt.wantTurnIndex {
				t.Fatalf("live messages/turn index = %d/%d, want %d/%d", len(messages), turnIndex, tt.wantMessages, tt.wantTurnIndex)
			}
			folded := foldLoop(events)
			if folded.Err != nil {
				t.Fatalf("foldLoop() error = %v", folded.Err)
			}
			if !reflect.DeepEqual(messages, folded.Msgs) || turnIndex != folded.TurnIndex {
				t.Fatalf("live snapshot = %#v/%d, restored = %#v/%d", messages, turnIndex, folded.Msgs, folded.TurnIndex)
			}
			if tt.failTurnStarted {
				if folded.HasBasis {
					t.Fatalf("restored basis = %+v, want absent after rejected turn start", folded.Basis)
				}
				if !errors.Is(session.faultIfFaulted(), publishErr) {
					t.Fatalf("session fault = %v, want TurnStarted append failure", session.faultIfFaulted())
				}
			}
			reserved := make(chan error, 1)
			go func() {
				reservation, reserveErr := session.hub.ReserveTurnStart(mustUUID())
				if reservation != nil {
					reservation.Release()
				}
				reserved <- reserveErr
			}()
			select {
			case reserveErr := <-reserved:
				if reserveErr != nil {
					t.Fatalf("reservation after outcome = %v", reserveErr)
				}
			case <-time.After(time.Second):
				t.Fatal("turn-start reservation ownership was not released")
			}
		})
	}
}
