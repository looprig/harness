package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
	durablestore "github.com/looprig/sessionstore"
)

// This file holds the two disposition arms v0.36.0 adds: create and restore.
//
// They are the arms that fix a WEDGED PRODUCT rather than a missing feature. Factory
// admits every session's first command as a create; Host claims it and durably begins
// an attempt; before this release the adapter refused the kind, so no disposition
// frame was ever written, the store could never settle the record, the consumer
// blocked at that command forever and no successor could close it either. The session
// existed and the agent was resident and the user could never talk to it.

func (f *runtimeCommandFixture) admittedCreate(id runtimecommand.CommandID, runtimeID uuid.UUID, text string) runtimecommand.Admitted {
	adm := runtimecommand.Admitted{
		CommandID:        id,
		RuntimeCommandID: runtimeID,
		Kind:             runtimecommand.KindCreate,
		LeaseEpoch:       f.lease.Epoch(),
		AttemptID:        "attempt/" + runtimecommand.AttemptID(id),
	}
	if text != "" {
		adm.Blocks = []content.Block{&content.TextBlock{Text: text}}
	}
	return adm
}

func (f *runtimeCommandFixture) admittedRestore(id runtimecommand.CommandID, runtimeID uuid.UUID) runtimecommand.Admitted {
	return runtimecommand.Admitted{
		CommandID:        id,
		RuntimeCommandID: runtimeID,
		Kind:             runtimecommand.KindRestore,
		LeaseEpoch:       f.lease.Epoch(),
		AttemptID:        "attempt/" + runtimecommand.AttemptID(id),
	}
}

// TestCreateCarryingAFirstMessageSendsItExactlyAsAnInput is the arm that carries the
// user's first words. A create's blocks are not a new dispatch path: they go to the
// session's active loop through the SAME send an input uses, carrying the admitted
// runtime id verbatim, so the first message's events correlate to the command that
// carried it.
func TestCreateCarryingAFirstMessageSendsItExactlyAsAnInput(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	runtimeID := mustUUID()
	adm := f.admittedCreate("v1:create-with-input", runtimeID, "hello from the create")

	disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	if disp.PrefixSequence == 0 {
		t.Fatalf("Disposition.PrefixSequence = 0, want the committed prefix")
	}
	sent, ok := f.drainOne(t).(command.UserInput)
	if !ok {
		t.Fatalf("a create carrying blocks dispatched something other than a UserInput")
	}
	if sent.Header.CommandID != runtimeID {
		t.Errorf("dispatched command id = %v, want the admitted runtime id %v", sent.Header.CommandID, runtimeID)
	}
	if len(sent.Blocks) != 1 {
		t.Fatalf("dispatched %d blocks, want the create's one", len(sent.Blocks))
	}
	text, ok := sent.Blocks[0].(*content.TextBlock)
	if !ok || text.Text != "hello from the create" {
		t.Errorf("dispatched block = %#v, want the create's first message", sent.Blocks[0])
	}

	got := onlyDisposition(t, f)
	want := runtimecommand.CommandDisposition{
		CommandID:           adm.CommandID,
		RuntimeCommandID:    runtimeID,
		Kind:                runtimecommand.KindCreate,
		LeaseEpoch:          f.lease.Epoch(),
		AttemptID:           adm.AttemptID,
		AttemptJournalEpoch: f.lease.Epoch(),
		Disposition:         runtimecommand.DispositionApplied,
	}
	if got != want {
		t.Fatalf("disposition = %+v, want %+v", got, want)
	}
}

// TestCreateAndRestoreWithNothingToSendStillSettleApplied is the pair of arms that
// look like they should be no_op and must not be.
//
// no_op is the vocabulary's "the runtime accepted this and it had no effect" — an
// interrupt of an idle session. A create and a restore are the opposite: the EFFECT
// is residency, and by the time either reaches this seam Host has already made the
// session resident, which is the whole reason the command was dispatched. Recording
// no_op would say the runtime found nothing to do about a session that now exists
// because of this command. applied is the narrow statement this protocol means by
// applied: under the attempt's grant the runtime durably recorded that it accepted
// the command into its execution path.
//
// Both settle the record either way — no_op settles applied — so this is not about
// the settlement outcome. It is about what the durable record SAYS, which is the only
// thing an operator reading a journal has.
func TestCreateAndRestoreWithNothingToSendStillSettleApplied(t *testing.T) {
	t.Parallel()
	for name, build := range map[string]struct {
		make func(f *runtimeCommandFixture, runtimeID uuid.UUID) runtimecommand.Admitted
		kind runtimecommand.Kind
	}{
		"create with no first message": {
			make: func(f *runtimeCommandFixture, id uuid.UUID) runtimecommand.Admitted {
				return f.admittedCreate("v1:bare-create", id, "")
			},
			kind: runtimecommand.KindCreate,
		},
		"restore": {
			make: func(f *runtimeCommandFixture, id uuid.UUID) runtimecommand.Admitted {
				return f.admittedRestore("v1:restore", id)
			},
			kind: runtimecommand.KindRestore,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRuntimeCommandFixture(t)
			runtimeID := mustUUID()
			adm := build.make(f, runtimeID)

			disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
			if err != nil {
				t.Fatalf("ApplyRuntimeCommand: %v", err)
			}
			if disp.PrefixSequence == 0 {
				t.Fatalf("Disposition.PrefixSequence = 0, want the committed prefix")
			}
			if disp.Interrupted {
				t.Errorf("Interrupted = true; only an interrupt reports one")
			}
			f.requireNoCommand(t, string(build.kind)+" with nothing to send")

			got := onlyDisposition(t, f)
			if got.Disposition != runtimecommand.DispositionApplied {
				t.Fatalf("disposition = %q, want applied", got.Disposition)
			}
			if got.Kind != build.kind {
				t.Errorf("command kind = %q, want %q", got.Kind, build.kind)
			}
			if got.AttemptID != adm.AttemptID {
				t.Errorf("attempt = %q, want %q", got.AttemptID, adm.AttemptID)
			}
			if got.LeaseEpoch != f.lease.Epoch() || got.AttemptJournalEpoch != f.lease.Epoch() {
				t.Errorf("grants = author %d attempt %d, want %d for both",
					got.LeaseEpoch, got.AttemptJournalEpoch, f.lease.Epoch())
			}
		})
	}
}

// TestCreateWhoseFirstMessageFailsAfterThePrefixSettlesRefused is the create's
// liveness arm, and it is exactly the input's: the loop exits in the window between
// the durable prefix and the send, so the effect fails under a STILL-LIVE lease.
// Without the refusal such a command has no terminal arm at all — not_applied needs a
// strictly later grant and a healthy Host never turns its lease over — and it sits
// applying forever, which is the very state this release exists to end.
func TestCreateWhoseFirstMessageFailsAfterThePrefixSettlesRefused(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	adm := f.admittedCreate("v1:create-effect-fails", mustUUID(), "hello")

	unread := make(chan command.Command)
	f.cmds = unread
	f.session.loops[f.session.activeLoopID].backend = &channelBackend{Commands: unread, Done: f.done}
	var exitOnce sync.Once
	f.session.runtimeCommands = dispositionAwareOrderLog{
		inner:   f.session.runtimeCommands.(dispositionLogAndRuntimeLog),
		observe: func() { exitOnce.Do(func() { close(f.done) }) },
	}

	stranded, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	var sessionErr *SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionLoopExited {
		t.Fatalf("ApplyRuntimeCommand err = %v, want *SessionError{SessionLoopExited}", err)
	}
	if stranded.PrefixSequence == 0 {
		t.Fatalf("the prefix did not commit, so the refusal arm was not reached")
	}
	got := onlyDisposition(t, f)
	if got.Disposition != runtimecommand.DispositionRefused {
		t.Fatalf("disposition = %q, want refused", got.Disposition)
	}
	if got.Kind != runtimecommand.KindCreate {
		t.Errorf("command kind = %q, want create", got.Kind)
	}
}

// TestACreateForAnExitedLoopIsRefusedBeforeThePrefix pins the preparation half: a
// create carrying blocks resolves its target loop BEFORE the prefix is written,
// exactly as an input does, so a refusal leaves nothing durable and the command may
// be re-offered. Writing the prefix first would strand it: every redelivery would
// deduplicate against a prefix for a message that was never sent.
func TestACreateForAnExitedLoopIsRefusedBeforeThePrefix(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	adm := f.admittedCreate("v1:create-loop-gone", mustUUID(), "hello")
	close(f.done)

	disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	var sessionErr *SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionLoopExited {
		t.Fatalf("ApplyRuntimeCommand err = %v, want *SessionError{SessionLoopExited}", err)
	}
	if disp != (runtimecommand.Disposition{}) {
		t.Fatalf("Disposition = %+v, want zero: nothing durable may have been written", disp)
	}
	if got := readDispositions(t, f); len(got) != 0 {
		t.Fatalf("the journal holds %d dispositions, want none: %+v", len(got), got)
	}
	// The proof that nothing was written: a later delivery is a FIRST delivery.
	f.session.loops[f.session.activeLoopID].backend = &channelBackend{Commands: admittingSink(t, f.cmds), Done: make(chan struct{})}
	again, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("re-delivery after a pre-prefix refusal: %v", err)
	}
	if again.Duplicate {
		t.Errorf("re-delivery Duplicate = true; the refused delivery must have written no prefix")
	}
	f.drainOne(t)
}

// TestARestoreIsNotRefusedByAnExitedLoop is the other side of the same coin, and it
// is why the preparation is gated on the PAYLOAD rather than on the kind. A restore
// sends nothing, so it needs no live loop and must not borrow the input path's
// refusals: a restore that failed because some loop had exited would be a resident
// session's resume turned into an unsettleable command for no reason.
func TestARestoreIsNotRefusedByAnExitedLoop(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	adm := f.admittedRestore("v1:restore-loop-gone", mustUUID())
	close(f.done)

	disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	if disp.PrefixSequence == 0 {
		t.Fatalf("Disposition.PrefixSequence = 0, want the committed prefix")
	}
	if got := onlyDisposition(t, f).Disposition; got != runtimecommand.DispositionApplied {
		t.Fatalf("disposition = %q, want applied", got)
	}
}

// TestADuplicateCreateDeliveryAppliesOnce holds the retry property for the new kinds.
// Host redelivers on any ambiguous answer, and a create redelivered after its prefix
// committed must not send the first message a second time.
func TestADuplicateCreateDeliveryAppliesOnce(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	adm := f.admittedCreate("v1:create-twice", mustUUID(), "hello once")

	first, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("first ApplyRuntimeCommand: %v", err)
	}
	f.drainOne(t)

	second, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("second ApplyRuntimeCommand: %v", err)
	}
	if !second.Duplicate {
		t.Errorf("Duplicate = false, want true on a redelivered create")
	}
	if second.PrefixSequence != first.PrefixSequence {
		t.Errorf("duplicate prefix = %d, want the original %d", second.PrefixSequence, first.PrefixSequence)
	}
	f.requireNoCommand(t, "a redelivered create")
	if got := readDispositions(t, f); len(got) != 1 {
		t.Fatalf("the journal holds %d dispositions, want exactly one: %+v", len(got), got)
	}
}

// TestACreatesPrefixIsTheLastRecordBeforeItsEffect is the ordering property, run over
// the REAL composition rather than the fake-loop fixture, because the record that
// could break it — the audit-only intent record — is appended by the production
// command journal and is invisible to a fixture that has none.
//
// The released settlement correlation resolves an application by ADJACENCY: the
// record at prefix+1 must be the public event the command caused. A create carrying a
// first message therefore audits its intent FIRST and writes the prefix LAST, exactly
// as an input does. Getting this backwards would resolve every create UNRESOLVED —
// which is the same liveness failure by a different route.
func TestACreatesPrefixIsTheLastRecordBeforeItsEffect(t *testing.T) {
	t.Parallel()
	store, backend := sessionstoreOverMemstoreWithBackend(t)
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}), store)
	if err != nil {
		t.Fatalf("newTestLifecycle: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := lifecycle.NewSession(ctx, "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	applier, ok := s.RuntimeCommands()
	if !ok {
		t.Fatalf("no runtime-command capability")
	}
	disp, err := applier.ApplyRuntimeCommand(ctx, runtimecommand.Admitted{
		CommandID:        "v1:create-adjacency",
		RuntimeCommandID: mustUUID(),
		Kind:             runtimecommand.KindCreate,
		LeaseEpoch:       s.runtimeCommandLease.Epoch(),
		// NO AttemptID, deliberately, and it is what makes the assertion the
		// ADJACENCY one. An attempt-bearing command writes its disposition frame
		// immediately after the prefix, so prefix+1 is that frame and settlement comes
		// from the evidence rather than from adjacency. The attempt-less shape is the
		// one the released correlation resolves positionally, and it is the shape this
		// ordering property is about — exactly as it is for an input
		// (TestApplicationPrefixIsTheLastRecordBeforeItsEffect).
		Blocks: []content.Block{&content.TextBlock{Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	sawPrefix, adjacent := recordsAroundPrefix(t, ctx, store, s.SessionID(), disp.PrefixSequence)
	if !sawPrefix {
		t.Fatalf("the walk never reached the application prefix at seq %d", disp.PrefixSequence)
	}
	if adjacent == nil {
		t.Fatalf("no record follows the prefix at seq %d; the effect never landed", disp.PrefixSequence)
	}
	eventRecord, isEvent := adjacent.(journal.EventRecord)
	if !isEvent {
		t.Fatalf("record at prefix+1 is %T, want a public journal.EventRecord:"+
			" the released correlation resolves by adjacency and reads anything else as UNRESOLVED", adjacent)
	}
	if vis := eventRecord.Event().Visibility(); vis != event.Public {
		t.Fatalf("record at prefix+1 is %T with visibility %v, want event.Public",
			eventRecord.Event(), vis)
	}
	if env := decodeStoredFrame(t, backend, s.SessionID(), disp.PrefixSequence+1); env.Kind != durablestore.EnvelopeKindPublicEvent {
		t.Fatalf("frame at prefix+1 has EnvelopeKind %d, want EnvelopeKindPublicEvent (%d)",
			env.Kind, durablestore.EnvelopeKindPublicEvent)
	}
	if env := decodeStoredFrame(t, backend, s.SessionID(), disp.PrefixSequence); env.Kind != durablestore.EnvelopeKindApplicationPrefix {
		t.Fatalf("frame at the reported prefix sequence has EnvelopeKind %d, want EnvelopeKindApplicationPrefix (%d)",
			env.Kind, durablestore.EnvelopeKindApplicationPrefix)
	}
}
