package sessionruntime

import (
	"context"
	"errors"
	"io"
	"reflect"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
)

type restoredGatePlan struct {
	open        map[gate.ID]gateEntry
	unavailable []event.GateOpened
	// resume holds the private step snapshot of every open gate that carries a
	// valid one (event.GatePrepared.Resume), keyed by gate id.
	resume map[gate.ID]*event.ToolStepResume
	// opened keeps the GateOpened of every entry in open, so a gate that turns out
	// not to be resumable can still be closed restore_unavailable.
	opened map[gate.ID]event.GateOpened
	// order is the open gates' activation order.
	order []gate.ID
}

type restoredPreparedGate struct {
	prepared event.GatePrepared
	payload  gate.Payload
}

// drainRecordReplay opens a cold record cursor for req and reads it to io.EOF,
// preserving ledger order. Restore uses record replay, not event replay, because
// the private journal.GatePreparedRecord is intentionally invisible to event
// consumers but is required to recover gate state fail-securely.
//
// It returns each record's JOURNAL SEQUENCE alongside it, index-aligned. The sequence is
// not recoverable from a record — an event payload cannot carry the number the append
// that encodes it assigns — and the workspace-residency fold needs it to report which
// checkpoint a restored tree came from.
func drainRecordReplay(ctx context.Context, replayer journal.RecordReplayer, req journal.ReplayRequest) ([]journal.JournalRecord, []uint64, error) {
	cursor, err := replayer.Open(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = cursor.Close() }()

	var out []journal.JournalRecord
	var seqs []uint64
	for {
		rec, seq, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out, seqs, nil
		}
		if err != nil {
			return nil, nil, err
		}
		out = append(out, rec)
		seqs = append(seqs, seq)
	}
}

func eventsFromRecords(records []journal.JournalRecord, loopID uuid.UUID) []event.Event {
	out := make([]event.Event, 0, len(records))
	for _, rec := range records {
		evRec, ok := rec.(journal.EventRecord)
		if !ok {
			continue
		}
		ev := evRec.Event()
		if deliverRestoredEvent(ev, loopID) {
			out = append(out, ev)
		}
	}
	return out
}

func deliverRestoredEvent(ev event.Event, loopID uuid.UUID) bool {
	if loopID.IsZero() {
		return true
	}
	if ev.Scope() == event.ScopeSession {
		return true
	}
	return ev.EventHeader().LoopID == loopID
}

func foldRestoredGates(records []journal.JournalRecord) restoredGatePlan {
	prepared := make(map[gate.ID]restoredPreparedGate)
	opened := make(map[gate.ID]event.GateOpened)
	closed := make(map[gate.ID]bool)
	openOrder := make([]gate.ID, 0)

	for _, rec := range records {
		switch r := rec.(type) {
		case journal.GatePreparedRecord:
			preparedEvent := r.Prepared()
			id := preparedEvent.Gate.ID
			if id == (gate.ID{}) {
				id = r.Payload().GateID
			}
			if id == (gate.ID{}) {
				continue
			}
			prepared[id] = restoredPreparedGate{prepared: preparedEvent, payload: r.Payload().Payload}
		case journal.EventRecord:
			switch ev := r.Event().(type) {
			case event.GateOpened:
				id := ev.Gate.ID
				if id == (gate.ID{}) || closed[id] {
					continue
				}
				if _, seen := opened[id]; !seen {
					openOrder = append(openOrder, id)
				}
				opened[id] = ev
			case event.GateResolved:
				closed[ev.GateID] = true
				delete(opened, ev.GateID)
			}
		}
	}

	plan := restoredGatePlan{
		open:   make(map[gate.ID]gateEntry),
		resume: make(map[gate.ID]*event.ToolStepResume),
		opened: make(map[gate.ID]event.GateOpened),
	}
	for _, id := range openOrder {
		openedEvent, ok := opened[id]
		if !ok || closed[id] {
			continue
		}
		preparedGate, ok := prepared[id]
		var resume *event.ToolStepResume
		if ok && preparedGate.prepared.Resume.Valid() {
			resume = preparedGate.prepared.Resume
		}
		if !ok || !openedEvent.Gate.Restorable || !gateRestoreHookSupported(openedEvent.Gate, resume != nil) {
			plan.unavailable = append(plan.unavailable, openedEvent)
			continue
		}
		if resume != nil {
			plan.resume[id] = resume
		}
		plan.opened[id] = openedEvent
		plan.order = append(plan.order, id)
		coords := preparedGate.prepared.EventHeader().Coordinates
		if coords == (identity.Coordinates{}) {
			coords = openedEvent.EventHeader().Coordinates
		}
		plan.open[id] = gateEntry{
			gate:        openedEvent.Gate,
			route:       gate.Route{GateID: id, LoopID: gate.ID(openedEvent.EventHeader().LoopID), ToolExecutionID: openedEvent.Gate.Subject.ToolExecutionID},
			payload:     preparedGate.payload,
			coordinates: coords,
			state:       gateOpen,
		}
	}
	return plan
}

// gateRestoreHookSupported reports whether restore may reinstall an opened,
// unresolved gate as a live, answerable directory entry rather than closing
// it CloseRestoreUnavailable. An ask_user gate is supported only PROVISIONALLY and
// only with a resume snapshot: it is answerable solely through the call that asked
// it, so it stays open only if the restore actually resumes that call's step
// (planParkedStep); otherwise closeUnresumedAskUserGates closes it. KindPermission is supported (design §15: "the
// permission gate restores normally... the gate remains answerable by a
// human"): foldRestoredGates never restores reviewBasis (gateEntry's zero
// value) or any cancellation handle (reviewLifecycle always starts zero on a
// fresh Session — see session.go's review field), so a restored permission
// gate is structurally incapable of receiving a classifier-originated
// response until a NEW review starts, and design §15 additionally requires
// that no new review is ever started from restored/guessed context — restore
// never calls StartPermissionReview. Every other kind remains unsupported
// pending its own restore design.
func gateRestoreHookSupported(g gate.Gate, resumable bool) bool {
	switch g.Kind {
	case gate.KindPermission:
		return true
	case gate.KindAskUser:
		return resumable
	default:
		return false
	}
}

func cloneGateEntries(in map[gate.ID]gateEntry) map[gate.ID]gateEntry {
	out := make(map[gate.ID]gateEntry, len(in))
	for id, entry := range in {
		out[id] = entry
	}
	return out
}

func appendRestoreUnavailableGates(ctx context.Context, j journal.SessionJournal, factory *event.Factory, opened []event.GateOpened) error {
	for _, open := range opened {
		stamped, err := factory.Stamp(event.Header{Coordinates: open.EventHeader().Coordinates})
		if err != nil {
			return &RestoreError{Kind: RestoreIDGenerationFailed, Cause: err}
		}
		resolved := event.GateResolved{
			Header:   stamped,
			GateID:   open.Gate.ID,
			Resolver: open.Gate.Resolver,
			Reason:   gate.CloseRestoreUnavailable,
		}
		if _, err := j.Append(ctx, journal.NewEventRecord(resolved)); err != nil {
			return err
		}
	}
	return nil
}

// planParkedStep decides whether the active loop's open turn can be RESUMED at its
// in-flight tool step rather than interrupted, and returns the step when it can.
// Every condition fails toward the pre-resume behaviour (nil: the turn is
// interrupted and an ask_user gate closed), never toward a guess:
//
//   - the loop is native and its fold has an open turn whose own messages are
//     distinguishable from its base (no compaction inside it);
//   - at least one open gate is owned by that loop, and EVERY open gate the loop
//     owns carries a valid snapshot, belongs to the open turn and to ONE step, and
//     every snapshot names the same step (index and message);
//   - that step never committed (no StepDone for it).
func planParkedStep(plan restoredGatePlan, loopID uuid.UUID, native bool, folded foldResult, events []event.Event) *loopruntime.ParkedStep {
	if !native || !folded.OpenTurn || folded.OpenTurnStart < 0 || folded.OpenTurnID.IsZero() {
		return nil
	}
	var step *loopruntime.ParkedStep
	var stepID uuid.UUID
	var first *event.ToolStepResume
	for _, id := range plan.order {
		entry := plan.open[id]
		if entry.route.LoopID != loopID {
			continue
		}
		resume, ok := plan.resume[id]
		if !ok || entry.gate.Resolver != gate.ResolverLoop {
			return nil
		}
		if uuid.UUID(entry.gate.Subject.TurnID) != folded.OpenTurnID || entry.gate.Subject.StepID.IsZero() {
			return nil
		}
		if step == nil {
			stepID = uuid.UUID(entry.gate.Subject.StepID)
			first = resume
			step = &loopruntime.ParkedStep{
				TurnID:    folded.OpenTurnID,
				Cause:     folded.OpenTurnCause,
				TurnStart: folded.OpenTurnStart,
				StepID:    stepID,
				StepIndex: loopruntime.StepIndex(resume.StepIndex),
				Message:   resume.Message,
			}
		} else if uuid.UUID(entry.gate.Subject.StepID) != stepID || resume.StepIndex != first.StepIndex || !reflect.DeepEqual(resume.Message, first.Message) {
			return nil
		}
		parkedGate := loopruntime.ParkedGate{
			GateID:          id,
			Kind:            entry.gate.Kind,
			ToolExecutionID: uuid.UUID(entry.gate.Subject.ToolExecutionID),
			ToolUseID:       resume.ToolUseID,
		}
		if entry.gate.Kind == gate.KindPermission {
			payload, ok := permissionPayloadFromGatePayload(entry.payload)
			if !ok {
				return nil
			}
			request := payload.Request.Clone()
			parkedGate.PermissionRequest = &request
		}
		if entry.gate.Kind == gate.KindAskUser {
			ask, ok := askUserPayloadFromGatePayload(entry.payload)
			if !ok {
				return nil
			}
			parkedGate.AskUser = &ask
		}
		step.Gates = append(step.Gates, parkedGate)
	}
	if step == nil {
		return nil
	}
	for _, ev := range events {
		if done, ok := ev.(event.StepDone); ok && done.StepID == stepID {
			return nil
		}
	}
	return step
}

// closeUnresumedAskUserGates moves every open ask_user gate that is not part of
// parked into the unavailable set: it can only be answered through the call that
// asked it, and that call is not being resumed.
func closeUnresumedAskUserGates(plan *restoredGatePlan, parked *loopruntime.ParkedStep) {
	resumed := make(map[gate.ID]bool)
	if parked != nil {
		for _, g := range parked.Gates {
			resumed[g.GateID] = true
		}
	}
	kept := plan.order[:0]
	for _, id := range plan.order {
		entry := plan.open[id]
		if entry.gate.Kind == gate.KindAskUser && !resumed[id] {
			plan.unavailable = append(plan.unavailable, plan.opened[id])
			delete(plan.open, id)
			continue
		}
		kept = append(kept, id)
	}
	plan.order = kept
}

// parkedTurnStarter is the loop backend capability that releases a restored parked
// turn (loopruntime.Loop.StartParkedTurn).
type parkedTurnStarter interface {
	StartParkedTurn(ctx context.Context) error
}

// startParkedTurn releases loopID's resumed turn. A backend that cannot resume one
// was never handed a parked step, so there is nothing to release.
func (s *Session) startParkedTurn(ctx context.Context, loopID uuid.UUID) error {
	s.loopsMu.RLock()
	h := s.loops[loopID]
	s.loopsMu.RUnlock()
	if h == nil {
		return nil
	}
	starter, ok := h.backend.(parkedTurnStarter)
	if !ok {
		return nil
	}
	return starter.StartParkedTurn(ctx)
}

// askUserPayloadFromGatePayload narrows a restored gate's private payload to the
// question and choices an ask_user gate shows.
func askUserPayloadFromGatePayload(payload gate.Payload) (gate.AskUserPayload, bool) {
	switch v := payload.(type) {
	case gate.AskUserPayload:
		return gate.AskUserPayload{Question: v.Question, Choices: append([]string(nil), v.Choices...)}, true
	case *gate.AskUserPayload:
		if v == nil {
			return gate.AskUserPayload{}, false
		}
		return gate.AskUserPayload{Question: v.Question, Choices: append([]string(nil), v.Choices...)}, true
	default:
		return gate.AskUserPayload{}, false
	}
}
