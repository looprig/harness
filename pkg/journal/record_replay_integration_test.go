//go:build integration

package journal_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// drainRecords opens a cold (Follow:false) RecordCursor for req and reads every record
// until io.EOF, returning the decoded JournalRecords paired with their stream sequences
// in delivery order. A non-EOF error fails the test fatally — fail-secure paths are
// asserted by the caller's own Next loop, never swallowed here. It is the RecordReplayer
// analogue of drainAll (which is event-only).
func drainRecords(t *testing.T, r journal.RecordReplayer, req journal.ReplayRequest) ([]journal.JournalRecord, []uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cur, err := r.Open(ctx, req)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := cur.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	var recs []journal.JournalRecord
	var seqs []uint64
	for {
		rec, seq, err := cur.Next(ctx)
		if errors.Is(err, io.EOF) {
			return recs, seqs
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		recs = append(recs, rec)
		seqs = append(seqs, seq)
	}
}

// appendCommand wraps cmd in a CommandRecord targeting loop lid in session sid and
// appends it through the real journal, returning the assigned sequence.
func appendCommand(t *testing.T, j journal.SessionJournal, ctx context.Context, sid, lid uuid.UUID, cmd command.Command) uint64 {
	t.Helper()
	seq, err := j.Append(ctx, journal.NewCommandRecord(sid, lid, cmd))
	if err != nil {
		t.Fatalf("Append(command %T): %v", cmd, err)
	}
	return seq
}

// TestRecordReplayerYieldsEventsAndCommandsInOrder is the core Task 10 assertion and the
// property the EventReplayer LACKS: a session journal holding events AND a user command
// (a gate decision) on different subjects is drained by a cold RecordReplayer and yields
// BOTH the EventRecords and the CommandRecord, interleaved in stream-sequence order. An
// oversized event is offloaded to the object store on append and must rehydrate
// byte-for-byte. The opening LeaseFence is surfaced as a FenceRecord (fences are not
// filtered out). A cross-check over the same stream with the event-only EventReplayer
// proves the command is invisible there but present here.
func TestRecordReplayerYieldsEventsAndCommandsInOrder(t *testing.T) {
	sid := seedUUID(0xA0)
	lid := seedUUID(0xA1)
	tid := seedUUID(0xA2)
	stepID := seedUUID(0xA3)
	toolExecID := seedUUID(0xA4)

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid))
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sessionStarted := event.SessionStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid},
			EventID:     seedUUID(0xB0),
		},
	}
	loopStarted := event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
			EventID:     seedUUID(0xB1),
		},
	}
	turnStarted := event.TurnStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid},
			EventID:     seedUUID(0xB2),
		},
		TurnIndex: 1,
		Message: &content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: "hi"}},
		}},
	}
	// > 512 KiB once marshaled: forced down the offload path on append, must rehydrate.
	const blockChars = 700 * 1024
	bigStep := event.StepDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID},
			EventID:     seedUUID(0xB3),
		},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: strings.Repeat("x", blockChars)}},
			}},
		},
	}
	// A gate EVENT and the user's resolving COMMAND, on different subjects. The
	// EventReplayer would drop the command; the RecordReplayer must surface it.
	permRequested := event.PermissionRequested{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID},
			EventID:     seedUUID(0xB4),
		},
		ToolExecutionID: toolExecID,
	}
	turnDone := event.TurnDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid},
			EventID:     seedUUID(0xB5),
		},
		TurnIndex: 1,
		Message: &content.AIMessage{Message: content.Message{
			Role:   content.RoleAssistant,
			Blocks: []content.Block{&content.TextBlock{Text: "done"}},
		}},
	}
	approve := command.ApproveToolCall{
		Header: command.Header{CommandID: seedUUID(0xC0), Agency: identity.AgencyUser},
		GateRoute: command.GateRoute{
			Coordinates:     identity.Coordinates{SessionID: sid, LoopID: lid},
			ToolExecutionID: toolExecID,
		},
		Scope: tool.ScopeSession,
	}

	// Append the known sequence. The opening LeaseFence already sits at seq 1; these land
	// at seq 2..8 with the command (seq 7) BETWEEN the gate event (6) and TurnDone (8).
	wantEvents := []event.Event{sessionStarted, loopStarted, turnStarted, bigStep, permRequested}
	for _, ev := range wantEvents {
		appendEvent(t, j, ctx, ev)
	}
	appendCommand(t, j, ctx, sid, lid, approve)
	appendEvent(t, j, ctx, turnDone)
	wantEvents = append(wantEvents, turnDone)

	store := mustObjectStore(t, js, sid)
	r := journal.NewRecordReplayer(js, store)
	recs, seqs := drainRecords(t, r, journal.ReplayRequest{SessionID: sid, From: journal.Beginning(), Follow: false})

	// Stream-sequence order: strictly increasing across every surfaced record.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("seq[%d]=%d not strictly greater than seq[%d]=%d", i, seqs[i], i-1, seqs[i-1])
		}
	}

	// Classify the surfaced records by variant.
	var gotEvents []event.Event
	var gotCommands []command.Command
	var gotCommandRec journal.CommandRecord
	var fenceCount int
	cmdIdx := -1
	for i, rec := range recs {
		switch v := rec.(type) {
		case journal.EventRecord:
			gotEvents = append(gotEvents, v.Event())
		case journal.CommandRecord:
			gotCommands = append(gotCommands, v.Command())
			gotCommandRec = v
			cmdIdx = i
		case journal.FenceRecord:
			fenceCount++
		default:
			t.Fatalf("record[%d] unexpected variant %T", i, rec)
		}
	}

	// The EventReplayer-lacked property: the user command is surfaced, exactly once.
	if len(gotCommands) != 1 {
		t.Fatalf("RecordReplayer surfaced %d commands, want exactly 1 (the ApproveToolCall)", len(gotCommands))
	}
	// The CommandRecord routes by the sid/lid recovered FROM ITS SUBJECT (decode's
	// ParseSubject path), not from the command body — so its Subject() must be exactly the
	// loop command subject the writer placed it on. This pins the routing-recovery path.
	if got, want := gotCommandRec.Subject(), journal.LoopCommandSubject(sid, lid); got != want {
		t.Errorf("CommandRecord.Subject() = %q, want %q", got, want)
	}
	gotApprove, ok := gotCommands[0].(command.ApproveToolCall)
	if !ok {
		t.Fatalf("surfaced command = %T, want command.ApproveToolCall", gotCommands[0])
	}
	if gotApprove.GateRoute.ToolExecutionID != toolExecID {
		t.Errorf("ApproveToolCall.ToolExecutionID = %v, want %v", gotApprove.GateRoute.ToolExecutionID, toolExecID)
	}

	// All events surfaced, in append order — and the oversized bigStep deep-equals what
	// was appended, proving the object-store rehydration round-trips byte-for-byte.
	if !reflect.DeepEqual(gotEvents, wantEvents) {
		t.Fatalf("RecordReplayer events =\n%#v\nwant\n%#v", gotEvents, wantEvents)
	}

	// Interleaving in causal/append order: the command lands BETWEEN the gate event and
	// TurnDone, exactly as appended — the merged stream the transcript export needs.
	if cmdIdx <= 0 || cmdIdx+1 >= len(recs) {
		t.Fatalf("command at record index %d is not interleaved between events", cmdIdx)
	}
	if before, ok := recs[cmdIdx-1].(journal.EventRecord); !ok {
		t.Errorf("record before command = %T, want EventRecord (PermissionRequested)", recs[cmdIdx-1])
	} else if _, ok := before.Event().(event.PermissionRequested); !ok {
		t.Errorf("event before command = %T, want PermissionRequested", before.Event())
	}
	if after, ok := recs[cmdIdx+1].(journal.EventRecord); !ok {
		t.Errorf("record after command = %T, want EventRecord (TurnDone)", recs[cmdIdx+1])
	} else if _, ok := after.Event().(event.TurnDone); !ok {
		t.Errorf("event after command = %T, want TurnDone", after.Event())
	}

	// The opening LeaseFence is surfaced as a FenceRecord (fences are not filtered out).
	if fenceCount == 0 {
		t.Errorf("RecordReplayer surfaced no FenceRecord; expected the opening LeaseFence")
	}

	// Cross-check the EventReplayer-lacked property directly: the event-only replayer over
	// the SAME stream yields the same events and CANNOT surface the command at all.
	er := journal.NewEventReplayer(js, store)
	erEvents, _ := drainAll(t, er, journal.ReplayRequest{SessionID: sid, From: journal.Beginning(), Follow: false})
	if !reflect.DeepEqual(erEvents, wantEvents) {
		t.Fatalf("EventReplayer events =\n%#v\nwant\n%#v", erEvents, wantEvents)
	}
}

// TestRecordReplayerFailSecureMissingObject pins the fail-secure rehydration path for the
// record replayer: a dangling offload pointer (its backing object deleted) surfaces a
// typed *ObjectMissingError rather than a skipped or zero-valued record.
func TestRecordReplayerFailSecureMissingObject(t *testing.T) {
	sid := seedUUID(0xA8)
	lid := seedUUID(0xA9)

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid, mustAcquireLease(t, js, sid))
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	appendEvent(t, j, ctx, largeStepDone(sid, lid, 0xAA, 700*1024))

	store := mustObjectStore(t, js, sid)
	objID := onlyObjectID(t, store)
	if err := store.Delete(objID); err != nil {
		t.Fatalf("Delete(%s): %v", objID, err)
	}

	r := journal.NewRecordReplayer(js, store)
	cur, err := r.Open(ctx, journal.ReplayRequest{SessionID: sid, From: journal.Beginning()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := cur.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// Drain until the offloaded record is reached; it must fail secure, never EOF/skip.
	var sawMissing bool
	for {
		_, _, err := cur.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		var missing *journal.ObjectMissingError
		if errors.As(err, &missing) {
			if missing.ObjectID != objID {
				t.Errorf("ObjectMissingError.ObjectID = %q, want %q", missing.ObjectID, objID)
			}
			sawMissing = true
			break
		}
		if err != nil {
			t.Fatalf("Next: unexpected error %v", err)
		}
	}
	if !sawMissing {
		t.Fatalf("dangling offload pointer was not surfaced as *ObjectMissingError (silent skip?)")
	}
}
