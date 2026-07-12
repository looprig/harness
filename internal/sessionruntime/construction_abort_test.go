package sessionruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
)

type abortBackend struct {
	commands chan command.Command
	done     chan struct{}
}

func (b *abortBackend) CommandSink() chan<- command.Command { return b.commands }
func (b *abortBackend) DoneChan() <-chan struct{}           { return b.done }
func (b *abortBackend) Snapshot(context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	return nil, 0, nil
}

func TestAbortConstructionStopsCollaboratorsWithoutSessionStopped(t *testing.T) {
	sid, _ := uuid.New()
	lid, _ := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())
	backend := &abortBackend{commands: make(chan command.Command), done: make(chan struct{})}
	go func() {
		<-ctx.Done()
		close(backend.done)
	}()
	appender := &recordingEventAppender{}
	h := hub.New(sid, hub.WithAppender(appender))
	sub, err := h.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	var timerFired atomic.Bool
	timer := time.AfterFunc(100*time.Millisecond, func() { timerFired.Store(true) })
	s := &Session{
		sessionID: sid, sessionCtx: ctx, sessionCancel: cancel, hub: h,
		checkpointAdmission: newCheckpointAdmissionGate(),
		loops:               map[uuid.UUID]*loopHandle{lid: {id: lid, backend: backend, cancel: cancel}},
		gates:               map[gate.ID]gateEntry{}, gateTimers: map[gate.ID]*time.Timer{gate.ID(lid): timer},
	}
	s.abortConstruction(errors.New("construction failed"))

	select {
	case <-backend.done:
	default:
		t.Fatal("abort returned before loop joined")
	}
	if len(s.gateTimers) != 0 {
		t.Fatalf("gate timers remain: %d", len(s.gateTimers))
	}
	time.Sleep(120 * time.Millisecond)
	if timerFired.Load() {
		t.Fatal("gate timer callback survived abort")
	}
	if _, ok := <-sub.Events(); ok {
		t.Fatal("hub subscription remained open")
	}
	for _, appended := range appender.snapshot() {
		if _, stopped := appended.(event.SessionStopped); stopped {
			t.Fatal("construction abort appended SessionStopped")
		}
	}
}
