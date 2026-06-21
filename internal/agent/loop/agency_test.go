package loop

import (
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
)

// TestTurnStartedCopiesCommandAgency proves the loop copies the submit command's
// Header.Agency onto the resulting event.TurnStarted's Cause.Agency. An event has no
// agency of its own; it surfaces "who started this turn" through Cause.Agency, which
// is a copy of the originating command's authoritative Header.Agency. Both the user
// case (a human-stamped UserInput) and the machine case (the zero default) must
// round-trip unchanged. A SubagentResult hand-back is always machine (zero default).
func TestTurnStartedCopiesCommandAgency(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		send       func(t *testing.T, l *Loop, id [16]byte)
		wantAgency identity.Agency
	}{
		{
			name: "user UserInput -> Cause.Agency AgencyUser",
			send: func(t *testing.T, l *Loop, id [16]byte) {
				l.Commands <- command.UserInput{
					Header: command.Header{CommandID: id, Agency: identity.AgencyUser},
					Blocks: textBlocks("hello"),
				}
			},
			wantAgency: identity.AgencyUser,
		},
		{
			name: "machine UserInput (zero default) -> Cause.Agency AgencyMachine",
			send: func(t *testing.T, l *Loop, id [16]byte) {
				l.Commands <- command.UserInput{
					Header: command.Header{CommandID: id}, // Agency unset = AgencyMachine
					Blocks: textBlocks("hello"),
				}
			},
			wantAgency: identity.AgencyMachine,
		},
		{
			name: "SubagentResult hand-back -> Cause.Agency AgencyMachine",
			send: func(t *testing.T, l *Loop, id [16]byte) {
				l.Commands <- command.SubagentResult{
					Coordinates: identity.Coordinates{LoopID: mustID(t)}, // PARENT delivery target
					Header: command.Header{
						CommandID: id,                                                                   // hand-back is machine (Agency unset)
						Cause:     identity.Cause{Coordinates: identity.Coordinates{LoopID: mustID(t)}}, // CHILD
					},
					Blocks: textBlocks("subagent output"),
				}
			},
			wantAgency: identity.AgencyMachine,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, rec, _ := newLoopRec(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi")}})
			inputID := mustID(t)
			tt.send(t, l, inputID)

			ev := awaitReply(t, rec, inputID)
			started, ok := ev.(event.TurnStarted)
			if !ok {
				t.Fatalf("outcome = %T, want event.TurnStarted", ev)
			}
			if started.Cause.Agency != tt.wantAgency {
				t.Errorf("TurnStarted.Cause.Agency = %v, want %v", started.Cause.Agency, tt.wantAgency)
			}
		})
	}
}

// TestTurnFoldedIntoCopiesCommandAgency proves a queued submit that folds at a
// tool-continuation boundary carries its command's Header.Agency onto the resulting
// event.TurnFoldedInto's Cause.Agency. The agency must survive the queue + drain
// handshake, exactly like Cause.CommandID and Cause.LoopID do.
func TestTurnFoldedIntoCopiesCommandAgency(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		agency     identity.Agency
		wantAgency identity.Agency
	}{
		{name: "user fold -> Cause.Agency AgencyUser", agency: identity.AgencyUser, wantAgency: identity.AgencyUser},
		{name: "machine fold -> Cause.Agency AgencyMachine", agency: identity.AgencyMachine, wantAgency: identity.AgencyMachine},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bt := newBlockingTool()
			ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
			client := &scriptedLLM{scripts: [][]content.Chunk{
				{toolUseChunk(0, "id-1", "Block", `{}`)}, // step 0: blocking tool
				{textChunk("final")},                     // step 1: text -> TurnDone
			}}
			l, rec := newFoldLoop(t, client, ts)

			startTurn(t, l, rec, textBlocks("turn1"))
			<-bt.started

			foldedID := mustID(t)
			l.Commands <- command.UserInput{
				Header: command.Header{CommandID: foldedID, Agency: tt.agency},
				Blocks: textBlocks("folded"),
			}
			if _, ok := awaitReply(t, rec, foldedID).(event.InputQueued); !ok {
				t.Fatal("queued input not queued")
			}

			close(bt.release)

			blockUntilEvents(t, rec, func(evs []event.Event) bool {
				for _, e := range evs {
					if fi, ok := e.(event.TurnFoldedInto); ok && fi.Cause.CommandID == foldedID {
						if fi.Cause.Agency != tt.wantAgency {
							t.Errorf("TurnFoldedInto.Cause.Agency = %v, want %v", fi.Cause.Agency, tt.wantAgency)
						}
						return true
					}
				}
				return false
			})
		})
	}
}

// TestInputCancelledCopiesCommandAgency proves a still-queued submit that is
// client-retracted before it starts carries its command's Header.Agency onto the
// resulting event.InputCancelled's Cause.Agency.
func TestInputCancelledCopiesCommandAgency(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		agency     identity.Agency
		wantAgency identity.Agency
	}{
		{name: "user cancel -> Cause.Agency AgencyUser", agency: identity.AgencyUser, wantAgency: identity.AgencyUser},
		{name: "machine cancel -> Cause.Agency AgencyMachine", agency: identity.AgencyMachine, wantAgency: identity.AgencyMachine},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// blockUntilCancel keeps the first turn running so the second submit queues
			// and can then be retracted while still queued.
			l, rec, _ := newLoopRec(t, &fakeLLM{blockUntilCancel: true})
			startTurn(t, l, rec, nil) // occupies the loop

			queuedID := mustID(t)
			l.Commands <- command.UserInput{
				Header: command.Header{CommandID: queuedID, Agency: tt.agency},
				Blocks: textBlocks("queued"),
			}
			if _, ok := awaitReply(t, rec, queuedID).(event.InputQueued); !ok {
				t.Fatal("queued input not queued")
			}

			l.Commands <- command.CancelQueuedInput{
				Header:          command.Header{CommandID: mustID(t)},
				TargetCommandID: queuedID,
			}

			blockUntilEvents(t, rec, func(evs []event.Event) bool {
				for _, e := range evs {
					if ic, ok := e.(event.InputCancelled); ok && ic.Cause.CommandID == queuedID {
						if ic.Cause.Agency != tt.wantAgency {
							t.Errorf("InputCancelled.Cause.Agency = %v, want %v", ic.Cause.Agency, tt.wantAgency)
						}
						return true
					}
				}
				return false
			})
		})
	}
}
