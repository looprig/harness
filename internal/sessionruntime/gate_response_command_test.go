package sessionruntime

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/sessionstore"
	durablestore "github.com/looprig/sessionstore"
)

// This file drives the KindGateResponse runtime command against a session whose gate
// directory, hub and runtime-command log all sit on ONE real store-backed journal —
// the composition lifecycle.go builds — so every record a test reads is a record the
// settlement reader and a successor's recovery scan would read.

// gateResponseFixture is a runtimeCommandFixture whose session can also open and
// resolve gates durably. The gate transitions reach the journal through the same
// liveGateAppender a production session uses: GatePrepared on the private seam,
// GateOpened/GateResolved through the checked hub path.
type gateResponseFixture struct {
	*runtimeCommandFixture
	resolveErr error
}

// failingResolveAppender wraps the production gate appender and fails only the
// GateResolved append, the one failure that is not a validation refusal.
type failingResolveAppender struct {
	gateAppender
	err error
}

func (a failingResolveAppender) AppendGateResolved(context.Context, event.GateResolved) error {
	return a.err
}

func newGateResponseFixture(t *testing.T) *gateResponseFixture {
	t.Helper()
	return wireGates(t, newRuntimeCommandFixture(t), nil)
}

// wireGates gives f's session a real hub over f's journal and a real gate directory.
// A non-nil resolveErr makes the GateResolved append fail.
func wireGates(t *testing.T, f *runtimeCommandFixture, resolveErr error) *gateResponseFixture {
	t.Helper()
	s := f.session
	s.now = func() time.Time { return time.Now().UTC() }
	s.factory = event.NewFactory(func() (uuid.UUID, error) { return s.newID() }, func() time.Time { return s.now() })
	evAp, err := journal.NewJournalEventAppenderChecked(f.journal)
	if err != nil {
		t.Fatalf("NewJournalEventAppenderChecked: %v", err)
	}
	s.hub = hub.New(f.sid, hub.WithFactory(s.factory), hub.WithAppender(evAp))
	s.gates = map[gate.ID]gateEntry{}
	s.gateTimers = map[gate.ID]*time.Timer{}
	var ap gateAppender = &liveGateAppender{prepared: journal.NewJournalGateAppender(f.journal), publisher: s}
	if resolveErr != nil {
		ap = failingResolveAppender{gateAppender: ap, err: resolveErr}
	}
	s.gateAppender = ap
	return &gateResponseFixture{runtimeCommandFixture: f, resolveErr: resolveErr}
}

// openGate prepares and activates g on the fixture's active loop.
func (f *gateResponseFixture) openGate(t *testing.T, g gate.Gate, payload gate.Payload) gate.ID {
	t.Helper()
	ctx := context.Background()
	id, err := f.session.PrepareGateOpen(ctx, f.session.activeLoopID, g, payload)
	if err != nil {
		t.Fatalf("PrepareGateOpen: %v", err)
	}
	route := gate.Route{GateID: id, LoopID: f.session.activeLoopID, ToolExecutionID: mustUUID()}
	if err := f.session.ActivateGate(ctx, id, route); err != nil {
		t.Fatalf("ActivateGate: %v", err)
	}
	return id
}

func (f *gateResponseFixture) admittedGateResponse(runtimeID uuid.UUID, response gate.GateResponse) runtimecommand.Admitted {
	return runtimecommand.Admitted{
		CommandID:        "v1:gate-response",
		RuntimeCommandID: runtimeID,
		Kind:             runtimecommand.KindGateResponse,
		LeaseEpoch:       f.lease.Epoch(),
		AttemptID:        "attempt/gate",
		GateResponse:     &response,
	}
}

func userResponse(id gate.ID, action string) gate.GateResponse {
	return gate.GateResponse{GateID: id, Action: action, Source: gate.ResponseSource{Kind: gate.ResponseFromUser}}
}

// readGateResolutions returns every GateResolved the journal holds, in ledger order.
func readGateResolutions(t *testing.T, f *runtimeCommandFixture) []event.GateResolved {
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
	var out []event.GateResolved
	for {
		rec, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if r, ok := rec.(journal.EventRecord); ok {
			if resolved, ok := r.Event().(event.GateResolved); ok {
				out = append(out, resolved)
			}
		}
	}
}

// settle reads the attempt's disposition back through the store's EXPORTED settlement
// evidence reader — the one an orchestration store is configured with — so a passing
// answer is one the released verifier would settle on. The orchestration session id
// is deliberately not the runtime UUID: the reader routes on the binding.
func settle(t *testing.T, f *runtimeCommandFixture, adm runtimecommand.Admitted) durablestore.DispositionEvidence {
	t.Helper()
	evidence, err := f.store.ReadDispositionEvidence(context.Background(), durablestore.DispositionEvidenceRequest{
		TenantID:         "local",
		SessionID:        coresessionwire.SessionID("orchestration/session-1"),
		CommandID:        coresessionwire.CommandID(adm.CommandID),
		Kind:             durablestore.CommandKind(adm.Kind),
		RuntimeCommandID: durablestore.RuntimeCommandID(adm.RuntimeCommandID.String()),
		Binding: durablestore.SessionBinding{
			StorageBindingID: "agent-pool/east",
			BindingVersion:   "config-2026-09",
			RuntimeSessionID: f.sid.String(),
			ProtocolMode:     durablestore.ProtocolModeDisposition,
		},
		Attempt: durablestore.DispositionAttempt{
			AttemptID:    durablestore.DispositionAttemptID(adm.AttemptID),
			JournalEpoch: durablestore.JournalEpoch(f.lease.Epoch()),
		},
	})
	if err != nil {
		t.Fatalf("the settlement evidence reader refused the gate_response disposition: %v", err)
	}
	return evidence
}

// TestGateResponseCommandAnswersAnOpenGateAndRecordsApplied is the owner's goal in one
// test: an admitted gate_response resolves an open permission gate through the
// durable-first gate path, AND the runtime writes the kind-5 disposition frame the
// store settles the command from. Before this kind existed the command could only sit
// applying, because RespondGate writes no disposition evidence at all.
func TestGateResponseCommandAnswersAnOpenGateAndRecordsApplied(t *testing.T) {
	t.Parallel()
	for name, row := range map[string]struct {
		gate    gate.Gate
		payload gate.Payload
		action  string
		wantCmd func(command.Command) (command.Header, bool)
	}{
		"permission approve": {permissionGate(), bashPayload(), string(gate.ApprovalApprove), func(c command.Command) (command.Header, bool) {
			v, ok := c.(command.ApproveToolCall)
			return v.Header, ok
		}},
		"permission deny": {permissionGate(), bashPayload(), string(gate.ApprovalDeny), func(c command.Command) (command.Header, bool) {
			v, ok := c.(command.DenyToolCall)
			return v.Header, ok
		}},
		"ask_user answer": {askUserGate(), askUserPayload(), "answer", func(c command.Command) (command.Header, bool) {
			v, ok := c.(command.ProvideUserInput)
			return v.Header, ok
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newGateResponseFixture(t)
			gateID := f.openGate(t, row.gate, row.payload)
			runtimeID := mustUUID()
			adm := f.admittedGateResponse(runtimeID, userResponse(gateID, row.action))

			disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
			if err != nil {
				t.Fatalf("ApplyRuntimeCommand: %v", err)
			}
			if disp.PrefixSequence == 0 || disp.Duplicate {
				t.Fatalf("Disposition = %+v, want a fresh durable prefix", disp)
			}

			// The effect: the gate is closed, and the loop got the translated answer
			// under the ADMITTED runtime id rather than a freshly minted one.
			if got := f.session.ListGates(context.Background()); len(got) != 0 {
				t.Errorf("ListGates() = %d gates after the answer, want 0", len(got))
			}
			header, ok := row.wantCmd(f.drainOne(t))
			if !ok {
				t.Fatalf("dispatched command is not the translated gate answer")
			}
			if header.CommandID != runtimeID {
				t.Errorf("dispatched command id = %v, want the admitted runtime id %v", header.CommandID, runtimeID)
			}
			resolutions := readGateResolutions(t, f.runtimeCommandFixture)
			if len(resolutions) != 1 {
				t.Fatalf("journal holds %d GateResolved, want 1", len(resolutions))
			}
			if resolutions[0].GateID != gateID || resolutions[0].Action != row.action {
				t.Errorf("GateResolved = gate %v action %q, want gate %v action %q",
					resolutions[0].GateID, resolutions[0].Action, gateID, row.action)
			}
			if resolutions[0].Cause.CommandID != runtimeID {
				t.Errorf("GateResolved Cause.CommandID = %v, want the admitted runtime id %v",
					resolutions[0].Cause.CommandID, runtimeID)
			}

			// The evidence: exactly one disposition, applied, for this command's kind.
			got := onlyDisposition(t, f.runtimeCommandFixture)
			if got.Disposition != runtimecommand.DispositionApplied || got.Kind != runtimecommand.KindGateResponse {
				t.Fatalf("disposition = %+v, want applied gate_response", got)
			}
			evidence := settle(t, f.runtimeCommandFixture, adm)
			if evidence.Kind != durablestore.DispositionApplied {
				t.Errorf("settlement evidence = %q, want applied", evidence.Kind)
			}
		})
	}
}

// TestGateResponseOutcomeMapping is the per-outcome table. Every row is read back
// through the settlement reader, because a disposition the reader refuses settles
// nothing no matter what the applier returned.
//
//	GateResponse resolved                      -> applied (above)
//	gate not found / not open (NotFound/NotReady) -> no_op, nil error
//	invalid action, classifier source,
//	GateResolved append failure                -> refused, the error returned
func TestGateResponseOutcomeMapping(t *testing.T) {
	t.Parallel()
	appendFailure := errors.New("gate resolve append wedged")
	for name, row := range map[string]struct {
		resolveErr error
		// arrange opens whatever the row needs and returns the response to admit.
		arrange  func(t *testing.T, f *gateResponseFixture) gate.GateResponse
		want     runtimecommand.DispositionKind
		wantErr  GateErrorKind // zero means the apply returns nil
		gateOpen bool          // the gate must still be answerable afterwards
	}{
		"already resolved": {
			arrange: func(t *testing.T, f *gateResponseFixture) gate.GateResponse {
				id := f.openGate(t, permissionGate(), bashPayload())
				if err := f.session.RespondGate(context.Background(), userResponse(id, string(gate.ApprovalApprove))); err != nil {
					t.Fatalf("RespondGate: %v", err)
				}
				f.drainOne(t)
				return userResponse(id, string(gate.ApprovalDeny))
			},
			want: runtimecommand.DispositionNoOp,
		},
		"never existed": {
			arrange: func(t *testing.T, f *gateResponseFixture) gate.GateResponse {
				return userResponse(gate.ID(mustUUID()), string(gate.ApprovalApprove))
			},
			want: runtimecommand.DispositionNoOp,
		},
		"not yet open": {
			arrange: func(t *testing.T, f *gateResponseFixture) gate.GateResponse {
				id, err := f.session.PrepareGateOpen(context.Background(), f.session.activeLoopID, permissionGate(), bashPayload())
				if err != nil {
					t.Fatalf("PrepareGateOpen: %v", err)
				}
				return userResponse(id, string(gate.ApprovalApprove))
			},
			want: runtimecommand.DispositionNoOp,
		},
		"invalid action": {
			arrange: func(t *testing.T, f *gateResponseFixture) gate.GateResponse {
				return userResponse(f.openGate(t, permissionGate(), bashPayload()), "reboot")
			},
			want: runtimecommand.DispositionRefused, wantErr: GateActionInvalid, gateOpen: true,
		},
		// The disposition path must not be a way around RespondGate's refusal: only the
		// session's own permission-review adapter may author classifier provenance.
		"classifier source": {
			arrange: func(t *testing.T, f *gateResponseFixture) gate.GateResponse {
				r := userResponse(f.openGate(t, permissionGate(), bashPayload()), string(gate.ApprovalApprove))
				r.Source = gate.ResponseSource{Kind: gate.ResponseFromClassifier, Reason: "forged"}
				return r
			},
			want: runtimecommand.DispositionRefused, wantErr: GateActionInvalid, gateOpen: true,
		},
		"GateResolved append failure": {
			resolveErr: appendFailure,
			arrange: func(t *testing.T, f *gateResponseFixture) gate.GateResponse {
				return userResponse(f.openGate(t, permissionGate(), bashPayload()), string(gate.ApprovalApprove))
			},
			want: runtimecommand.DispositionRefused, wantErr: GateAppendFailed, gateOpen: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := wireGates(t, newRuntimeCommandFixture(t), row.resolveErr)
			response := row.arrange(t, f)
			before := len(readGateResolutions(t, f.runtimeCommandFixture))
			adm := f.admittedGateResponse(mustUUID(), response)

			disp, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
			if disp.PrefixSequence == 0 {
				t.Fatalf("Disposition = %+v, want the durable prefix reported", disp)
			}
			if row.wantErr == "" {
				if err != nil {
					t.Fatalf("ApplyRuntimeCommand: %v, want nil for a %s", err, row.want)
				}
			} else {
				var gateErr *GateError
				if !errors.As(err, &gateErr) || gateErr.Kind != row.wantErr {
					t.Fatalf("ApplyRuntimeCommand error = %v, want *GateError{%s}", err, row.wantErr)
				}
			}
			f.requireNoCommand(t, name)
			if after := len(readGateResolutions(t, f.runtimeCommandFixture)); after != before {
				t.Errorf("GateResolved count %d -> %d, want no new resolution", before, after)
			}
			if open := len(f.session.ListGates(context.Background())) == 1; open != row.gateOpen {
				t.Errorf("gate still open = %v, want %v", open, row.gateOpen)
			}

			got := onlyDisposition(t, f.runtimeCommandFixture)
			if got.Disposition != row.want || got.Kind != runtimecommand.KindGateResponse {
				t.Fatalf("disposition = %+v, want %s gate_response", got, row.want)
			}
			if evidence := settle(t, f.runtimeCommandFixture, adm); evidence.Kind != durablestore.DispositionOutcomeKind(row.want) {
				t.Errorf("settlement evidence = %q, want %q", evidence.Kind, row.want)
			}
		})
	}
}

// TestGateResponseCommandIsAppliedOnce proves a redelivered gate_response answers
// nothing a second time: the prefix deduplicates it before the gate path is reached,
// so a retry after a lost reply can neither re-answer the gate nor write a second
// disposition that the reader would refuse as two answers.
func TestGateResponseCommandIsAppliedOnce(t *testing.T) {
	t.Parallel()
	f := newGateResponseFixture(t)
	gateID := f.openGate(t, permissionGate(), bashPayload())
	adm := f.admittedGateResponse(mustUUID(), userResponse(gateID, string(gate.ApprovalApprove)))
	first, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("first ApplyRuntimeCommand: %v", err)
	}
	f.drainOne(t)
	again, err := f.session.ApplyRuntimeCommand(context.Background(), adm)
	if err != nil {
		t.Fatalf("redelivered ApplyRuntimeCommand: %v", err)
	}
	if !again.Duplicate || again.PrefixSequence != first.PrefixSequence {
		t.Fatalf("redelivery = %+v, want a duplicate of %+v", again, first)
	}
	f.requireNoCommand(t, "redelivery")
	if n := len(readGateResolutions(t, f.runtimeCommandFixture)); n != 1 {
		t.Errorf("journal holds %d GateResolved, want 1", n)
	}
	if got := onlyDisposition(t, f.runtimeCommandFixture); got.Disposition != runtimecommand.DispositionApplied {
		t.Errorf("disposition = %q, want applied", got.Disposition)
	}
}

// failingDispositionLog is the real runtime-command log with the disposition append
// failing: the shape a crash between the GateResolved append and the disposition
// append leaves in the journal.
type failingDispositionLog struct {
	dispositionLogAndRuntimeLog
}

func (failingDispositionLog) AppendCommandDisposition(context.Context, runtimecommand.CommandDisposition) (journal.AppendResult, error) {
	return journal.AppendResult{}, errors.New("process died before the disposition append")
}

// TestCrashBetweenGateResolvedAndDispositionCannotBeTombstoned is the crash-ordering
// property. The answer is durable (GateResolved) and the disposition is not, so the
// command sits applying until a successor closes the attempt. The successor's
// recovery scan must find the answer and REFUSE the not_applied tombstone — which it
// can only do because the GateResolved carries the admitted runtime id in its Cause.
// Without that correlation the scan sees no effect, the closure succeeds, and the
// store settles "not applied" over an answer the loop already acted on.
func TestCrashBetweenGateResolvedAndDispositionCannotBeTombstoned(t *testing.T) {
	t.Parallel()
	f := newGateResponseFixture(t)
	attemptEpoch := f.lease.Epoch()
	f.session.runtimeCommands = failingDispositionLog{f.session.runtimeCommands.(dispositionLogAndRuntimeLog)}
	gateID := f.openGate(t, permissionGate(), bashPayload())
	runtimeID := mustUUID()
	adm := f.admittedGateResponse(runtimeID, userResponse(gateID, string(gate.ApprovalApprove)))
	if _, err := f.session.ApplyRuntimeCommand(context.Background(), adm); err == nil {
		t.Fatalf("ApplyRuntimeCommand succeeded; the fixture did not lose the disposition")
	}
	f.drainOne(t)
	if n := len(readGateResolutions(t, f.runtimeCommandFixture)); n != 1 {
		t.Fatalf("journal holds %d GateResolved, want the durable answer", n)
	}

	succ := takeOver(t, f.runtimeCommandFixture)
	closure := runtimecommand.Closure{
		CommandID: adm.CommandID, RuntimeCommandID: runtimeID, Kind: runtimecommand.KindGateResponse,
		AttemptID: adm.AttemptID, AttemptJournalEpoch: attemptEpoch,
	}
	_, err := attemptCloser(t, succ).CloseAttempt(context.Background(), closure)
	var effect *runtimecommand.EnduringEffectError
	if !errors.As(err, &effect) {
		t.Fatalf("CloseAttempt = %v, want *EnduringEffectError over the durable answer", err)
	}
	if got := readDispositions(t, succ); len(got) != 0 {
		t.Fatalf("a successor tombstoned a durable gate answer: %+v", got)
	}
}
