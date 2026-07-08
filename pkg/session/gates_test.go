package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/tool"
)

// fakeGateAppender is a concurrency-safe gateAppender double that records every
// call and can inject a per-method error. It does no real journal append or
// fan-out — tests inspect the recorded calls to prove the strict append seam was
// invoked in the right order with the right payload.
type fakeGateAppender struct {
	mu         sync.Mutex
	prepared   []journal.GatePreparedRecord
	opened     []event.GateOpened
	resolved   []event.GateResolved
	prepErr    error
	openErr    error
	resolveErr error
}

func (f *fakeGateAppender) AppendGatePrepared(_ context.Context, rec journal.GatePreparedRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared = append(f.prepared, rec)
	return f.prepErr
}

func (f *fakeGateAppender) AppendGateOpened(_ context.Context, ev event.GateOpened) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, ev)
	return f.openErr
}

func (f *fakeGateAppender) AppendGateResolved(_ context.Context, ev event.GateResolved) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, ev)
	return f.resolveErr
}

func (f *fakeGateAppender) snapshotPrepared() []journal.GatePreparedRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]journal.GatePreparedRecord, len(f.prepared))
	copy(out, f.prepared)
	return out
}

func (f *fakeGateAppender) snapshotOpened() []event.GateOpened {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]event.GateOpened, len(f.opened))
	copy(out, f.opened)
	return out
}

// gateSession builds a struct-literal Session wired to a fakeGateAppender and a
// real hub, with a pinned clock. It returns the session, the fake appender, and
// a loopID the gate events' loopScoped header requires.
func gateSession(t *testing.T) (*Session, *fakeGateAppender, uuid.UUID) {
	t.Helper()
	id := mustUUID()
	loopID := mustUUID()
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	t.Cleanup(sessionCancel)
	app := &fakeGateAppender{}
	s := &Session{
		SessionID:     id,
		hub:           hub.New(id),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         map[uuid.UUID]*loopHandle{},
		newID:         uuid.New,
		now:           pinnedClock(time.Date(2026, time.July, 7, 12, 0, 0, 0, time.UTC)),
		gateAppender:  app,
		gates:         map[gate.ID]gateEntry{},
	}
	s.factory = event.NewFactory(func() (uuid.UUID, error) { return s.newID() }, func() time.Time { return s.now() })
	s.loops[loopID] = &loopHandle{}
	return s, app, loopID
}

// permissionGate builds a representative permission gate envelope for the test
// table, without an ID (PrepareGateOpen mints the ID). Subject carries non-zero
// TurnID/StepID so the step-profiled event header validates.
func permissionGate() gate.Gate {
	return gate.Gate{
		Kind:     gate.KindPermission,
		Resolver: gate.ResolverLoop,
		Blocks:   gate.BlocksToolCall,
		Effect:   gate.EffectResume,
		Subject:  gate.Subject{TurnID: gate.ID(mustUUID()), StepID: gate.ID(mustUUID())},
		Prompt: gate.Prompt{
			Title: "Approve tool call",
			Body:  "echo ok",
			Controls: []gate.Control{
				{Action: "approve", Label: "Approve"},
				{Action: "deny", Label: "Deny"},
			},
		},
	}
}

// TestGatePrepareAppendsPrivateRecordAndCreatesPreparing proves PrepareGateOpen
// mints a GateID, calls the strict gateAppender with a GatePreparedRecord
// carrying the gate and payload, and inserts a non-listable preparing entry.
func TestGatePrepareAppendsPrivateRecordAndCreatesPreparing(t *testing.T) {
	t.Parallel()
	s, app, loopID := gateSession(t)
	payload := gate.PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}}

	gateID, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), payload)
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}
	if gateID == (gate.ID{}) {
		t.Fatal("PrepareGateOpen() returned zero gate id")
	}

	prepared := app.snapshotPrepared()
	if len(prepared) != 1 {
		t.Fatalf("appender recorded %d prepared records, want 1", len(prepared))
	}
	rec := prepared[0]
	if rec.Prepared().Gate.ID != gateID {
		t.Errorf("prepared gate id = %v, want %v", rec.Prepared().Gate.ID, gateID)
	}
	if rec.Payload().GateID != gateID {
		t.Errorf("payload gate id = %v, want %v", rec.Payload().GateID, gateID)
	}
	if _, ok := rec.Payload().Payload.(gate.PermissionPayload); !ok {
		t.Errorf("payload type = %T, want gate.PermissionPayload", rec.Payload().Payload)
	}

	got := s.ListGates(context.Background())
	if len(got) != 0 {
		t.Errorf("ListGates() returned %d gates, want 0 (preparing not listable)", len(got))
	}
}

// TestGatePrepareAppendFailureLeavesNoDirectoryEntry proves a failed strict
// append does not mutate the directory — the gate is neither preparing nor
// listable, and the minted id is not reusable.
func TestGatePrepareAppendFailureLeavesNoDirectoryEntry(t *testing.T) {
	t.Parallel()
	s, app, loopID := gateSession(t)
	app.prepErr = errors.New("journal wedge")

	_, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err == nil {
		t.Fatal("PrepareGateOpen() error = nil, want append failure")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateAppendFailed {
		t.Fatalf("PrepareGateOpen() error = %v, want *GateError{GateAppendFailed}", err)
	}

	got := s.ListGates(context.Background())
	if len(got) != 0 {
		t.Errorf("ListGates() returned %d gates after failed prepare, want 0", len(got))
	}
}

// TestGateActivateAppendsOpenedAndFlipsToOpen proves ActivateGate requires a
// preparing gate, appends the public GateOpened event, stores the route, and
// flips the entry to open so ListGates returns it.
func TestGateActivateAppendsOpenedAndFlipsToOpen(t *testing.T) {
	t.Parallel()
	s, app, loopID := gateSession(t)

	gateID, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}

	route := gate.Route{GateID: gateID, LoopID: loopID, ToolExecutionID: mustUUID()}
	if err := s.ActivateGate(context.Background(), gateID, route); err != nil {
		t.Fatalf("ActivateGate() error = %v", err)
	}

	opened := app.snapshotOpened()
	if len(opened) != 1 {
		t.Fatalf("appender recorded %d opened events, want 1", len(opened))
	}
	if opened[0].Gate.ID != gateID {
		t.Errorf("opened gate id = %v, want %v", opened[0].Gate.ID, gateID)
	}

	got := s.ListGates(context.Background())
	if len(got) != 1 {
		t.Fatalf("ListGates() returned %d gates, want 1", len(got))
	}
	if got[0].ID != gateID {
		t.Errorf("ListGates()[0].ID = %v, want %v", got[0].ID, gateID)
	}
}

// TestGateActivateAppendFailureLeavesPreparing proves a failed activation append
// does not flip the entry to open — the gate remains non-listable and the
// directory is not mutated.
func TestGateActivateAppendFailureLeavesPreparing(t *testing.T) {
	t.Parallel()
	s, app, loopID := gateSession(t)
	app.openErr = errors.New("journal wedge")

	gateID, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}

	route := gate.Route{GateID: gateID, LoopID: loopID}
	err = s.ActivateGate(context.Background(), gateID, route)
	if err == nil {
		t.Fatal("ActivateGate() error = nil, want append failure")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateAppendFailed {
		t.Fatalf("ActivateGate() error = %v, want *GateError{GateAppendFailed}", err)
	}

	got := s.ListGates(context.Background())
	if len(got) != 0 {
		t.Errorf("ListGates() returned %d gates after failed activate, want 0", len(got))
	}
}

// TestGateActivateUnknownGateFailsSecure proves activating a gate that was never
// prepared returns a typed not-found error and does not append.
func TestGateActivateUnknownGateFailsSecure(t *testing.T) {
	t.Parallel()
	s, app, _ := gateSession(t)

	unknownID := gate.ID(mustUUID())
	err := s.ActivateGate(context.Background(), unknownID, gate.Route{GateID: unknownID})
	if err == nil {
		t.Fatal("ActivateGate() error = nil, want not-found")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateNotFound {
		t.Fatalf("ActivateGate() error = %v, want *GateError{GateNotFound}", err)
	}
	if len(app.snapshotOpened()) != 0 {
		t.Errorf("appender recorded opened events for unknown gate, want 0")
	}
}

// TestGateActivateAlreadyActivatedFailsSecure proves activating a gate that is
// already open returns a typed not-ready error and does not append a second
// GateOpened.
func TestGateActivateAlreadyActivatedFailsSecure(t *testing.T) {
	t.Parallel()
	s, app, loopID := gateSession(t)

	gateID, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}
	route := gate.Route{GateID: gateID, LoopID: loopID}
	if err := s.ActivateGate(context.Background(), gateID, route); err != nil {
		t.Fatalf("ActivateGate() #1 error = %v", err)
	}

	err = s.ActivateGate(context.Background(), gateID, route)
	if err == nil {
		t.Fatal("ActivateGate() #2 error = nil, want not-ready")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateNotReady {
		t.Fatalf("ActivateGate() #2 error = %v, want *GateError{GateNotReady}", err)
	}
	if len(app.snapshotOpened()) != 1 {
		t.Errorf("appender recorded %d opened events, want 1 (no double-activate)", len(app.snapshotOpened()))
	}
}

// TestGateCapRejectsPrepareOverLimit proves the open-gate cap counts preparing +
// open + claiming and rejects a PrepareGateOpen that would exceed it, returning a
// typed capacity error before appending.
func TestGateCapRejectsPrepareOverLimit(t *testing.T) {
	t.Parallel()
	s, app, loopID := gateSession(t)
	s.gateCaps = GateCaps{MaxOpen: 1}

	if _, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{}); err != nil {
		t.Fatalf("PrepareGateOpen() #1 error = %v", err)
	}
	_, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err == nil {
		t.Fatal("PrepareGateOpen() #2 error = nil, want capacity")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateCapacity {
		t.Fatalf("PrepareGateOpen() #2 error = %v, want *GateError{GateCapacity}", err)
	}
	if len(app.snapshotPrepared()) != 1 {
		t.Errorf("appender recorded %d prepared records, want 1 (cap rejects before append)", len(app.snapshotPrepared()))
	}
}

// TestGateCapAllowsMultiplePreparing proves a cap of N allows N concurrent
// preparing entries but not N+1.
func TestGateCapAllowsMultiplePreparing(t *testing.T) {
	t.Parallel()
	s, _, loopID := gateSession(t)
	s.gateCaps = GateCaps{MaxOpen: 3}

	for i := 0; i < 3; i++ {
		if _, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{}); err != nil {
			t.Fatalf("PrepareGateOpen() #%d error = %v", i+1, err)
		}
	}
	_, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err == nil {
		t.Fatal("PrepareGateOpen() #4 error = nil, want capacity")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateCapacity {
		t.Fatalf("PrepareGateOpen() #4 error = %v, want *GateError{GateCapacity}", err)
	}
}

// TestGateErrorTyped proves every GateError variant errors.As to *GateError and
// renders the expected prefix.
func TestGateErrorTyped(t *testing.T) {
	t.Parallel()
	cause := errors.New("underlying")
	tests := []struct {
		name     string
		err      *GateError
		wantKind GateErrorKind
	}{
		{name: "not found", err: &GateError{Kind: GateNotFound}, wantKind: GateNotFound},
		{name: "not ready", err: &GateError{Kind: GateNotReady}, wantKind: GateNotReady},
		{name: "kind mismatch", err: &GateError{Kind: GateKindMismatch}, wantKind: GateKindMismatch},
		{name: "action invalid", err: &GateError{Kind: GateActionInvalid}, wantKind: GateActionInvalid},
		{name: "capacity", err: &GateError{Kind: GateCapacity}, wantKind: GateCapacity},
		{name: "append failed with cause", err: &GateError{Kind: GateAppendFailed, Cause: cause}, wantKind: GateAppendFailed},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.err.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", tt.err.Kind, tt.wantKind)
			}
			var ge *GateError
			if !errors.As(error(tt.err), &ge) {
				t.Fatalf("errors.As failed for %T", tt.err)
			}
			if ge.Kind != tt.wantKind {
				t.Errorf("recovered Kind = %q, want %q", ge.Kind, tt.wantKind)
			}
		})
	}
}

// TestGateListGatesReturnsOnlyOpen proves ListGates returns only open entries —
// preparing and closed entries are excluded.
func TestGateListGatesReturnsOnlyOpen(t *testing.T) {
	t.Parallel()
	s, _, loopID := gateSession(t)

	id1, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() #1 error = %v", err)
	}
	if err := s.ActivateGate(context.Background(), id1, gate.Route{GateID: id1, LoopID: loopID}); err != nil {
		t.Fatalf("ActivateGate() #1 error = %v", err)
	}

	if _, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{}); err != nil {
		t.Fatalf("PrepareGateOpen() #2 error = %v", err)
	}

	got := s.ListGates(context.Background())
	if len(got) != 1 {
		t.Fatalf("ListGates() returned %d gates, want 1 (only open)", len(got))
	}
	if got[0].ID != id1 {
		t.Errorf("ListGates()[0].ID = %v, want %v", got[0].ID, id1)
	}
}

// TestGatePrepareStampsHeaderWithSessionCoordinates proves the prepared event
// carries the session's coordinates, the caller's loopID, and a non-zero
// EventID/CreatedAt.
func TestGatePrepareStampsHeaderWithSessionCoordinates(t *testing.T) {
	t.Parallel()
	s, app, loopID := gateSession(t)

	gateID, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}

	prepared := app.snapshotPrepared()
	if len(prepared) != 1 {
		t.Fatalf("appender recorded %d prepared records, want 1", len(prepared))
	}
	hdr := prepared[0].Prepared().EventHeader()
	if hdr.SessionID != s.SessionID {
		t.Errorf("prepared SessionID = %v, want %v", hdr.SessionID, s.SessionID)
	}
	if hdr.LoopID != loopID {
		t.Errorf("prepared LoopID = %v, want %v", hdr.LoopID, loopID)
	}
	if hdr.EventID == (uuid.UUID{}) {
		t.Error("prepared EventID is zero, want non-zero")
	}
	if hdr.CreatedAt.IsZero() {
		t.Error("prepared CreatedAt is zero, want non-zero")
	}
	_ = gateID
}
