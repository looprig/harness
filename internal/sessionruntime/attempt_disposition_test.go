package sessionruntime

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/sessionstore"
)

// readDispositions walks the session's journal and returns every disposition record
// it holds, in ledger order. It reads through the REAL record replayer, so a record
// this test can see is a record the counterparty's walk can see.
func readDispositions(t *testing.T, f *runtimeCommandFixture) []runtimecommand.CommandDisposition {
	t.Helper()
	ctx := context.Background()
	replayer, err := f.store.OpenInternalRecordReplayer(f.sid, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatalf("OpenInternalRecordReplayer: %v", err)
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: f.sid, From: journal.Beginning()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	var out []runtimecommand.CommandDisposition
	for {
		rec, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if d, ok := rec.(journal.CommandDispositionRecord); ok {
			out = append(out, d.Disposition())
		}
	}
}

func onlyDisposition(t *testing.T, f *runtimeCommandFixture) runtimecommand.CommandDisposition {
	t.Helper()
	got := readDispositions(t, f)
	if len(got) != 1 {
		t.Fatalf("the journal holds %d dispositions, want exactly one: %+v", len(got), got)
	}
	return got[0]
}

// ackInterrupts answers every interrupt the session fans out with cancelled, until
// the test ends. It is bounded by a stop channel and JOINED at cleanup: ranging over
// f.cmds would leak a goroutine reading a session the fixture has torn down.
//
// Without a responder an interrupt BLOCKS on its ack forever, which is a hang rather
// than a failure — and a hang scores nothing.
func ackInterrupts(t *testing.T, f *runtimeCommandFixture, cancelled bool) {
	t.Helper()
	stop := make(chan struct{})
	var responder sync.WaitGroup
	responder.Add(1)
	go func() {
		defer responder.Done()
		for {
			select {
			case cmd := <-f.cmds:
				if interrupt, ok := cmd.(command.Interrupt); ok {
					select {
					case interrupt.Ack <- cancelled:
					case <-stop:
						return
					}
				}
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		responder.Wait()
	})
}

func (f *runtimeCommandFixture) admittedInterrupt(id runtimecommand.CommandID, runtimeID uuid.UUID) runtimecommand.Admitted {
	return runtimecommand.Admitted{
		CommandID:        id,
		RuntimeCommandID: runtimeID,
		Kind:             runtimecommand.KindInterrupt,
		LeaseEpoch:       f.lease.Epoch(),
	}
}

// TestAppliedDispositionIsWrittenAfterASucceedingInput is the first row of the
// per-kind table: sendUserInput returned nil, so the loop is live and took the
// command, and the runtime durably records that it accepted it.
//
// The narrowness is the contract. This record does NOT say a turn started, does NOT
// say a later TurnRejected cannot follow, and does NOT say the queued input survives
// a crash — the loop mailbox is in memory. Nothing here may be widened into any of
// those, which is why this test asserts only the record's content.
func TestAppliedDispositionIsWrittenAfterASucceedingInput(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	runtimeID := mustUUID()
	adm := f.admittedInput("v1:applied", runtimeID, "hello")
	adm.AttemptID = "attempt/applied"

	disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	f.drainOne(t)

	got := onlyDisposition(t, f)
	want := runtimecommand.CommandDisposition{
		CommandID:           adm.CommandID,
		RuntimeCommandID:    runtimeID,
		Kind:                runtimecommand.KindInput,
		LeaseEpoch:          f.lease.Epoch(),
		AttemptID:           "attempt/applied",
		AttemptJournalEpoch: f.lease.Epoch(),
		Disposition:         runtimecommand.DispositionApplied,
	}
	if got != want {
		t.Fatalf("disposition = %+v, want %+v", got, want)
	}
	if disp.PrefixSequence == 0 {
		t.Errorf("Disposition.PrefixSequence = 0, want the committed prefix")
	}
}

// TestInterruptDispositionSeparatesAppliedFromNoOp is the pair of interrupt rows,
// and the distinction is the protocol's rather than this writer's convenience: an
// interrupt that cancelled a running turn is applied, and one that found the session
// idle is a SUCCESSFUL no-op. Both settle the command as applied; collapsing them
// would erase "the runtime accepted this and it had no effect", which the design
// states is a different outcome from reject-before-dispatch.
func TestInterruptDispositionSeparatesAppliedFromNoOp(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		busy bool
		want runtimecommand.DispositionKind
	}{
		"idle session":   {false, runtimecommand.DispositionNoOp},
		"turn in flight": {true, runtimecommand.DispositionApplied},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRuntimeCommandFixture(t)
			// The loop's ack IS the fan-out result: true means a running turn was
			// cancelled, false means the loop was idle. Nothing else distinguishes the
			// two rows, which is what makes the disposition the only variable.
			ackInterrupts(t, f, row.busy)
			runtimeID := mustUUID()
			adm := f.admittedInterrupt("v1:interrupt", runtimeID)
			adm.AttemptID = "attempt/interrupt"

			disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
			if err != nil {
				t.Fatalf("ApplyRuntimeCommand: %v", err)
			}
			if disp.Interrupted != row.busy {
				t.Fatalf("Interrupted = %v, want %v — the fixture did not produce the state under test",
					disp.Interrupted, row.busy)
			}
			got := onlyDisposition(t, f)
			if got.Disposition != row.want {
				t.Errorf("disposition = %q, want %q", got.Disposition, row.want)
			}
			if got.Kind != runtimecommand.KindInterrupt {
				t.Errorf("command kind = %q, want interrupt", got.Kind)
			}
			if got.AttemptID != "attempt/interrupt" {
				t.Errorf("attempt = %q, want attempt/interrupt", got.AttemptID)
			}
		})
	}
}

// TestRefusedDispositionIsWrittenWhenTheEffectFailsAfterThePrefix drives the arm
// that only exists for LIVENESS, and it is the arm a suite whose fixtures all
// succeed cannot see at all.
//
// The shape is exactly TestEffectFailureAfterTheDurablePrefixStrandsTheCommand's:
// the loop exits at the instant the prefix commits, so the prefix is durable and the
// effect fails under a STILL-LIVE lease. Without the refusal such a command has no
// terminal arm — not_applied requires a strictly later grant, and a healthy Host
// never turns its lease over — so it sits applying forever.
func TestRefusedDispositionIsWrittenWhenTheEffectFailsAfterThePrefix(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	runtimeID := mustUUID()
	adm := f.admittedInput("v1:refused", runtimeID, "hello")
	adm.AttemptID = "attempt/refused"

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
	// A refusal is authored by the ATTEMPT's own grant — that is the whole reason it
	// exists alongside not_applied — so both epochs are the live lease's.
	if got.LeaseEpoch != f.lease.Epoch() || got.AttemptJournalEpoch != f.lease.Epoch() {
		t.Errorf("grants = author %d attempt %d, want %d for both",
			got.LeaseEpoch, got.AttemptJournalEpoch, f.lease.Epoch())
	}
	if got.AttemptID != "attempt/refused" {
		t.Errorf("attempt = %q, want attempt/refused", got.AttemptID)
	}
}

// TestNoAttemptIDWritesNoDisposition is the compatibility contract, and it is a
// whole-journal claim rather than a "no disposition for this attempt" one: a legacy
// admitted record leaves the journal exactly as it was, so a deployment that has not
// adopted the protocol sees no new frame kind — which matters because a binary
// pinned below sessionstore v0.9.0 REFUSES a journal containing one.
func TestNoAttemptIDWritesNoDisposition(t *testing.T) {
	t.Parallel()
	for name, adm := range map[string]func(f *runtimeCommandFixture) runtimecommand.Admitted{
		"input": func(f *runtimeCommandFixture) runtimecommand.Admitted {
			return f.admittedInput("v1:legacy", mustUUID(), "hi")
		},
		"interrupt": func(f *runtimeCommandFixture) runtimecommand.Admitted {
			return f.admittedInterrupt("v1:legacy", mustUUID())
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRuntimeCommandFixture(t)
			ackInterrupts(t, f, false)
			record := adm(f)
			if record.AttemptID != "" {
				t.Fatalf("the fixture carries an attempt id, so this proves nothing")
			}
			if _, err := f.session.ApplyRuntimeCommand(context.Background(), record); err != nil {
				t.Fatalf("ApplyRuntimeCommand: %v", err)
			}
			if got := readDispositions(t, f); len(got) != 0 {
				t.Fatalf("a legacy admitted record produced %d dispositions: %+v", len(got), got)
			}
		})
	}
}

// TestAttemptBearingCommandIsRefusedWhenTheLogCannotRecordADisposition fails closed
// BEFORE the prefix. Applying a command whose evidence can never be written leaves
// it applying forever, which nobody can settle; refusing it is a state Host can act
// on.
func TestAttemptBearingCommandIsRefusedWhenTheLogCannotRecordADisposition(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	// A log that satisfies only the base seam, which is exactly what an older
	// composition root or a test double provides.
	f.session.runtimeCommands = baseOnlyLog{inner: f.session.runtimeCommands}
	adm := f.admittedInput("v1:no-disposition", mustUUID(), "hi")
	adm.AttemptID = "attempt/x"

	disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	var unsupported *DispositionUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want a *DispositionUnsupportedError", err)
	}
	if disp != (runtimecommand.Disposition{}) {
		t.Errorf("Disposition = %+v, want the zero value: nothing durable was written", disp)
	}
	f.requireNoCommand(t, "a command refused for want of a disposition log")
	// The SAME log applies a legacy record unchanged, so the refusal is about the
	// attempt and not about the log being unusable.
	legacy := f.admittedInput("v1:legacy-ok", mustUUID(), "hi")
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), legacy); err != nil {
		t.Fatalf("a legacy record was refused by the same log: %v", err)
	}
	f.drainOne(t)
}

// baseOnlyLog satisfies runtimeCommandLog and NOTHING else, so a type assertion for
// the disposition extension fails.
type baseOnlyLog struct{ inner runtimeCommandLog }

func (l baseOnlyLog) AppendCommandApplication(ctx context.Context, app runtimecommand.Application) (journal.AppendResult, error) {
	return l.inner.AppendCommandApplication(ctx, app)
}

func (l baseOnlyLog) ReadCommandApplicationAt(ctx context.Context, seq uint64) (runtimecommand.Application, error) {
	return l.inner.ReadCommandApplicationAt(ctx, seq)
}

// dispositionLogAndRuntimeLog is the composed seam a real log satisfies.
type dispositionLogAndRuntimeLog interface {
	runtimeCommandLog
	dispositionLog
}

// dispositionAwareOrderLog is orderRecordingLog widened to the disposition seam: it
// runs observe at the instant the application prefix becomes durable.
type dispositionAwareOrderLog struct {
	inner   dispositionLogAndRuntimeLog
	observe func()
}

func (l dispositionAwareOrderLog) AppendCommandApplication(ctx context.Context, app runtimecommand.Application) (journal.AppendResult, error) {
	res, err := l.inner.AppendCommandApplication(ctx, app)
	if err == nil && l.observe != nil {
		l.observe()
	}
	return res, err
}

func (l dispositionAwareOrderLog) ReadCommandApplicationAt(ctx context.Context, seq uint64) (runtimecommand.Application, error) {
	return l.inner.ReadCommandApplicationAt(ctx, seq)
}

func (l dispositionAwareOrderLog) AppendCommandDisposition(ctx context.Context, d runtimecommand.CommandDisposition) (journal.AppendResult, error) {
	return l.inner.AppendCommandDisposition(ctx, d)
}

func (l dispositionAwareOrderLog) ScanCommandEffect(ctx context.Context, commandID runtimecommand.CommandID, runtimeID uuid.UUID) (runtimecommand.EffectScan, error) {
	return l.inner.ScanCommandEffect(ctx, commandID, runtimeID)
}

// --- the recovery closure --------------------------------------------------

// attemptCloser obtains the capability the way a caller must: through the segregated
// applier, by assertion. Reaching for *Session directly would prove nothing about
// discoverability.
func attemptCloser(t *testing.T, f *runtimeCommandFixture) runtimecommand.AttemptCloser {
	t.Helper()
	applier, ok := f.session.RuntimeCommands()
	if !ok {
		t.Fatalf("the session does not advertise the applier")
	}
	closer, ok := applier.(runtimecommand.AttemptCloser)
	if !ok {
		t.Fatalf("the applier is not an AttemptCloser")
	}
	return closer
}

func closureFor(commandID runtimecommand.CommandID, runtimeID uuid.UUID, attemptEpoch uint64) runtimecommand.Closure {
	return runtimecommand.Closure{
		CommandID:           commandID,
		RuntimeCommandID:    runtimeID,
		Kind:                runtimecommand.KindInput,
		AttemptID:           "attempt/closed",
		AttemptJournalEpoch: attemptEpoch,
	}
}

// TestAttemptCloserIsDiscoveredByAssertion pins the segregation. The capability is
// found the way session.LeaseEpochReporter is — by asserting on the value — and it
// is deliberately NOT on Applier, so no existing implementer of that interface is
// forced to grow a method it cannot honor.
func TestAttemptCloserIsDiscoveredByAssertion(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	applier, ok := f.session.RuntimeCommands()
	if !ok {
		t.Fatalf("the fixture session does not advertise the applier")
	}
	if _, ok := applier.(runtimecommand.AttemptCloser); !ok {
		t.Fatalf("the live session's applier is not discoverable as an AttemptCloser")
	}
	// And the base interface is unchanged: a value that is only an Applier still
	// compiles as one. If AttemptCloser had been folded into Applier this would not.
	var _ runtimecommand.Applier = applierOnly{}
}

type applierOnly struct{}

func (applierOnly) ApplyRuntimeCommand(context.Context, runtimecommand.Admitted) (runtimecommand.Disposition, error) {
	return runtimecommand.Disposition{}, nil
}

// TestClosureRefusedWithoutAStrictlyLaterGrant is the first guard. The two rows are
// different failures and the error says which: a runtime holding no live grant, and
// a runtime trying to close its OWN attempt. The equal-epoch row is the one that
// matters — it is what an in-place "close my own attempt" bug produces, and the
// released verifier refuses it as author_journal_epoch.
func TestClosureRefusedWithoutAStrictlyLaterGrant(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		attemptEpochDelta int64
		release           bool
		wantHeld          bool
	}{
		"equal grant":   {0, false, true},
		"later attempt": {1, false, true},
		// Delta 0 rather than a lower attempt grant on purpose: a released lease must
		// be refused for want of a GRANT, before the epoch comparison is reached. A row
		// that also failed the comparison could not tell the two refusals apart.
		"released lease": {0, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRuntimeCommandFixture(t)
			epoch := f.lease.Epoch()
			if row.release {
				if err := f.lease.Release(context.Background()); err != nil {
					t.Fatalf("Release: %v", err)
				}
			}
			attemptEpoch := uint64(int64(epoch) + row.attemptEpochDelta)
			closer := attemptCloser(t, f)
			_, err := closer.CloseAttempt(context.Background(), closureFor("v1:c", mustUUID(), attemptEpoch))
			var refusal *runtimecommand.ClosureNotAuthorizedError
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want a *ClosureNotAuthorizedError", err)
			}
			if refusal.Held != row.wantHeld {
				t.Errorf("Held = %v, want %v", refusal.Held, row.wantHeld)
			}
			if got := readDispositions(t, f); len(got) != 0 {
				t.Errorf("a refused closure still wrote %d dispositions: %+v", len(got), got)
			}
		})
	}
}

// TestClosureWritesNotAppliedUnderTheSuccessorGrant is the success path: a successor
// holding a strictly later grant, over a command whose prefix exists but whose
// effect never committed, writes the tombstone.
func TestClosureWritesNotAppliedUnderTheSuccessorGrant(t *testing.T) {
	t.Parallel()
	f, attemptEpoch, commandID, runtimeID := strandedPrefixThenSuccessor(t)
	closer := attemptCloser(t, f)
	res, err := closer.CloseAttempt(context.Background(), closureFor(commandID, runtimeID, attemptEpoch))
	if err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	if !res.Appended || res.Sequence == 0 {
		t.Fatalf("ClosureResult = %+v, want a new durable frame", res)
	}
	got := onlyDisposition(t, f)
	if got.Disposition != runtimecommand.DispositionNotApplied {
		t.Fatalf("disposition = %q, want not_applied", got.Disposition)
	}
	if got.AttemptJournalEpoch != attemptEpoch {
		t.Errorf("attempt grant = %d, want the predecessor's %d", got.AttemptJournalEpoch, attemptEpoch)
	}
	if got.LeaseEpoch != f.lease.Epoch() {
		t.Errorf("author grant = %d, want the successor's live %d", got.LeaseEpoch, f.lease.Epoch())
	}
	if got.LeaseEpoch <= got.AttemptJournalEpoch {
		t.Errorf("author grant %d is not strictly later than the attempt's %d", got.LeaseEpoch, got.AttemptJournalEpoch)
	}
}

// TestClosureIsRefusedOverACommittedEffect is the SECOND guard, and it is the one
// that is not free. The idempotency guard catches a successor colliding with a
// predecessor's durable APPLIED disposition — both key on the attempt id — but it
// cannot catch the case where the predecessor's EFFECT committed and its disposition
// append then failed. There is no colliding record then, and nothing but this scan
// stands between the successor and a tombstone over a real effect, which all future
// grants must honour.
func TestClosureIsRefusedOverACommittedEffect(t *testing.T) {
	t.Parallel()
	f, attemptEpoch, commandID, runtimeID := strandedPrefixThenSuccessor(t)
	// The predecessor's effect: an enduring event caused by that runtime command,
	// appended after the prefix. This is exactly the record a committed input leaves.
	appendEffectEvent(t, f, runtimeID)

	closer := attemptCloser(t, f)
	_, err := closer.CloseAttempt(context.Background(), closureFor(commandID, runtimeID, attemptEpoch))
	var effect *runtimecommand.EnduringEffectError
	if !errors.As(err, &effect) {
		t.Fatalf("err = %v, want an *EnduringEffectError", err)
	}
	if effect.EffectSeq == 0 || effect.EffectSeq <= effect.PrefixSeq {
		t.Errorf("EnduringEffectError = %+v, want an effect sequence above the prefix's", effect)
	}
	if got := readDispositions(t, f); len(got) != 0 {
		t.Fatalf("the closure tombstoned a committed effect: %+v", got)
	}
}

// TestClosureIsRefusedByTheIdempotencyGuardOverADurableApplication is the FIRST
// guard, the free one: the successor's not_applied keys on the same attempt id as
// the predecessor's durable applied disposition, so the append collides and fails
// closed. It is measured separately from the effect guard because they catch
// different journals — this one has a disposition and no effect event.
func TestClosureIsRefusedByTheIdempotencyGuardOverADurableApplication(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	attemptEpoch := f.lease.Epoch()
	runtimeID := mustUUID()
	adm := f.admittedInput("v1:already-applied", runtimeID, "hi")
	adm.AttemptID = "attempt/closed"
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), adm); err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	f.drainOne(t)
	if got := onlyDisposition(t, f); got.Disposition != runtimecommand.DispositionApplied {
		t.Fatalf("setup wrote %q, want applied", got.Disposition)
	}
	successor := takeOver(t, f)

	closer := attemptCloser(t, successor)
	_, err := closer.CloseAttempt(context.Background(), closureFor(adm.CommandID, runtimeID, attemptEpoch))
	if err == nil {
		t.Fatalf("a successor's closure overwrote a durable applied disposition")
	}
	var collision *journal.IdempotencyCollisionError
	var effect *runtimecommand.EnduringEffectError
	if !errors.As(err, &collision) && !errors.As(err, &effect) {
		t.Fatalf("err = %v, want either the idempotency collision or the effect refusal", err)
	}
	got := readDispositions(t, successor)
	if len(got) != 1 || got[0].Disposition != runtimecommand.DispositionApplied {
		t.Fatalf("the journal holds %+v, want only the original applied disposition", got)
	}
}

// strandedPrefixThenSuccessor leaves a durable application prefix whose effect never
// committed, then hands back a fixture over a SUCCESSOR grant on the same journal.
func strandedPrefixThenSuccessor(t *testing.T) (*runtimeCommandFixture, uint64, runtimecommand.CommandID, uuid.UUID) {
	t.Helper()
	f := newRuntimeCommandFixture(t)
	attemptEpoch := f.lease.Epoch()
	runtimeID := mustUUID()
	commandID := runtimecommand.CommandID("v1:stranded")
	if _, err := f.session.runtimeCommands.AppendCommandApplication(context.Background(), runtimecommand.Application{
		CommandID:        commandID,
		RuntimeCommandID: runtimeID,
		LeaseEpoch:       attemptEpoch,
		Kind:             runtimecommand.KindInput,
	}); err != nil {
		t.Fatalf("AppendCommandApplication: %v", err)
	}
	return takeOver(t, f), attemptEpoch, commandID, runtimeID
}

// takeOver releases the fixture's lease and rebuilds it over a strictly later grant
// on the SAME session, which is what a successor runtime is.
func takeOver(t *testing.T, f *runtimeCommandFixture) *runtimeCommandFixture {
	t.Helper()
	if err := f.lease.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	successor := newRuntimeCommandFixtureForSession(t, f.store, f.sid)
	if successor.lease.Epoch() <= f.lease.Epoch() {
		t.Fatalf("successor grant %d is not later than %d", successor.lease.Epoch(), f.lease.Epoch())
	}
	return successor
}

// appendEffectEvent appends one enduring, public event whose Cause.CommandID is the
// runtime command id — the correlation every event a command causes carries.
func appendEffectEvent(t *testing.T, f *runtimeCommandFixture, runtimeID uuid.UUID) {
	t.Helper()
	// TurnStarted is the shape a committed input leaves: enduring, loop-scoped, and
	// carrying the submit command id in Cause.CommandID. Any command-caused enduring
	// event would do; the scan correlates on the cause, never on the event type.
	ev := event.TurnStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: f.sid, LoopID: f.session.activeLoopID, TurnID: mustUUID()},
			EventID:     mustUUID(),
			Cause:       identity.Cause{CommandID: runtimeID, Agency: identity.AgencyUser},
			CreatedAt:   time.Now().UTC(),
		},
		TurnIndex: 1,
	}
	if _, err := f.journal.Append(context.Background(), journal.NewEventRecord(ev)); err != nil {
		t.Fatalf("Append(effect event): %v", err)
	}
}
