package sessionruntime

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// This file pins the two properties gate_response's correctness argument rests on
// but that the single-answer tests cannot reach: the gate directory's exactly-once
// CLAIM under two answers, and the journal's refusal to advance its tracked tip on
// a failed append. They were first written as probes by the v0.35.0 quality gate
// (TestQTwoGateResponsesSameGate, TestQSecondAnswerDuringClaimWindow,
// TestQCrashAmbiguousGateResolvedAppend) and are adopted here so a regression in
// either property fails this package.

func admittedAs(f *gateResponseFixture, commandID, attempt string, runtimeID uuid.UUID, r gate.GateResponse) runtimecommand.Admitted {
	a := f.admittedGateResponse(runtimeID, r)
	a.CommandID = runtimecommand.CommandID(commandID)
	a.AttemptID = runtimecommand.AttemptID(attempt)
	return a
}

func gateCommandID(c command.Command) uuid.UUID {
	switch v := c.(type) {
	case command.ApproveToolCall:
		return v.Header.CommandID
	case command.DenyToolCall:
		return v.Header.CommandID
	case command.ProvideUserInput:
		return v.Header.CommandID
	}
	return uuid.UUID{}
}

func drainGateCommands(f *gateResponseFixture, wait time.Duration) []command.Command {
	var out []command.Command
	deadline := time.After(wait)
	for {
		select {
		case c := <-f.cmds:
			out = append(out, c)
		case <-deadline:
			return out
		}
	}
}

func dispositionsByRuntimeID(t *testing.T, f *gateResponseFixture) map[uuid.UUID]runtimecommand.DispositionKind {
	t.Helper()
	by := map[uuid.UUID]runtimecommand.DispositionKind{}
	for _, d := range readDispositions(t, f.runtimeCommandFixture) {
		by[d.RuntimeCommandID] = d.Disposition
	}
	return by
}

// TestTwoGateResponsesForOneGateSettleExactlyOneAnswer races two distinct commands
// answering one gate. Whichever wins, exactly one GateResolved lands (carrying the
// winner's runtime id), exactly one command reaches the loop, the winner settles
// applied and the loser no_op, and the settlement reader agrees for both. A claim
// that stopped being exclusive would land two answers and settle both applied.
func TestTwoGateResponsesForOneGateSettleExactlyOneAnswer(t *testing.T) {
	t.Parallel()
	const iterations = 40
	for i := range iterations {
		f := newGateResponseFixture(t)
		gateID := f.openGate(t, permissionGate(), bashPayload())
		ra, rb := mustUUID(), mustUUID()
		a := admittedAs(f, "v1:a", "attempt/a", ra, userResponse(gateID, string(gate.ApprovalApprove)))
		b := admittedAs(f, "v1:b", "attempt/b", rb, userResponse(gateID, string(gate.ApprovalDeny)))
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for j, adm := range []runtimecommand.Admitted{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[j] = f.session.ApplyRuntimeCommand(context.Background(), adm)
			}()
		}
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("iteration %d: errors %v / %v", i, errs[0], errs[1])
		}
		resolutions := readGateResolutions(t, f.runtimeCommandFixture)
		if len(resolutions) != 1 {
			t.Fatalf("iteration %d: %d GateResolved, want exactly 1", i, len(resolutions))
		}
		by := dispositionsByRuntimeID(t, f)
		if len(by) != 2 {
			t.Fatalf("iteration %d: %d dispositions, want 2", i, len(by))
		}
		var winner uuid.UUID
		switch {
		case by[ra] == runtimecommand.DispositionApplied && by[rb] == runtimecommand.DispositionNoOp:
			winner = ra
		case by[rb] == runtimecommand.DispositionApplied && by[ra] == runtimecommand.DispositionNoOp:
			winner = rb
		default:
			t.Fatalf("iteration %d: dispositions %+v, want one applied and one no_op", i, by)
		}
		if resolutions[0].Cause.CommandID != winner {
			t.Fatalf("iteration %d: GateResolved cause %v, want the winner %v", i, resolutions[0].Cause.CommandID, winner)
		}
		if cmds := drainGateCommands(f, 30*time.Millisecond); len(cmds) != 1 || gateCommandID(cmds[0]) != winner {
			t.Fatalf("iteration %d: dispatched %d commands, want 1 carrying the winner's id", i, len(cmds))
		}
		for _, adm := range []runtimecommand.Admitted{a, b} {
			if evidence := settle(t, f.runtimeCommandFixture, adm); string(evidence.Kind) != string(by[adm.RuntimeCommandID]) {
				t.Fatalf("iteration %d: evidence %q, disposition %q", i, evidence.Kind, by[adm.RuntimeCommandID])
			}
		}
	}
}

// blockingResolveAppender holds the FIRST GateResolved append until released, so a
// test can deliver a second answer while the first holds the claim.
type blockingResolveAppender struct {
	gateAppender
	entered chan struct{}
	release chan error // nil delegates; non-nil fails the held append with it
	once    sync.Once
	calls   atomic.Int32
}

func (a *blockingResolveAppender) AppendGateResolved(ctx context.Context, ev event.GateResolved) error {
	if a.calls.Add(1) == 1 {
		a.once.Do(func() { close(a.entered) })
		select {
		case err := <-a.release:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return a.gateAppender.AppendGateResolved(ctx, ev)
}

// TestSecondAnswerDuringTheClaimWindowIsNoOp forces the interleaving the race above
// only samples: B arrives while A holds the claim. B settles no_op either way. If
// A's append succeeds, A is applied and the gate is closed; if it fails, A is
// refused, nothing is resolved, and the gate is answerable again under a new
// command — the GateNotReady caveat, measured.
func TestSecondAnswerDuringTheClaimWindowIsNoOp(t *testing.T) {
	t.Parallel()
	for _, failFirst := range []bool{false, true} {
		t.Run("first_append_fails="+strconv.FormatBool(failFirst), func(t *testing.T) {
			t.Parallel()
			f := newGateResponseFixture(t)
			held := &blockingResolveAppender{gateAppender: f.session.gateAppender, entered: make(chan struct{}), release: make(chan error, 1)}
			f.session.gateAppender = held
			gateID := f.openGate(t, permissionGate(), bashPayload())
			ra, rb := mustUUID(), mustUUID()
			a := admittedAs(f, "v1:a", "attempt/a", ra, userResponse(gateID, string(gate.ApprovalApprove)))
			b := admittedAs(f, "v1:b", "attempt/b", rb, userResponse(gateID, string(gate.ApprovalDeny)))
			aDone := make(chan error, 1)
			go func() { _, err := f.session.ApplyRuntimeCommand(context.Background(), a); aDone <- err }()
			<-held.entered
			if _, err := f.session.ApplyRuntimeCommand(context.Background(), b); err != nil {
				t.Fatalf("B during A's claim: %v", err)
			}
			if failFirst {
				held.release <- errors.New("injected GateResolved failure")
			} else {
				held.release <- nil
			}
			<-aDone
			by := dispositionsByRuntimeID(t, f)
			resolved := len(readGateResolutions(t, f.runtimeCommandFixture))
			open := len(f.session.ListGates(context.Background()))
			if by[rb] != runtimecommand.DispositionNoOp {
				t.Fatalf("B = %q, want no_op", by[rb])
			}
			if !failFirst {
				if by[ra] != runtimecommand.DispositionApplied || resolved != 1 || open != 0 {
					t.Fatalf("A = %q, GateResolved = %d, open = %d; want applied, 1, 0", by[ra], resolved, open)
				}
				return
			}
			if by[ra] != runtimecommand.DispositionRefused || resolved != 0 || open != 1 {
				t.Fatalf("A = %q, GateResolved = %d, open = %d; want refused, 0, 1", by[ra], resolved, open)
			}
			rc := mustUUID()
			c := admittedAs(f, "v1:c", "attempt/c", rc, userResponse(gateID, string(gate.ApprovalApprove)))
			if _, err := f.session.ApplyRuntimeCommand(context.Background(), c); err != nil {
				t.Fatalf("C after the reopen: %v", err)
			}
			if got := dispositionsByRuntimeID(t, f)[rc]; got != runtimecommand.DispositionApplied {
				t.Fatalf("C = %q, want applied", got)
			}
		})
	}
}

// faultLedger wraps a real ledger and turns ONE append into an ambiguous ack —
// optionally after actually writing it — with AppendDefinite's single retry also
// ambiguous. It injects below the real sessionstore journal, so the journal's own
// tracked-tip handling is what is under test.
type faultLedger struct {
	storage.Ledger
	mu     sync.Mutex
	skip   int
	landed bool
	armed  bool
	retry  bool
	fired  int
}

func (l *faultLedger) arm(skip int, landed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.skip, l.landed, l.armed, l.retry = skip, landed, true, false
}

func (l *faultLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	l.mu.Lock()
	switch {
	case l.armed && l.retry:
		l.armed, l.retry = false, false
		l.mu.Unlock()
		return &storage.AmbiguousError{Name: name, Expected: expected, Cause: errors.New("injected: retry also ambiguous")}
	case l.armed && l.skip == 0:
		l.retry = true
		l.fired++
		landed := l.landed
		l.mu.Unlock()
		if landed {
			if err := l.Ledger.Append(ctx, name, expected, payload); err != nil {
				return err
			}
		}
		return &storage.AmbiguousError{Name: name, Expected: expected, Cause: errors.New("injected ambiguous ack")}
	case l.armed:
		l.skip--
	}
	l.mu.Unlock()
	return l.Ledger.Append(ctx, name, expected, payload)
}

// TestAmbiguousGateResolvedAppendNeverContradictsTheJournal is the ordering argument
// in gateResponseDisposition, driven at the storage ledger:
//
//   - LANDED: the GateResolved frame is written but acknowledged ambiguously. The
//     journal must not advance its tip, so the refused frame's CAS collides with the
//     landed answer and NO disposition is written. The session is faulted; neither a
//     direct answer, the gate's policy timer nor a second command lands a second
//     GateResolved; and a successor's closure is refused over the landed answer.
//   - NOT LANDED: nothing is written, and refused is recorded — the truth.
//
// A journal that advanced its tip on a failed append would write refused over a
// landed answer; the landed row fails on exactly that.
func TestAmbiguousGateResolvedAppendNeverContradictsTheJournal(t *testing.T) {
	t.Parallel()
	for _, landed := range []bool{true, false} {
		t.Run("landed="+strconv.FormatBool(landed), func(t *testing.T) {
			t.Parallel()
			backend := memstore.New()
			composite := *backend
			ledger := &faultLedger{Ledger: backend.Ledger}
			composite.Ledger = ledger
			store, err := sessionstore.Open(&composite)
			if err != nil {
				t.Fatalf("sessionstore.Open over the fault ledger: %v", err)
			}
			f := wireGates(t, newRuntimeCommandFixtureOver(t, store), nil)
			g := permissionGate()
			g.ResponsePolicy = gate.ResponsePolicy{Timeout: 500 * time.Millisecond, OnTimeout: gate.PolicyRespond,
				Response: gate.ResponseTemplate{Action: string(gate.ApprovalDeny)}}
			gateID := f.openGate(t, g, bashPayload())
			attemptEpoch := f.lease.Epoch()
			runtimeID := mustUUID()
			adm := admittedAs(f, "v1:crash", "attempt/crash", runtimeID, userResponse(gateID, string(gate.ApprovalApprove)))

			ledger.arm(1, landed) // the prefix passes; the GateResolved append is the faulted one
			disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
			var gateErr *GateError
			if !errors.As(err, &gateErr) || gateErr.Kind != GateAppendFailed || disp.PrefixSequence == 0 {
				t.Fatalf("apply = %+v / %v, want the prefix plus GateAppendFailed", disp, err)
			}
			if ledger.fired != 1 {
				t.Fatalf("the fault fired %d times, want 1", ledger.fired)
			}
			if f.session.faultIfFaulted() == nil {
				t.Fatalf("the session is not faulted after a failed GateResolved append")
			}
			resolutions := readGateResolutions(t, f.runtimeCommandFixture)
			dispositions := readDispositions(t, f.runtimeCommandFixture)
			if landed {
				if len(resolutions) != 1 || resolutions[0].Cause.CommandID != runtimeID {
					t.Fatalf("landed: %d GateResolved, want the landed answer", len(resolutions))
				}
				if len(dispositions) != 0 {
					t.Fatalf("CONTRADICTION: a disposition was written over a landed answer: %+v", dispositions)
				}
			} else if len(resolutions) != 0 || len(dispositions) != 1 || dispositions[0].Disposition != runtimecommand.DispositionRefused {
				t.Fatalf("not landed: %d GateResolved, dispositions %+v; want 0 and one refused", len(resolutions), dispositions)
			}
			// Dispatch is synchronous inside the apply, so an immediate look is exact;
			// waiting would race the gate's own policy timer, which may legitimately
			// answer the reopened gate in the not-landed case.
			select {
			case cmd := <-f.cmds:
				t.Fatalf("the failed apply dispatched %T", cmd)
			default:
			}

			// The gate is open again in memory. In the landed case the journal already
			// holds its answer, so neither a direct answer, the policy timer, nor a
			// second command may land a second GateResolved. (Not landed, the journal
			// holds none, and a later direct answer would be the first — not asserted.)
			_ = f.session.RespondGate(context.Background(), userResponse(gateID, string(gate.ApprovalDeny)))
			time.Sleep(700 * time.Millisecond) // past the 500ms policy timer
			second := admittedAs(f, "v1:crash2", "attempt/crash2", mustUUID(), userResponse(gateID, string(gate.ApprovalApprove)))
			if _, err := f.session.ApplyRuntimeCommand(context.Background(), second); err == nil {
				t.Fatalf("a faulted session applied a second command")
			}
			if landed && len(readGateResolutions(t, f.runtimeCommandFixture)) != 1 {
				t.Fatalf("a second GateResolved landed for one gate on the faulted session")
			}

			succ := takeOver(t, f.runtimeCommandFixture)
			_, closeErr := attemptCloser(t, succ).CloseAttempt(context.Background(), runtimecommand.Closure{
				CommandID: adm.CommandID, RuntimeCommandID: runtimeID, Kind: runtimecommand.KindGateResponse,
				AttemptID: adm.AttemptID, AttemptJournalEpoch: attemptEpoch,
			})
			var enduring *runtimecommand.EnduringEffectError
			if landed && !errors.As(closeErr, &enduring) {
				t.Fatalf("successor closure over a landed answer = %v, want *EnduringEffectError", closeErr)
			}
			if !landed && closeErr == nil {
				t.Fatalf("successor closed an attempt that already settled refused")
			}
			for _, d := range readDispositions(t, succ) {
				if d.AttemptID == adm.AttemptID && d.Disposition == runtimecommand.DispositionNotApplied {
					t.Fatalf("not_applied tombstone written: %+v", d)
				}
			}
		})
	}
}
