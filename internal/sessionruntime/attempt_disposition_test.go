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
// This record does NOT say a turn started, and this test asserts only the record's
// content. That the applied input survives a crash or shutdown before its turn is
// durable (restore replays it) is pinned in admitted_input_durability_test.go.
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
	var unsupported *runtimecommand.DispositionUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want a *runtimecommand.DispositionUnsupportedError", err)
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
	f, attemptEpoch, commandID, runtimeID, _ := strandedPrefixThenSuccessor(t)
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
	f, attemptEpoch, commandID, runtimeID, prefixSeq := strandedPrefixThenSuccessor(t)
	// The predecessor's effect: an enduring event caused by that runtime command.
	// This is exactly the record a committed input leaves.
	effectSeq := appendEffectEvent(t, f, runtimeID)

	closer := attemptCloser(t, f)
	_, err := closer.CloseAttempt(context.Background(), closureFor(commandID, runtimeID, attemptEpoch))
	var effect *runtimecommand.EnduringEffectError
	if !errors.As(err, &effect) {
		t.Fatalf("err = %v, want an *EnduringEffectError", err)
	}
	// Exact sequences, not an ordering. The ordering held here only by construction,
	// so asserting it defended a for-all with a fixed fixture — and the for-all is
	// not even true: the scan refuses an effect at ANY sequence, which
	// TestClosureRefusesAnEffectThatPrecedesThePrefix drives.
	if effect.EffectSeq != effectSeq {
		t.Errorf("EffectSeq = %d, want the effect event's own sequence %d", effect.EffectSeq, effectSeq)
	}
	if effect.PrefixSeq != prefixSeq {
		t.Errorf("PrefixSeq = %d, want the prefix's own sequence %d", effect.PrefixSeq, prefixSeq)
	}
	if got := readDispositions(t, f); len(got) != 0 {
		t.Fatalf("the closure tombstoned a committed effect: %+v", got)
	}
}

// TestClosureOverADurableApplicationReportsAlreadyDisposed: the predecessor's
// applied disposition is durable but the store never settled it (the Host died in
// between). The successor's closure must NOT refuse — Host's settleOrRecover returns
// before settling on any closure error, and the command would never settle — and
// must NOT tombstone: it reports the durable disposition and writes nothing.
//
// The fixture's channel backend appends no events, so the effect guard cannot fire
// and this isolates the already-disposed answer.
func TestClosureOverADurableApplicationReportsAlreadyDisposed(t *testing.T) {
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

	res, err := attemptCloser(t, successor).CloseAttempt(context.Background(), closureFor(adm.CommandID, runtimeID, attemptEpoch))
	if err != nil {
		t.Fatalf("CloseAttempt = %v, want the already-disposed success", err)
	}
	if res.AlreadyDisposed != runtimecommand.DispositionApplied || res.Appended || res.Sequence == 0 {
		t.Fatalf("result = %+v, want AlreadyDisposed=applied, nothing appended, the disposition's sequence", res)
	}
	got := readDispositions(t, successor)
	if len(got) != 1 || got[0].Disposition != runtimecommand.DispositionApplied {
		t.Fatalf("the journal holds %+v, want only the original applied disposition", got)
	}
}

// TestClosureOverAnotherAttemptsDispositionStillGuards: a durable disposition naming
// a DIFFERENT attempt of the same command is not this attempt's outcome, so the
// closer falls through to its ordinary guards rather than answering for it.
func TestClosureOverAnotherAttemptsDispositionStillGuards(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	attemptEpoch := f.lease.Epoch()
	runtimeID := mustUUID()
	adm := f.admittedInput("v1:other-attempt", runtimeID, "hi")
	adm.AttemptID = "attempt/other"
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), adm); err != nil {
		t.Fatalf("ApplyRuntimeCommand: %v", err)
	}
	f.drainOne(t)
	successor := takeOver(t, f)
	res, err := attemptCloser(t, successor).CloseAttempt(context.Background(), closureFor(adm.CommandID, runtimeID, attemptEpoch))
	if err != nil {
		t.Fatalf("CloseAttempt = %v", err)
	}
	if res.AlreadyDisposed != "" || !res.Appended {
		t.Fatalf("result = %+v, want a fresh not_applied for the other attempt", res)
	}
}

// TestClosureRefusesAConflictingDispositionForTheAttempt: a durable disposition
// naming THIS attempt under another runtime mapping is a conflict, as the store's
// own evidence reader treats it — never an already-disposed success. The journal
// holds no prefix, so the prefix mapping guard cannot be what refuses.
func TestClosureRefusesAConflictingDispositionForTheAttempt(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	attemptEpoch := f.lease.Epoch()
	commandID := runtimecommand.CommandID("v1:conflict")
	if _, err := f.session.runtimeCommands.(dispositionLog).AppendCommandDisposition(context.Background(), runtimecommand.CommandDisposition{
		CommandID: commandID, RuntimeCommandID: mustUUID(), Kind: runtimecommand.KindInput, LeaseEpoch: attemptEpoch,
		AttemptID: "attempt/closed", AttemptJournalEpoch: attemptEpoch, Disposition: runtimecommand.DispositionApplied,
	}); err != nil {
		t.Fatalf("AppendCommandDisposition: %v", err)
	}
	successor := takeOver(t, f)
	_, err := attemptCloser(t, successor).CloseAttempt(context.Background(), closureFor(commandID, mustUUID(), attemptEpoch))
	var conflict *runtimecommand.MappingConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("CloseAttempt = %v, want *MappingConflictError", err)
	}
}

// strandedPrefixThenSuccessor leaves a durable application prefix whose effect never
// committed, then hands back a fixture over a SUCCESSOR grant on the same journal.
func strandedPrefixThenSuccessor(t *testing.T) (*runtimeCommandFixture, uint64, runtimecommand.CommandID, uuid.UUID, uint64) {
	t.Helper()
	f := newRuntimeCommandFixture(t)
	attemptEpoch := f.lease.Epoch()
	runtimeID := mustUUID()
	commandID := runtimecommand.CommandID("v1:stranded")
	res, err := f.session.runtimeCommands.AppendCommandApplication(context.Background(), runtimecommand.Application{
		CommandID:        commandID,
		RuntimeCommandID: runtimeID,
		LeaseEpoch:       attemptEpoch,
		Kind:             runtimecommand.KindInput,
	})
	if err != nil {
		t.Fatalf("AppendCommandApplication: %v", err)
	}
	return takeOver(t, f), attemptEpoch, commandID, runtimeID, res.Sequence
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
func appendEffectEvent(t *testing.T, f *runtimeCommandFixture, runtimeID uuid.UUID) uint64 {
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
	seq, err := f.journal.Append(context.Background(), journal.NewEventRecord(ev))
	if err != nil {
		t.Fatalf("Append(effect event): %v", err)
	}
	return seq
}

// --- the recovery scan over a journal that holds MORE THAN ONE of things --------

// readApplications returns every application prefix in the journal, in ledger order,
// with its sequence. It exists so a multi-command fixture can prove it really built
// the shape it claims rather than asserting against a journal that quietly holds one
// record.
func readApplications(t *testing.T, f *runtimeCommandFixture) map[runtimecommand.CommandID]uint64 {
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
	out := map[runtimecommand.CommandID]uint64{}
	for {
		rec, seq, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if app, ok := rec.(journal.CommandApplicationRecord); ok {
			out[app.Application().CommandID] = seq
		}
	}
}

// appendForeignPrefix writes another command's application prefix, which is what
// every session that has applied more than one command has in its journal.
func appendForeignPrefix(t *testing.T, f *runtimeCommandFixture, id runtimecommand.CommandID) {
	t.Helper()
	if _, err := f.session.runtimeCommands.AppendCommandApplication(context.Background(), runtimecommand.Application{
		CommandID:        id,
		RuntimeCommandID: mustUUID(),
		LeaseEpoch:       f.lease.Epoch(),
		Kind:             runtimecommand.KindInterrupt,
	}); err != nil {
		t.Fatalf("AppendCommandApplication(%s): %v", id, err)
	}
}

// multiCommandJournal builds the journal shape EVERY REAL SESSION has by the time a
// recovery closure runs: several commands' application prefixes, not one. The target
// is deliberately in the MIDDLE, so a scan that took the first or the last record
// rather than the matching one is wrong in both directions.
//
// It returns the successor fixture, the attempt's grant, and the target's identity
// and prefix sequence.
func multiCommandJournal(t *testing.T) (succ *runtimeCommandFixture, attemptEpoch uint64,
	commandID runtimecommand.CommandID, runtimeID uuid.UUID, prefixSeq uint64, effectSeqs []uint64,
) {
	t.Helper()
	f := newRuntimeCommandFixture(t)
	attemptEpoch = f.lease.Epoch()
	commandID, runtimeID = "v1:target", mustUUID()

	appendForeignPrefix(t, f, "v1:earlier")
	res, err := f.session.runtimeCommands.AppendCommandApplication(context.Background(), runtimecommand.Application{
		CommandID: commandID, RuntimeCommandID: runtimeID, LeaseEpoch: attemptEpoch, Kind: runtimecommand.KindInput,
	})
	if err != nil {
		t.Fatalf("AppendCommandApplication(target): %v", err)
	}
	prefixSeq = res.Sequence
	// A LATER foreign prefix is the half that kills a scan which simply keeps the
	// last application record it saw — which is what dropping the comparison does.
	appendForeignPrefix(t, f, "v1:later")

	// Non-vacuity: if the fixture ever degenerates to one prefix, the comparison
	// under test has nothing to distinguish again and this test goes quietly back to
	// proving nothing.
	apps := readApplications(t, f)
	if len(apps) != 3 {
		t.Fatalf("the journal holds %d application prefixes, want 3: %v", len(apps), apps)
	}
	if apps[commandID] != prefixSeq {
		t.Fatalf("the target prefix is at %d, want %d", apps[commandID], prefixSeq)
	}
	if apps["v1:earlier"] >= prefixSeq || apps["v1:later"] <= prefixSeq {
		t.Fatalf("the target is not between the two foreign prefixes: %v", apps)
	}
	return f, attemptEpoch, commandID, runtimeID, prefixSeq, nil
}

// TestClosureScanCorrelatesTheTargetCommandAmongOthers is the row that was missing,
// and the reason it was missing is worth stating: the comparison it exercises —
// ScanCommandEffect's app.CommandID == commandID — is fed by HOW MANY COMMANDS A
// JOURNAL HOLDS, which is a structural property of the fixture rather than a value
// any fixture varied. Every other fixture in this suite writes exactly one
// application prefix, so the comparison had nothing to distinguish and could be
// replaced by a constant with the whole suite still green.
//
// A journal holding several commands' prefixes is not exotic. It is the normal shape
// of any session that has applied more than one runtime command, which is every real
// session by the time a recovery closure runs.
//
// What breaks without the comparison is LIVENESS, not safety: the scan keeps the last
// application record it saw, so a legitimate closure is refused as a mapping conflict
// against a command it was never about. It cannot tombstone an effect — the
// EffectFound arm is separately rowed — but a closure that can never succeed leaves
// the command applying forever, which is the exact liveness failure the refused arm
// exists to prevent elsewhere.
func TestClosureScanCorrelatesTheTargetCommandAmongOthers(t *testing.T) {
	t.Parallel()
	f, attemptEpoch, commandID, runtimeID, _, _ := multiCommandJournal(t)
	succ := takeOver(t, f)

	res, err := attemptCloser(t, succ).CloseAttempt(context.Background(), closureFor(commandID, runtimeID, attemptEpoch))
	if err != nil {
		var conflict *runtimecommand.MappingConflictError
		if errors.As(err, &conflict) {
			t.Fatalf("the scan correlated a FOREIGN prefix: %+v", conflict)
		}
		t.Fatalf("CloseAttempt over a multi-command journal: %v", err)
	}
	if !res.Appended || res.Sequence == 0 {
		t.Fatalf("ClosureResult = %+v, want a new durable frame", res)
	}
	got := onlyDisposition(t, succ)
	if got.Disposition != runtimecommand.DispositionNotApplied || got.CommandID != commandID {
		t.Fatalf("disposition = %+v, want a not_applied closure for %q", got, commandID)
	}
}

// TestClosureRefusalNamesTheTargetsOwnPrefixAndFirstEffect is the same structural
// blind spot measured on the arm that REFUSES, and it closes a second comparison the
// value-axis sweep also missed for the same reason.
//
// Two structural properties are varied here that no other fixture varies:
//
//   - HOW MANY COMMANDS the journal holds. Without the correlation the refusal names
//     another command's prefix, so an operator reading EnduringEffectError goes to a
//     record that has nothing to do with the attempt.
//   - HOW MANY EFFECT EVENTS the target caused. ScanCommandEffect keeps the FIRST
//     (`!scan.EffectFound`), and with one event per fixture "first" and "last" are the
//     same record. A turn that emits several events for one command is ordinary, and
//     the first is the one that proves the effect committed — the last merely proves
//     it was still going.
func TestClosureRefusalNamesTheTargetsOwnPrefixAndFirstEffect(t *testing.T) {
	t.Parallel()
	f, attemptEpoch, commandID, runtimeID, prefixSeq, _ := multiCommandJournal(t)

	// The target's effect commits, twice — an ordinary multi-event turn.
	firstEffect := appendEffectEvent(t, f, runtimeID)
	secondEffect := appendEffectEvent(t, f, runtimeID)
	if firstEffect >= secondEffect {
		t.Fatalf("the two effect events are not ordered: %d, %d", firstEffect, secondEffect)
	}
	// And another command's effect after them, so "the last event caused by anything"
	// is not the target's either.
	appendEffectEvent(t, f, mustUUID())
	succ := takeOver(t, f)

	_, err := attemptCloser(t, succ).CloseAttempt(context.Background(), closureFor(commandID, runtimeID, attemptEpoch))
	var effect *runtimecommand.EnduringEffectError
	if !errors.As(err, &effect) {
		t.Fatalf("err = %v, want an *EnduringEffectError", err)
	}
	if effect.PrefixSeq != prefixSeq {
		t.Errorf("PrefixSeq = %d, want the TARGET's own prefix at %d — a foreign prefix was correlated",
			effect.PrefixSeq, prefixSeq)
	}
	if effect.EffectSeq != firstEffect {
		t.Errorf("EffectSeq = %d, want the FIRST effect at %d (the second is at %d)",
			effect.EffectSeq, firstEffect, secondEffect)
	}
	if got := readDispositions(t, succ); len(got) != 0 {
		t.Fatalf("the closure tombstoned a committed effect: %+v", got)
	}
}

// TestClosureSucceedsWhenAnotherCommandsEffectIsInTheJournal is the THIRD comparison
// the value-axis sweep missed, found by applying the corrected structural lens rather
// than by a review: ScanCommandEffect's Cause.CommandID == runtimeID is fed by HOW
// MANY DISTINCT CAUSES the journal's events have, and every other fixture has either
// no events at all or only the target's.
//
// The shape here is ordinary: a session applied command A, which emitted its events,
// and then command B, which stranded. Closing B must succeed. Without the cause
// correlation the scan takes A's event as B's effect and refuses the closure forever,
// so B sits applying with nothing able to settle it — the same liveness failure the
// refused arm exists to prevent, reached from the recovery side.
//
// The assertion is deliberately the SUCCESS, not an error: this is the arm where a
// spurious refusal is the defect, and a test that only checked refusals could never
// see it.
func TestClosureSucceedsWhenAnotherCommandsEffectIsInTheJournal(t *testing.T) {
	t.Parallel()
	f, attemptEpoch, commandID, runtimeID, _, _ := multiCommandJournal(t)

	// Another command's turn, complete with the events it caused. The target caused
	// NONE — that is what makes its closure legitimate.
	otherRuntimeID := mustUUID()
	if seq := appendEffectEvent(t, f, otherRuntimeID); seq == 0 {
		t.Fatalf("the foreign effect event did not land")
	}
	if seq := appendEffectEvent(t, f, otherRuntimeID); seq == 0 {
		t.Fatalf("the second foreign effect event did not land")
	}
	if otherRuntimeID == runtimeID {
		t.Fatalf("the two runtime ids collided, so this test proves nothing")
	}
	succ := takeOver(t, f)

	res, err := attemptCloser(t, succ).CloseAttempt(context.Background(), closureFor(commandID, runtimeID, attemptEpoch))
	if err != nil {
		var effect *runtimecommand.EnduringEffectError
		if errors.As(err, &effect) {
			t.Fatalf("another command's event was read as this attempt's effect: %+v", effect)
		}
		t.Fatalf("CloseAttempt: %v", err)
	}
	if !res.Appended {
		t.Fatalf("ClosureResult = %+v, want a new durable frame", res)
	}
	got := onlyDisposition(t, succ)
	if got.Disposition != runtimecommand.DispositionNotApplied {
		t.Fatalf("disposition = %q, want not_applied", got.Disposition)
	}
}

// TestClosureRefusesAnEffectThatPrecedesThePrefix pins the ScanCommandEffect
// CONTRACT, which four doc comments previously stated one way and the code the other:
// the scan refuses on an event caused by the attempt's runtime command at ANY
// sequence, not only after the application prefix.
//
// Nothing distinguished the two readings, so the exported contract on
// EnduringEffectError — which Host is told to handle — could have shipped on an
// immutable tag saying something the code does not do.
//
// The code's reading is the one kept, and the reason is asymmetric cost. An event
// carrying that runtime id in its cause cannot exist unless the command was
// dispatched, so its position proves nothing extra. Restricting the match to
// "after the prefix" would mean that in the one journal shape nobody can explain —
// an effect with no prefix before it — the scan answered "no effect" and licensed a
// TOMBSTONE. Refusing an odd journal costs liveness; tombstoning a committed effect
// is unrecoverable.
func TestClosureRefusesAnEffectThatPrecedesThePrefix(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	attemptEpoch := f.lease.Epoch()
	runtimeID := mustUUID()
	commandID := runtimecommand.CommandID("v1:effect-first")

	// The effect lands BEFORE the prefix. This inverts the ordering every other
	// fixture builds, which is the whole point: the ordering was previously an
	// artefact of construction that a fixed fixture then asserted as a for-all.
	effectSeq := appendEffectEvent(t, f, runtimeID)
	res, err := f.session.runtimeCommands.AppendCommandApplication(context.Background(), runtimecommand.Application{
		CommandID: commandID, RuntimeCommandID: runtimeID, LeaseEpoch: attemptEpoch, Kind: runtimecommand.KindInput,
	})
	if err != nil {
		t.Fatalf("AppendCommandApplication: %v", err)
	}
	if effectSeq >= res.Sequence {
		t.Fatalf("the effect at %d does not precede the prefix at %d, so this test proves nothing",
			effectSeq, res.Sequence)
	}
	succ := takeOver(t, f)

	_, err = attemptCloser(t, succ).CloseAttempt(context.Background(), closureFor(commandID, runtimeID, attemptEpoch))
	var effect *runtimecommand.EnduringEffectError
	if !errors.As(err, &effect) {
		t.Fatalf("err = %v, want an *EnduringEffectError — an effect before the prefix is still an effect", err)
	}
	if effect.EffectSeq != effectSeq {
		t.Errorf("EffectSeq = %d, want %d", effect.EffectSeq, effectSeq)
	}
	// And the documented consequence of the contract: the two sequences carry NO
	// ordering relationship. A reader that assumed one would be wrong here.
	if effect.EffectSeq >= effect.PrefixSeq {
		t.Errorf("EnduringEffectError = %+v, want the effect BELOW the prefix in this journal", effect)
	}
	if got := readDispositions(t, succ); len(got) != 0 {
		t.Fatalf("the closure tombstoned an effect that preceded its prefix: %+v", got)
	}
}

// TestClosureFailsClosedWhenTheScanCannotAnswer is the closer's half of the
// fail-closed contract: a scan that returns an error must STOP the closure, not be
// discarded so it proceeds on a zero EffectScan.
//
// The failure is injected at the seam rather than by corrupting a ledger, and that is
// deliberate. An earlier version of this test corrupted the journal directly and
// PASSED FOR THE WRONG REASON: appending raw bytes behind the journal's back leaves
// its tracked tip stale, so the closure's own append failed on the CAS and the test
// could not tell that outcome from the scan refusal it claimed to measure. It passed
// identically with the guard removed. A double makes the scan's error the only
// variable.
//
// The scan's OWN half — that it returns an error rather than reporting an unreadable
// journal as "no effect" — is measured where it lives, over a real corrupted ledger,
// by sessionstore's TestScanCommandEffectFailsClosedOnAnUnreadableFrame.
func TestClosureFailsClosedWhenTheScanCannotAnswer(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	attemptEpoch := f.lease.Epoch()
	runtimeID := mustUUID()
	commandID := runtimecommand.CommandID("v1:scan-fails")
	if _, err := f.session.runtimeCommands.AppendCommandApplication(context.Background(), runtimecommand.Application{
		CommandID: commandID, RuntimeCommandID: runtimeID, LeaseEpoch: attemptEpoch, Kind: runtimecommand.KindInput,
	}); err != nil {
		t.Fatalf("AppendCommandApplication: %v", err)
	}
	succ := takeOver(t, f)

	// CONTROL: with a working scan this exact closure SUCCEEDS. Without it, a refusal
	// below would prove nothing — the closure might be refused for any other reason.
	control := *succ
	if _, err := attemptCloser(t, &control).CloseAttempt(context.Background(),
		closureFor(commandID, runtimeID, attemptEpoch)); err != nil {
		t.Fatalf("control: the closure failed for a reason other than the scan: %v", err)
	}

	// Now the same shape, on a fresh successor, with only the scan broken.
	f2 := newRuntimeCommandFixture(t)
	attemptEpoch2 := f2.lease.Epoch()
	runtimeID2 := mustUUID()
	if _, err := f2.session.runtimeCommands.AppendCommandApplication(context.Background(), runtimecommand.Application{
		CommandID: commandID, RuntimeCommandID: runtimeID2, LeaseEpoch: attemptEpoch2, Kind: runtimecommand.KindInput,
	}); err != nil {
		t.Fatalf("AppendCommandApplication: %v", err)
	}
	succ2 := takeOver(t, f2)
	wedged := errors.New("the journal could not be walked")
	succ2.session.runtimeCommands = failingScanLog{
		dispositionLogAndRuntimeLog: succ2.session.runtimeCommands.(dispositionLogAndRuntimeLog),
		err:                         wedged,
	}

	_, err := attemptCloser(t, succ2).CloseAttempt(context.Background(),
		closureFor(commandID, runtimeID2, attemptEpoch2))
	if !errors.Is(err, wedged) {
		t.Fatalf("err = %v, want the scan's own error propagated", err)
	}
	if got := readDispositions(t, succ2); len(got) != 0 {
		t.Fatalf("a tombstone was written over a journal the scan could not read: %+v", got)
	}
}

// failingScanLog is a real log whose SCAN alone fails, so a test can vary that one
// answer without disturbing the appends around it.
type failingScanLog struct {
	dispositionLogAndRuntimeLog
	err error
}

func (l failingScanLog) ScanCommandEffect(context.Context, runtimecommand.CommandID, uuid.UUID) (runtimecommand.EffectScan, error) {
	return runtimecommand.EffectScan{}, l.err
}

// --- the mapping guard, which sits BEHIND the grant guards ---------------------

// TestClosureMappingGuardDiscriminatesOnTheDurableBinding rows the two comparisons in
// CloseAttempt's mapping guard that had none, and the reason they had none is the
// sweep defect this round exists to fix: the guard sits at :142, BEHIND five
// early-return guards, so every row cited as covering it actually returns at :134 or
// earlier and never reaches it. A booking that names covering rows has to be checked
// against where those rows RETURN.
//
// The guard exists so a closure cannot tombstone a command whose durable prefix binds
// it to something else. Both halves of its disjunction are rowed here, each isolated:
// the runtime-id row keeps the kind equal, and the kind row keeps the runtime id
// equal, so a row that fails names the comparison that stopped discriminating.
func TestClosureMappingGuardDiscriminatesOnTheDurableBinding(t *testing.T) {
	t.Parallel()
	commandID := runtimecommand.CommandID("v1:mapping")
	for name, row := range map[string]struct {
		durableKind      runtimecommand.Kind
		differentRuntime bool
	}{
		"a different runtime id": {runtimecommand.KindInput, true},
		"a different kind":       {runtimecommand.KindInterrupt, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRuntimeCommandFixture(t)
			attemptEpoch := f.lease.Epoch()
			offered := mustUUID()
			durable := offered
			if row.differentRuntime {
				durable = mustUUID()
			}
			if _, err := f.session.runtimeCommands.AppendCommandApplication(context.Background(), runtimecommand.Application{
				CommandID: commandID, RuntimeCommandID: durable, LeaseEpoch: attemptEpoch, Kind: row.durableKind,
			}); err != nil {
				t.Fatalf("AppendCommandApplication: %v", err)
			}
			succ := takeOver(t, f)

			// closureFor offers KindInput, so the kind row disagrees on kind alone and
			// the runtime-id row disagrees on the id alone.
			_, err := attemptCloser(t, succ).CloseAttempt(context.Background(),
				closureFor(commandID, offered, attemptEpoch))
			var conflict *runtimecommand.MappingConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("err = %v, want a *MappingConflictError", err)
			}
			if conflict.DurableRuntimeID != durable || conflict.DurableKind != row.durableKind {
				t.Errorf("conflict reports durable (%v, %q), want (%v, %q)",
					conflict.DurableRuntimeID, conflict.DurableKind, durable, row.durableKind)
			}
			if conflict.RuntimeCommandID != offered || conflict.Kind != runtimecommand.KindInput {
				t.Errorf("conflict reports offered (%v, %q), want (%v, %q)",
					conflict.RuntimeCommandID, conflict.Kind, offered, runtimecommand.KindInput)
			}
			if got := readDispositions(t, succ); len(got) != 0 {
				t.Fatalf("a closure was written against a conflicting durable mapping: %+v", got)
			}
		})
	}
}

// TestClosureSucceedsWhenTheJournalHoldsNoPrefixAtAll is the mapping guard's NEGATIVE
// row — the case where it must stay SILENT — and it is the one the sweep could not
// have reached by any existing path.
//
// `scan.PrefixSeq != 0` was booked as covered by the grant-refusal rows. It is not:
// those return at the grant check, four guards before the scan runs. Nothing reached
// this comparison, and without it a closure over a journal with no prefix compares
// the offered runtime id against the ZERO uuid, finds them different, and refuses
// forever — which is precisely the shape recovery exists for. A predecessor that
// crashed BEFORE writing its prefix is the most ordinary thing a successor closes.
//
// Its assertion is a success, for the same reason the cause-correlation row's is: a
// guard that refuses everything passes every test that only checks refusals.
func TestClosureSucceedsWhenTheJournalHoldsNoPrefixAtAll(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	attemptEpoch := f.lease.Epoch()
	runtimeID := mustUUID()
	commandID := runtimecommand.CommandID("v1:never-prefixed")

	// Non-vacuity: the journal really holds no prefix for this command. Another
	// command's prefix IS present, so "no prefix" is a fact about this command rather
	// than about an empty journal — an empty journal would also pass a scan that had
	// stopped working entirely.
	appendForeignPrefix(t, f, "v1:someone-else")
	if apps := readApplications(t, f); len(apps) != 1 {
		t.Fatalf("the journal holds %d prefixes, want exactly the foreign one: %v", len(apps), apps)
	} else if _, ok := apps[commandID]; ok {
		t.Fatalf("the target command has a prefix, so this test proves nothing")
	}
	succ := takeOver(t, f)

	res, err := attemptCloser(t, succ).CloseAttempt(context.Background(),
		closureFor(commandID, runtimeID, attemptEpoch))
	if err != nil {
		var conflict *runtimecommand.MappingConflictError
		if errors.As(err, &conflict) {
			t.Fatalf("an absent prefix was read as a conflicting mapping: %+v", conflict)
		}
		t.Fatalf("CloseAttempt over a journal with no prefix for this command: %v", err)
	}
	if !res.Appended {
		t.Fatalf("ClosureResult = %+v, want a new durable frame", res)
	}
	got := onlyDisposition(t, succ)
	if got.Disposition != runtimecommand.DispositionNotApplied || got.CommandID != commandID {
		t.Fatalf("disposition = %+v, want a not_applied closure for %q", got, commandID)
	}
}
