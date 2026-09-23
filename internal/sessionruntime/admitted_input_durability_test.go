package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/sessionstore"
)

// These tests pin the crash window between an admitted input's durable `applied`
// disposition and its durable effect. In harness v0.36.0 the applier handed the
// input to the loop's in-memory inbox and wrote `applied` at once; the turn that
// carries it out was appended later, asynchronously. A crash — or a graceful
// shutdown — inside that window left the SessionStore record settled `applied`
// forever while the user's message was never run, because restore replayed only
// subagent hand-backs.
//
// The window is opened deterministically by holding the loop at session execution
// admission (the interrupt barrier enterExecution waits on): the input is accepted
// and queued, and TurnStarted cannot be published until the hold is released.

// durableInputLifecycle builds a real lifecycle-backed session over an in-memory
// store, so the journal, lease, application prefix, disposition, restore, and the
// loop actor are all production code.
func durableInputLifecycle(t *testing.T) (*sessionstore.Store, *Lifecycle, *Session) {
	t.Helper()
	store := sessionstoreOverMemstore(t)
	lifecycle, err := newTestLifecycle(cfg(&stubLLM{chunks: []content.Chunk{textChunk("done")}}), store)
	if err != nil {
		t.Fatalf("newTestLifecycle: %v", err)
	}
	s, err := lifecycle.NewSession(context.Background(), "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return store, lifecycle, s
}

// holdTurnStart parks the active loop at session execution admission, so an input
// it accepts is queued but its TurnStarted cannot be appended. The returned func
// releases the hold (idempotently).
func holdTurnStart(s *Session) func() {
	id := s.ActiveLoopID()
	s.loopsMu.Lock()
	if s.interruptPending == nil {
		s.interruptPending = make(map[uuid.UUID]int)
	}
	s.interruptPending[id]++
	s.loopsMu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { s.clearInterruptPending([]uuid.UUID{id}) }) }
}

// crashWithoutTeardown models process death: the single-writer lease is given up
// FIRST, so nothing the dying runtime does afterwards — the loop answering its
// cancelled context by returning its inbox, above all — can become durable. Only
// what was durable at the crash point survives, which is what a successor sees
// after a real crash.
func crashWithoutTeardown(t *testing.T, s *Session) {
	t.Helper()
	s.releaseLease(context.Background())
	s.sessionCancel()
	waitLoopsExited(t, s)
}

func admittedInputFor(s *Session, id runtimecommand.CommandID, runtimeID uuid.UUID, text string, attempt runtimecommand.AttemptID) runtimecommand.Admitted {
	return runtimecommand.Admitted{
		CommandID:        id,
		RuntimeCommandID: runtimeID,
		Kind:             runtimecommand.KindInput,
		LeaseEpoch:       s.runtimeCommandLease.Epoch(),
		AttemptID:        attempt,
		Blocks:           []content.Block{&content.TextBlock{Text: text}},
	}
}

// causedEvents returns every durable public event whose Cause.CommandID is runtimeID.
func causedEvents(t *testing.T, store *sessionstore.Store, sid, runtimeID uuid.UUID) []event.Event {
	t.Helper()
	var out []event.Event
	for _, ev := range replayAllSessionEvents(t, store, sid) {
		if ev.EventHeader().Cause.CommandID == runtimeID {
			out = append(out, ev)
		}
	}
	return out
}

// countOpenings counts TurnStarted/TurnFoldedInto caused by runtimeID — the events
// that prove the input entered a turn — and fails on any caused cancellation or
// rejection, which would be an applied command whose input did not run.
func countOpenings(t *testing.T, events []event.Event) int {
	t.Helper()
	n := 0
	for _, ev := range events {
		switch ev.(type) {
		case event.TurnStarted, event.TurnFoldedInto:
			n++
		case event.InputCancelled, event.TurnRejected:
			t.Fatalf("an input settled applied was answered %T: %+v", ev, ev)
		}
	}
	return n
}

// waitForOpening polls until the input has entered exactly one turn, failing on the
// deadline with "applied, never run".
func waitForOpening(t *testing.T, store *sessionstore.Store, sid, runtimeID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := countOpenings(t, causedEvents(t, store, sid, runtimeID))
		if n == 1 {
			return
		}
		if n > 1 {
			t.Fatalf("the input entered %d turns, want exactly one", n)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the input was settled applied and never entered a turn")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func requireAppliedDisposition(t *testing.T, store *sessionstore.Store, sid uuid.UUID, id runtimecommand.CommandID) {
	t.Helper()
	got := readDispositions(t, &runtimeCommandFixture{store: store, sid: sid})
	var applied int
	for _, d := range got {
		if d.CommandID != id {
			continue
		}
		if d.Disposition != runtimecommand.DispositionApplied {
			t.Fatalf("disposition for %s = %v, want applied", id, d.Disposition)
		}
		applied++
	}
	if applied != 1 {
		t.Fatalf("the journal holds %d dispositions for %s, want exactly one applied: %+v", applied, id, got)
	}
}

// TestAnAppliedInputSurvivesACrashBeforeItsTurnStarts is the defect. The input is
// accepted and settled applied; the runtime then dies before the turn that carries
// it out is durable. A successor restoring the session must run it.
func TestAnAppliedInputSurvivesACrashBeforeItsTurnStarts(t *testing.T) {
	t.Parallel()
	store, lifecycle, s := durableInputLifecycle(t)
	sid := s.SessionID()
	release := holdTurnStart(s)
	defer release()

	runtimeID := mustUUID()
	adm := admittedInputFor(s, "v1:crash-window", runtimeID, "do not lose me", "attempt/crash-window")
	if _, err := s.ApplyRuntimeCommand(context.Background(), adm); err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	requireAppliedDisposition(t, store, sid, adm.CommandID)
	if n := countOpenings(t, causedEvents(t, store, sid, runtimeID)); n != 0 {
		t.Fatalf("precondition: the hold did not open the window; %d openings are durable", n)
	}

	crashWithoutTeardown(t, s)
	if n := countOpenings(t, causedEvents(t, store, sid, runtimeID)); n != 0 {
		t.Fatalf("precondition: the crash left %d openings; the window was not exercised", n)
	}

	restored, err := lifecycle.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	waitForOpening(t, store, sid, runtimeID)
	requireAppliedDisposition(t, store, sid, adm.CommandID)
}

// TestAGracefulShutdownInTheWindowDoesNotCancelAnAppliedInput is the shutdown half.
// v0.36.0's shutdown terminal returned the queued input as InputCancelled — a
// durable contradiction of the command's `applied` settlement. The input must
// instead be carried to the successor.
func TestAGracefulShutdownInTheWindowDoesNotCancelAnAppliedInput(t *testing.T) {
	t.Parallel()
	store, lifecycle, s := durableInputLifecycle(t)
	sid := s.SessionID()
	release := holdTurnStart(s)
	defer release()

	runtimeID := mustUUID()
	adm := admittedInputFor(s, "v1:shutdown-window", runtimeID, "carry me over", "attempt/shutdown-window")
	if _, err := s.ApplyRuntimeCommand(context.Background(), adm); err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// countOpenings fails on a caused InputCancelled.
	if n := countOpenings(t, causedEvents(t, store, sid, runtimeID)); n != 0 {
		t.Fatalf("precondition: %d openings before restore", n)
	}

	restored, err := lifecycle.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	waitForOpening(t, store, sid, runtimeID)
}

// TestARecoveredInputIsNotAppliedTwice covers redelivery and a second restore: the
// successor replays the input once, a redelivery of the same admitted command under
// the successor's epoch is a duplicate, and a further restore after the turn is
// durable replays nothing.
func TestARecoveredInputIsNotAppliedTwice(t *testing.T) {
	t.Parallel()
	store, lifecycle, s := durableInputLifecycle(t)
	sid := s.SessionID()
	release := holdTurnStart(s)
	defer release()

	runtimeID := mustUUID()
	adm := admittedInputFor(s, "v1:once", runtimeID, "exactly once", "attempt/once")
	first, err := s.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	crashWithoutTeardown(t, s)

	restored, err := lifecycle.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	waitForOpening(t, store, sid, runtimeID)

	redelivered := adm
	redelivered.LeaseEpoch = restored.runtimeCommandLease.Epoch()
	second, err := restored.ApplyRuntimeCommand(context.Background(), redelivered)
	if err != nil {
		t.Fatalf("redelivered ApplyRuntimeCommand: %v", err)
	}
	if !second.Duplicate || second.PrefixSequence != first.PrefixSequence {
		t.Fatalf("redelivery = %+v, want a duplicate of prefix %d", second, first.PrefixSequence)
	}
	if err := restored.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if err := restored.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	again, err := lifecycle.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("second RestoreSession: %v", err)
	}
	if err := again.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if err := again.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := countOpenings(t, causedEvents(t, store, sid, runtimeID)); n != 1 {
		t.Fatalf("the input entered %d turns across two restores and a redelivery, want exactly one", n)
	}
	requireAppliedDisposition(t, store, sid, adm.CommandID)
}

// TestAnInterruptBeforeTheTurnStartsKeepsTheAppliedInput pins interrupt-before-start.
// An ordinary interrupt retains queued HUMAN input (retainUserQueuedInbox), and an
// admitted input is human input: it is neither cancelled nor lost, and it runs once
// admission is released. The command stays `applied` with a real effect.
func TestAnInterruptBeforeTheTurnStartsKeepsTheAppliedInput(t *testing.T) {
	t.Parallel()
	store, _, s := durableInputLifecycle(t)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	sid := s.SessionID()
	release := holdTurnStart(s)
	defer release()

	runtimeID := mustUUID()
	adm := admittedInputFor(s, "v1:interrupt-before-start", runtimeID, "still wanted", "attempt/interrupt-before-start")
	if _, err := s.ApplyRuntimeCommand(context.Background(), adm); err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	interruptDone := make(chan error, 1)
	go func() {
		_, err := s.Interrupt(context.Background())
		interruptDone <- err
	}()
	release()
	select {
	case err := <-interruptDone:
		if err != nil {
			t.Fatalf("Interrupt: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Interrupt did not return")
	}
	waitForOpening(t, store, sid, runtimeID)
	requireAppliedDisposition(t, store, sid, adm.CommandID)
}

// v036Journal writes, by hand, exactly what harness v0.36.0 left behind when it
// crashed in the window: the audit intent record, the application prefix, the
// applied disposition — and nothing else for that input. It returns the runtime id.
func v036Journal(t *testing.T, s *Session, id runtimecommand.CommandID, text string, tail ...event.Event) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	runtimeID := mustUUID()
	adm := admittedInputFor(s, id, runtimeID, text, runtimecommand.AttemptID("attempt/"+string(id)))
	input := command.UserInput{
		Header: command.Header{CommandID: runtimeID, Agency: identity.AgencyUser, CreatedAt: time.Now().UTC()},
		Blocks: adm.Blocks,
	}
	if err := s.cmdAppender.AppendCommand(ctx, journal.NewCommandRecord(s.sessionID, s.ActiveLoopID(), input)); err != nil {
		t.Fatalf("append intent: %v", err)
	}
	if _, err := s.runtimeCommands.AppendCommandApplication(ctx, adm.Application()); err != nil {
		t.Fatalf("append prefix: %v", err)
	}
	log := s.runtimeCommands.(dispositionLog)
	if _, err := log.AppendCommandDisposition(ctx, adm.DispositionFor(runtimecommand.DispositionApplied, s.runtimeCommandLease.Epoch())); err != nil {
		t.Fatalf("append disposition: %v", err)
	}
	return runtimeID
}

// TestAV036CrashWindowJournalIsRecoveredOnRestore is replay compatibility: a journal
// the released writer left in the crash window, with no new record kind anywhere,
// is recovered by this restore.
func TestAV036CrashWindowJournalIsRecoveredOnRestore(t *testing.T) {
	t.Parallel()
	store, lifecycle, s := durableInputLifecycle(t)
	sid := s.SessionID()
	runtimeID := v036Journal(t, s, "v1:v036-shape", "written by v0.36.0")
	crashWithoutTeardown(t, s)

	restored, err := lifecycle.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	waitForOpening(t, store, sid, runtimeID)
}

// TestPlanAppliedAdmittedInputs is the replay predicate over every journal shape
// that matters, including the v0.36.0 shapes it must NOT replay.
func TestPlanAppliedAdmittedInputs(t *testing.T) {
	t.Parallel()
	sid, loopID, runtimeID := mustUUID(), mustUUID(), mustUUID()
	blocks := []content.Block{&content.TextBlock{Text: "owed"}}
	intent := journal.NewCommandRecord(sid, loopID, command.UserInput{
		Header: command.Header{CommandID: runtimeID, Agency: identity.AgencyUser}, Blocks: blocks,
	})
	emptyIntent := journal.NewCommandRecord(sid, loopID, command.UserInput{
		Header: command.Header{CommandID: runtimeID, Agency: identity.AgencyUser},
	})
	disposition := func(kind runtimecommand.Kind, outcome runtimecommand.DispositionKind) journal.JournalRecord {
		return journal.NewCommandDispositionRecord(runtimecommand.CommandDisposition{
			CommandID: "v1:plan", RuntimeCommandID: runtimeID, Kind: kind, LeaseEpoch: 1,
			AttemptID: "attempt/plan", AttemptJournalEpoch: 1, Disposition: outcome,
		})
	}
	caused := func(ev event.Event) journal.JournalRecord { return journal.NewEventRecord(ev) }
	cause := event.Header{Cause: identity.Cause{CommandID: runtimeID}}
	applied := disposition(runtimecommand.KindInput, runtimecommand.DispositionApplied)
	tests := []struct {
		name    string
		records []journal.JournalRecord
		replay  bool
	}{
		{name: "applied input with no effect is owed", records: []journal.JournalRecord{intent, applied}, replay: true},
		{name: "applied create with a first message is owed", records: []journal.JournalRecord{intent, disposition(runtimecommand.KindCreate, runtimecommand.DispositionApplied)}, replay: true},
		{name: "a started turn settles it", records: []journal.JournalRecord{intent, applied, caused(event.TurnStarted{Header: cause})}},
		{name: "a fold settles it", records: []journal.JournalRecord{intent, applied, caused(event.TurnFoldedInto{Header: cause})}},
		{name: "a v0.36.0 shutdown cancellation settles it (released behaviour)", records: []journal.JournalRecord{intent, applied, caused(event.InputCancelled{Header: cause})}},
		{name: "a rejection settles it", records: []journal.JournalRecord{intent, applied, caused(event.TurnRejected{Header: cause})}},
		{name: "an effect before the disposition still settles it", records: []journal.JournalRecord{intent, caused(event.TurnStarted{Header: cause}), applied}},
		{name: "refused is not owed", records: []journal.JournalRecord{intent, disposition(runtimecommand.KindInput, runtimecommand.DispositionRefused)}},
		{name: "not_applied is not owed", records: []journal.JournalRecord{intent, disposition(runtimecommand.KindInput, runtimecommand.DispositionNotApplied)}},
		{name: "no disposition is not owed (a successor closes it)", records: []journal.JournalRecord{intent}},
		{name: "an applied interrupt is not an input", records: []journal.JournalRecord{intent, disposition(runtimecommand.KindInterrupt, runtimecommand.DispositionApplied)}},
		{name: "no intent record carries nothing to replay", records: []journal.JournalRecord{applied}},
		{name: "a bare create has no blocks to replay", records: []journal.JournalRecord{emptyIntent, disposition(runtimecommand.KindCreate, runtimecommand.DispositionApplied)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plan := planAppliedAdmittedInputs(tt.records)
			if !tt.replay {
				if len(plan) != 0 {
					t.Fatalf("plan = %+v, want nothing replayed", plan)
				}
				return
			}
			if len(plan) != 1 {
				t.Fatalf("plan has %d entries, want exactly one", len(plan))
			}
			if plan[0].loopID != loopID || plan[0].cmd.CommandID != runtimeID || len(plan[0].cmd.Blocks) != 1 {
				t.Fatalf("plan = %+v, want the intent's loop, id and blocks", plan[0])
			}
		})
	}
}

// TestADeclinedAdmittedInputIsRefusedNotApplied: a loop that declines the input
// before its acceptance commits (here: shutting down) leaves no effect, so the
// command is refused under the live grant. v0.36.0 recorded `applied` and the loop
// then published TurnRejected — an applied command whose input never ran.
func TestADeclinedAdmittedInputIsRefusedNotApplied(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	sink := make(chan command.Command)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case cmd := <-sink:
				if input, ok := cmd.(command.UserInput); ok && input.Admission != nil {
					input.Admission.Result <- &loop.InputRejectedError{Reason: event.RejectShuttingDown}
				}
			case <-stop:
				return
			}
		}
	}()
	f.session.loops[f.session.activeLoopID].backend = &admittingBackend{channelBackend{Commands: sink, Done: make(chan struct{})}}
	adm := f.admittedInput("v1:declined", mustUUID(), "too late")
	adm.AttemptID = "attempt/declined"

	disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	var rejected *loop.InputRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("ApplyRuntimeCommand err = %v, want the loop's *loop.InputRejectedError", err)
	}
	if disp.PrefixSequence == 0 {
		t.Fatalf("the prefix did not commit")
	}
	if got := onlyDisposition(t, f); got.Disposition != runtimecommand.DispositionRefused {
		t.Fatalf("disposition = %q, want refused", got.Disposition)
	}
}

// TestAFailedAcceptanceCommitLeavesTheAttemptClosable: when the applied append
// fails, the actor drops the input, so nothing caused by it exists and a successor's
// not_applied closure is TRUE and succeeds. v0.36.0 had already handed the input to
// the loop; a failed disposition then left an effect with no disposition, which the
// closure must refuse forever (EnduringEffectError).
func TestAFailedAcceptanceCommitLeavesTheAttemptClosable(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	attemptEpoch := f.lease.Epoch()
	f.session.runtimeCommands = failingDispositionLog{f.session.runtimeCommands.(dispositionLogAndRuntimeLog)}
	runtimeID := mustUUID()
	adm := f.admittedInput("v1:commit-fails", runtimeID, "hello")
	adm.AttemptID = "attempt/commit-fails"
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), adm); err == nil {
		t.Fatalf("ApplyRuntimeCommand succeeded; the fixture did not fail the disposition")
	}
	f.requireNoCommand(t, "an input whose acceptance did not commit")
	if got := readDispositions(t, f); len(got) != 0 {
		t.Fatalf("dispositions = %+v, want none", got)
	}
	succ := takeOver(t, f)
	if _, err := attemptCloser(t, succ).CloseAttempt(context.Background(), closureFor(adm.CommandID, runtimeID, attemptEpoch)); err != nil {
		t.Fatalf("CloseAttempt = %v, want not_applied to be written over an attempt with no effect", err)
	}
	if got := onlyDisposition(t, succ); got.Disposition != runtimecommand.DispositionNotApplied {
		t.Fatalf("closure = %q, want not_applied", got.Disposition)
	}
}

type failingIntentAppender struct{}

func (failingIntentAppender) AppendCommand(context.Context, journal.CommandRecord) error {
	return errors.New("intent append failed")
}

// TestAnAdmittedInputWhoseIntentCannotBeRecordedIsRefusedBeforeThePrefix: the intent
// record is the only durable copy of the input's blocks restore can replay from, so
// under an attempt it is load-bearing. Failing it writes nothing durable — no prefix,
// no disposition — and the command may be re-offered.
func TestAnAdmittedInputWhoseIntentCannotBeRecordedIsRefusedBeforeThePrefix(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	f.session.cmdAppender = failingIntentAppender{}
	adm := f.admittedInput("v1:no-intent", mustUUID(), "hello")
	adm.AttemptID = "attempt/no-intent"
	disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	var intentErr *AdmittedIntentAppendError
	if !errors.As(err, &intentErr) {
		t.Fatalf("ApplyRuntimeCommand err = %v, want *AdmittedIntentAppendError", err)
	}
	if disp != (runtimecommand.Disposition{}) {
		t.Fatalf("Disposition = %+v, want zero: nothing durable may have been written", disp)
	}
	if got := readApplications(t, f); len(got) != 0 {
		t.Fatalf("a prefix was written for a command whose intent failed: %+v", got)
	}
	f.requireNoCommand(t, "an input whose intent failed")
}

// TestABackendWithoutRuntimeAdmissionKeepsTheReleasedPath is F1: a loop.Backend that
// does not declare runtime admission (a foreign loop) must never be sent the
// handshake — it would consume the input, run it, and never answer, hanging the
// applier with no disposition. It gets the plain input and the applier records
// applied after the send, as v0.36.0 did.
func TestABackendWithoutRuntimeAdmissionKeepsTheReleasedPath(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	raw := make(chan command.Command, 4)
	f.session.loops[f.session.activeLoopID].backend = &channelBackend{Commands: raw, Done: make(chan struct{})}
	adm := f.admittedInput("v1:foreign", mustUUID(), "hello")
	adm.AttemptID = "attempt/foreign"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.session.ApplyRuntimeCommand(ctx, adm); err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	select {
	case cmd := <-raw:
		input, ok := cmd.(command.UserInput)
		if !ok || input.Admission != nil {
			t.Fatalf("backend received %T with Admission=%v, want a plain UserInput", cmd, ok && input.Admission != nil)
		}
	default:
		t.Fatalf("the backend received nothing")
	}
	if got := onlyDisposition(t, f); got.Disposition != runtimecommand.DispositionApplied {
		t.Fatalf("disposition = %q, want applied", got.Disposition)
	}
}

// TestTheApplierHonoursItsContextWhileTheActorIsSilent is F1's second half: even
// against a backend that declares admission and then never answers, the applier
// returns once its caller's context ends, with the prefix it already committed.
func TestTheApplierHonoursItsContextWhileTheActorIsSilent(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	silent := make(chan command.Command, 4)
	f.session.loops[f.session.activeLoopID].backend = &admittingBackend{channelBackend{Commands: silent, Done: make(chan struct{})}}
	adm := f.admittedInput("v1:silent", mustUUID(), "hello")
	adm.AttemptID = "attempt/silent"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	returned := make(chan struct{})
	var disp runtimecommand.Disposition
	var err error
	go func() {
		disp, err = f.session.ApplyRuntimeCommand(ctx, adm)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatalf("ApplyRuntimeCommand blocked past its context")
	}
	var sessionErr *SessionError
	if !errors.As(err, &sessionErr) || sessionErr.Kind != SessionContextDone {
		t.Fatalf("err = %v, want *SessionError{SessionContextDone}", err)
	}
	if disp.PrefixSequence == 0 {
		t.Fatalf("Disposition = %+v, want the committed prefix alongside the error", disp)
	}
}

// ambiguousDispositionLog lands the disposition and then reports failure — a lost
// reply.
type ambiguousDispositionLog struct{ dispositionLogAndRuntimeLog }

func (l ambiguousDispositionLog) AppendCommandDisposition(ctx context.Context, d runtimecommand.CommandDisposition) (journal.AppendResult, error) {
	if _, err := l.dispositionLogAndRuntimeLog.AppendCommandDisposition(ctx, d); err != nil {
		return journal.AppendResult{}, err
	}
	return journal.AppendResult{}, errors.New("reply lost after the append landed")
}

// TestALandedCommitReportedAsFailedStillRunsTheInput is F6: the applied append
// landed but reported an error. Dropping the input would leave a durable `applied`
// whose input waits for a restore that a live session may never have; the re-read
// finds it and the input runs now.
func TestALandedCommitReportedAsFailedStillRunsTheInput(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	f.session.runtimeCommands = ambiguousDispositionLog{f.session.runtimeCommands.(dispositionLogAndRuntimeLog)}
	adm := f.admittedInput("v1:ambiguous", mustUUID(), "hello")
	adm.AttemptID = "attempt/ambiguous"
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), adm); err != nil {
		t.Fatalf("ApplyRuntimeCommand = %v, want success: the applied record is durable", err)
	}
	if _, ok := f.drainOne(t).(command.UserInput); !ok {
		t.Fatalf("the input was not delivered")
	}
	if got := onlyDisposition(t, f); got.Disposition != runtimecommand.DispositionApplied {
		t.Fatalf("disposition = %q, want applied", got.Disposition)
	}
}

// TestReplayCarriesTheInputOverOnlyOnACapableBackend pins the replay's handshake:
// a capable backend gets an Admission with NO Commit (the acceptance is already
// durable) so the replayed input is carried over again if this runtime goes away
// before it starts; a backend without admission gets the plain input.
func TestReplayCarriesTheInputOverOnlyOnACapableBackend(t *testing.T) {
	t.Parallel()
	entry := admittedInputReplay{cmd: command.UserInput{
		Header: command.Header{CommandID: mustUUID(), Agency: identity.AgencyUser},
		Blocks: []content.Block{&content.TextBlock{Text: "owed"}},
	}}

	f := newRuntimeCommandFixture(t)
	entry.loopID = f.session.activeLoopID
	if err := f.session.replayAdmittedInput(context.Background(), entry); err != nil {
		t.Fatalf("replay (capable): %v", err)
	}
	input, ok := f.drainOne(t).(command.UserInput)
	if !ok || input.Admission == nil || input.Admission.Commit != nil {
		t.Fatalf("capable backend got %+v, want an Admission with a nil Commit", input.Admission)
	}

	g := newRuntimeCommandFixture(t)
	raw := make(chan command.Command, 1)
	g.session.loops[g.session.activeLoopID].backend = &channelBackend{Commands: raw, Done: make(chan struct{})}
	entry.loopID = g.session.activeLoopID
	if err := g.session.replayAdmittedInput(context.Background(), entry); err != nil {
		t.Fatalf("replay (plain): %v", err)
	}
	if plain := (<-raw).(command.UserInput); plain.Admission != nil {
		t.Fatalf("a backend without admission was sent the handshake")
	}
}

// TestASuccessorSettlesAnOwedInputEndToEnd is F2 in harness: the predecessor
// committed `applied` and died before the store settled it; the successor restores
// (replaying the owed input) and Host's settleOrRecover closes before it settles.
// The closure must succeed — reporting the durable applied — both before and after
// the replay's TurnStarted lands, and must write nothing.
func TestASuccessorSettlesAnOwedInputEndToEnd(t *testing.T) {
	t.Parallel()
	store, lifecycle, s := durableInputLifecycle(t)
	sid := s.SessionID()
	release := holdTurnStart(s)
	defer release()
	runtimeID := mustUUID()
	attempt := runtimecommand.AttemptID("attempt/owed-e2e")
	adm := admittedInputFor(s, "v1:owed-e2e", runtimeID, "owed", attempt)
	epoch := s.runtimeCommandLease.Epoch()
	if _, err := s.ApplyRuntimeCommand(context.Background(), adm); err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	crashWithoutTeardown(t, s)
	restored, err := lifecycle.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
	closure := runtimecommand.Closure{CommandID: adm.CommandID, RuntimeCommandID: runtimeID, Kind: runtimecommand.KindInput, AttemptID: attempt, AttemptJournalEpoch: epoch}
	check := func(when string) {
		res, err := restored.CloseAttempt(context.Background(), closure)
		if err != nil {
			t.Fatalf("CloseAttempt %s = %v, want the already-disposed success", when, err)
		}
		if res.AlreadyDisposed != runtimecommand.DispositionApplied || res.Appended {
			t.Fatalf("CloseAttempt %s = %+v, want AlreadyDisposed=applied with nothing appended", when, res)
		}
	}
	check("right after restore")
	waitForOpening(t, store, sid, runtimeID)
	check("after the replayed TurnStarted")
	requireAppliedDisposition(t, store, sid, adm.CommandID)
}

// blockingDispositionLog holds every disposition append until its context ends,
// then reports that it was cancelled.
type blockingDispositionLog struct {
	dispositionLogAndRuntimeLog
	cancelled chan struct{}
}

func (l blockingDispositionLog) AppendCommandDisposition(ctx context.Context, _ runtimecommand.CommandDisposition) (journal.AppendResult, error) {
	<-ctx.Done()
	close(l.cancelled)
	return journal.AppendResult{}, ctx.Err()
}

// TestTheCommitIsCancelledByTheCallersContext is F3's caller half: the actor runs
// Commit under its own loop context, and a wedged append must still end when the
// Host's context does — otherwise the actor stays blocked in storage after the
// applier has already given up.
func TestTheCommitIsCancelledByTheCallersContext(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	cancelled := make(chan struct{})
	f.session.runtimeCommands = blockingDispositionLog{f.session.runtimeCommands.(dispositionLogAndRuntimeLog), cancelled}
	adm := f.admittedInput("v1:wedged", mustUUID(), "hello")
	adm.AttemptID = "attempt/wedged"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() { _, _ = f.session.ApplyRuntimeCommand(ctx, adm) }()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatalf("the actor's Commit was not cancelled by the caller's context")
	}
}
