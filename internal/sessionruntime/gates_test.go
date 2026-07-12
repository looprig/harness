package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
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

type gateTransitionAppender struct {
	events       []event.Event
	err          error
	beforeReturn func()
}

func (a *gateTransitionAppender) AppendEvent(_ context.Context, ev event.Event) (uint64, error) {
	a.events = append(a.events, ev)
	if a.beforeReturn != nil {
		a.beforeReturn()
	}
	if a.err != nil {
		return 0, a.err
	}
	return uint64(len(a.events)), nil
}

// TestLiveGateAppenderCheckedFanout pins the common new/restored adapter contract:
// public gate transitions use the checked hub path, so the durable append finishes
// before live delivery and an append failure produces no live delivery. The prepared
// appender is deliberately different in the two construction cases to prove public
// GateOpened/GateResolved routing is independent of that constructor-specific seam.
func TestLiveGateAppenderCheckedFanout(t *testing.T) {
	t.Parallel()

	appendFailure := errors.New("durable gate transition failed")
	constructions := []struct {
		name     string
		prepared gateAppender
	}{
		{name: "new session", prepared: nopGateAppender{}},
		{name: "restored session", prepared: &fakeGateAppender{}},
	}
	transitions := []struct {
		name   string
		event  event.Event
		append func(*liveGateAppender, context.Context, event.Event) error
	}{
		{
			name:  "GateOpened",
			event: event.GateOpened{},
			append: func(a *liveGateAppender, ctx context.Context, ev event.Event) error {
				return a.AppendGateOpened(ctx, ev.(event.GateOpened))
			},
		},
		{
			name:  "GateResolved",
			event: event.GateResolved{},
			append: func(a *liveGateAppender, ctx context.Context, ev event.Event) error {
				return a.AppendGateResolved(ctx, ev.(event.GateResolved))
			},
		},
	}

	for _, construction := range constructions {
		construction := construction
		for _, transition := range transitions {
			transition := transition
			for _, fail := range []bool{false, true} {
				fail := fail
				name := construction.name + "/" + transition.name
				if fail {
					name += "/append failure"
				} else {
					name += "/append success"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					sessionID := mustUUID()
					appender := &gateTransitionAppender{}
					if fail {
						appender.err = appendFailure
					}
					h := hub.New(sessionID, hub.WithAppender(appender))
					sub, err := h.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = sub.Close() }()
					appender.beforeReturn = func() {
						select {
						case delivery := <-sub.Events():
							t.Errorf("live %T delivered before durable append returned", delivery.Event)
						default:
						}
					}
					adapter := &liveGateAppender{prepared: construction.prepared, publisher: h}
					err = transition.append(adapter, context.Background(), transition.event)
					if fail {
						if !errors.Is(err, appendFailure) {
							t.Fatalf("append error = %v, want durable failure", err)
						}
						select {
						case delivery := <-sub.Events():
							t.Fatalf("durable failure fanned out %T", delivery.Event)
						case <-time.After(50 * time.Millisecond):
						}
						return
					}
					if err != nil {
						t.Fatalf("append: %v", err)
					}
					select {
					case delivery := <-sub.Events():
						if reflect.TypeOf(delivery.Event) != reflect.TypeOf(transition.event) {
							t.Fatalf("live event = %T, want %T", delivery.Event, transition.event)
						}
					case <-time.After(time.Second):
						t.Fatal("durable gate transition was not fanned out")
					}
				})
			}
		}
	}
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

func (f *fakeGateAppender) snapshotResolved() []event.GateResolved {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]event.GateResolved, len(f.resolved))
	copy(out, f.resolved)
	return out
}

// gateSession builds a struct-literal Session wired to a fakeGateAppender and a
// real hub, with a pinned clock. It returns the session, the fake appender, a
// loopID, and the loop's command channel so RespondGate tests can verify
// dispatch.
func gateSession(t *testing.T) (*Session, *fakeGateAppender, uuid.UUID, chan command.Command) {
	t.Helper()
	id := mustUUID()
	loopID := mustUUID()
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	t.Cleanup(sessionCancel)
	app := &fakeGateAppender{}
	cmds := make(chan command.Command, 4)
	s := &Session{
		sessionID:     id,
		hub:           hub.New(id),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         map[uuid.UUID]*loopHandle{},
		newID:         uuid.New,
		now:           pinnedClock(time.Date(2026, time.July, 7, 12, 0, 0, 0, time.UTC)),
		gateAppender:  app,
		gates:         map[gate.ID]gateEntry{},
		gateTimers:    map[gate.ID]*time.Timer{},
	}
	s.factory = event.NewFactory(func() (uuid.UUID, error) { return s.newID() }, func() time.Time { return s.now() })
	s.loops[loopID] = &loopHandle{backend: &channelBackend{Commands: cmds, Done: make(chan struct{})}}
	return s, app, loopID, cmds
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
	s, app, loopID, _ := gateSession(t)
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
	s, app, loopID, _ := gateSession(t)
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
	s, app, loopID, _ := gateSession(t)

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
	s, app, loopID, _ := gateSession(t)
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
	s, app, _, _ := gateSession(t)

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
	s, app, loopID, _ := gateSession(t)

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

func TestGateClosePreparingRemovesWithoutPublicResolve(t *testing.T) {
	t.Parallel()
	s, app, loopID, _ := gateSession(t)
	gateID, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}

	if err := s.CloseGate(context.Background(), gateID, gate.CloseAbandoned); err != nil {
		t.Fatalf("CloseGate() error = %v", err)
	}
	if len(app.resolved) != 0 {
		t.Fatalf("resolved events = %d, want 0 for preparing close", len(app.resolved))
	}
	if got := s.ListGates(context.Background()); len(got) != 0 {
		t.Fatalf("ListGates() = %d, want 0", len(got))
	}
	if err := s.ActivateGate(context.Background(), gateID, gate.Route{GateID: gateID, LoopID: loopID}); err == nil {
		t.Fatal("ActivateGate() after CloseGate(preparing) error = nil, want not found")
	}
}

func TestGateCloseOpenAppendsOwnerClosedAndRemoves(t *testing.T) {
	t.Parallel()
	s, app, loopID, cmds := gateSession(t)
	gateID := prepareAndActivate(t, s, loopID, gate.PermissionPayload{})

	if err := s.CloseGate(context.Background(), gateID, gate.CloseOwnerClosed); err != nil {
		t.Fatalf("CloseGate() error = %v", err)
	}
	if len(app.resolved) != 1 {
		t.Fatalf("resolved events = %d, want 1", len(app.resolved))
	}
	if app.resolved[0].GateID != gateID || app.resolved[0].Reason != gate.CloseOwnerClosed {
		t.Fatalf("resolved = %+v, want owner_closed for %v", app.resolved[0], gateID)
	}
	if got := s.ListGates(context.Background()); len(got) != 0 {
		t.Fatalf("ListGates() = %d, want 0", len(got))
	}
	select {
	case c := <-cmds:
		t.Fatalf("CloseGate dispatched command %T, want none", c)
	default:
	}
}

func TestGateCloseAppendFailureLeavesOpen(t *testing.T) {
	t.Parallel()
	s, app, loopID, _ := gateSession(t)
	gateID := prepareAndActivate(t, s, loopID, gate.PermissionPayload{})
	app.resolveErr = errors.New("journal wedge")

	err := s.CloseGate(context.Background(), gateID, gate.CloseAbandoned)
	if err == nil {
		t.Fatal("CloseGate() error = nil, want append failure")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateAppendFailed {
		t.Fatalf("CloseGate() error = %v, want *GateError{GateAppendFailed}", err)
	}
	if got := s.ListGates(context.Background()); len(got) != 1 || got[0].ID != gateID {
		t.Fatalf("ListGates() = %+v, want still-open gate %v", got, gateID)
	}
}

// TestGateCapRejectsPrepareOverLimit proves the open-gate cap counts preparing +
// open + claiming and rejects a PrepareGateOpen that would exceed it, returning a
// typed capacity error before appending.
func TestGateCapRejectsPrepareOverLimit(t *testing.T) {
	t.Parallel()
	s, app, loopID, _ := gateSession(t)
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
	s, _, loopID, _ := gateSession(t)
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

func TestGatePolicyPermissionDefaultDeny(t *testing.T) {
	t.Parallel()
	s, _, loopID, _ := gateSession(t)

	gateID, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}
	entry := s.gates[gateID]
	if entry.gate.ResponsePolicy.Timeout != 5*time.Minute {
		t.Fatalf("default timeout = %v, want 5m", entry.gate.ResponsePolicy.Timeout)
	}
	if entry.gate.ResponsePolicy.OnTimeout != gate.PolicyRespond || entry.gate.ResponsePolicy.Response.Action != "deny" {
		t.Fatalf("default policy = %+v, want respond deny", entry.gate.ResponsePolicy)
	}
}

func TestGatePolicyRespondSubmitsThroughRespondGate(t *testing.T) {
	t.Parallel()
	s, app, loopID, cmds := gateSession(t)
	g := permissionGate()
	g.ResponsePolicy = gate.ResponsePolicy{
		Timeout:   10 * time.Millisecond,
		OnTimeout: gate.PolicyRespond,
		Response:  gate.ResponseTemplate{Action: "deny"},
	}
	gateID, err := s.PrepareGateOpen(context.Background(), loopID, g, gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}
	if err := s.ActivateGate(context.Background(), gateID, gate.Route{GateID: gateID, LoopID: loopID, ToolExecutionID: mustUUID()}); err != nil {
		t.Fatalf("ActivateGate() error = %v", err)
	}

	deadline := time.After(2 * time.Second)
	var resolved []event.GateResolved
	for {
		resolved = app.snapshotResolved()
		if len(resolved) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("policy timer did not append GateResolved within deadline")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if resolved[0].Source.Kind != gate.ResponseFromPolicy || resolved[0].Source.Reason != "timeout" {
		t.Fatalf("resolved source = %+v, want policy timeout", resolved[0].Source)
	}
	if _, ok := recvCommand(t, cmds).(command.DenyToolCall); !ok {
		t.Fatal("policy response did not dispatch DenyToolCall")
	}
}

func TestGatePolicyRespondLosesToEarlierHumanResponse(t *testing.T) {
	t.Parallel()
	s, app, loopID, cmds := gateSession(t)
	g := permissionGate()
	g.ResponsePolicy = gate.ResponsePolicy{
		Timeout:   50 * time.Millisecond,
		OnTimeout: gate.PolicyRespond,
		Response:  gate.ResponseTemplate{Action: "deny"},
	}
	gateID, err := s.PrepareGateOpen(context.Background(), loopID, g, gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}
	if err := s.ActivateGate(context.Background(), gateID, gate.Route{GateID: gateID, LoopID: loopID, ToolExecutionID: mustUUID()}); err != nil {
		t.Fatalf("ActivateGate() error = %v", err)
	}
	if err := s.RespondGate(context.Background(), gate.GateResponse{GateID: gateID, Action: "deny", Source: gate.ResponseSource{Kind: gate.ResponseFromUser}}); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}
	recvCommand(t, cmds)
	time.Sleep(100 * time.Millisecond)
	resolved := app.snapshotResolved()
	if len(resolved) != 1 {
		t.Fatalf("resolved events = %d, want exactly 1", len(resolved))
	}
	if resolved[0].Source.Kind != gate.ResponseFromUser {
		t.Fatalf("resolved source = %+v, want user", resolved[0].Source)
	}
}

func TestGatePolicyWaitWithNoTimeoutLeavesGateOpen(t *testing.T) {
	t.Parallel()
	s, _, loopID, _ := gateSession(t)
	g := permissionGate()
	g.ResponsePolicy = gate.ResponsePolicy{OnTimeout: gate.PolicyWait}
	gateID, err := s.PrepareGateOpen(context.Background(), loopID, g, gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}
	if err := s.ActivateGate(context.Background(), gateID, gate.Route{GateID: gateID, LoopID: loopID}); err != nil {
		t.Fatalf("ActivateGate() error = %v", err)
	}
	if len(s.gateTimers) != 0 {
		t.Fatalf("gate timers = %d, want 0 for wait policy", len(s.gateTimers))
	}
	if got := s.ListGates(context.Background()); len(got) != 1 || got[0].ID != gateID {
		t.Fatalf("ListGates() = %+v, want open gate", got)
	}
}

func TestGatePolicyRejectsUnsupportedActions(t *testing.T) {
	t.Parallel()
	tests := []gate.PolicyAction{gate.PolicyModelDecide, gate.PolicySuspendSession}
	for _, action := range tests {
		action := action
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()
			s, _, loopID, _ := gateSession(t)
			g := permissionGate()
			g.ResponsePolicy = gate.ResponsePolicy{Timeout: time.Second, OnTimeout: action}
			_, err := s.PrepareGateOpen(context.Background(), loopID, g, gate.PermissionPayload{})
			if err == nil {
				t.Fatal("PrepareGateOpen() error = nil, want unsupported policy rejection")
			}
			var ge *GateError
			if !errors.As(err, &ge) || ge.Kind != GateActionInvalid {
				t.Fatalf("PrepareGateOpen() error = %v, want *GateError{GateActionInvalid}", err)
			}
		})
	}
}

func TestGatePolicyTimeoutCapRejectsBeforeAppend(t *testing.T) {
	t.Parallel()
	s, app, loopID, _ := gateSession(t)
	s.gateCaps = GateCaps{MaxTimeout: time.Second}
	g := permissionGate()
	g.ResponsePolicy = gate.ResponsePolicy{
		Timeout:   2 * time.Second,
		OnTimeout: gate.PolicyRespond,
		Response:  gate.ResponseTemplate{Action: "deny"},
	}

	_, err := s.PrepareGateOpen(context.Background(), loopID, g, gate.PermissionPayload{})
	if err == nil {
		t.Fatal("PrepareGateOpen() error = nil, want timeout cap rejection")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateCapacity {
		t.Fatalf("PrepareGateOpen() error = %v, want *GateError{GateCapacity}", err)
	}
	if len(app.snapshotPrepared()) != 0 {
		t.Fatalf("prepared records = %d, want 0", len(app.snapshotPrepared()))
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
	s, _, loopID, _ := gateSession(t)

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
	s, app, loopID, _ := gateSession(t)

	gateID, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}

	prepared := app.snapshotPrepared()
	if len(prepared) != 1 {
		t.Fatalf("appender recorded %d prepared records, want 1", len(prepared))
	}
	hdr := prepared[0].Prepared().EventHeader()
	if hdr.SessionID != s.SessionID() {
		t.Errorf("prepared SessionID = %v, want %v", hdr.SessionID, s.SessionID())
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

// --- RespondGate tests ---------------------------------------------------

// prepareAndActivate prepares and activates a permission gate, returning the
// open gate id. It is the shared setup for RespondGate tests.
func prepareAndActivate(t *testing.T, s *Session, loopID uuid.UUID, payload gate.Payload) gate.ID {
	t.Helper()
	gateID, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), payload)
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}
	if err := s.ActivateGate(context.Background(), gateID, gate.Route{GateID: gateID, LoopID: loopID, ToolExecutionID: mustUUID()}); err != nil {
		t.Fatalf("ActivateGate() error = %v", err)
	}
	return gateID
}

// askUserGate builds a representative ask-user gate envelope (action "answer"),
// the ask-user counterpart to permissionGate. Subject carries non-zero
// TurnID/StepID so the step-profiled event header validates.
func askUserGate() gate.Gate {
	return gate.Gate{
		Kind:     gate.KindAskUser,
		Resolver: gate.ResolverLoop,
		Blocks:   gate.BlocksToolCall,
		Effect:   gate.EffectResume,
		Subject:  gate.Subject{TurnID: gate.ID(mustUUID()), StepID: gate.ID(mustUUID())},
		Prompt: gate.Prompt{
			Title:    "User input requested",
			Body:     "question",
			Controls: []gate.Control{{Action: "answer", Label: "Answer"}},
		},
	}
}

// bashPayload is a permission payload carrying a Bash request that offers the
// persistable scopes, the representative payload for approve/deny RespondGate
// tests (a permission approve requires a non-nil Request).
func bashPayload() gate.Payload {
	return gate.PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}}
}

// askUserPayload is the payload for an ask-user gate.
func askUserPayload() gate.Payload {
	return gate.AskUserPayload{Question: "question"}
}

// activateOn prepares and activates gate g with payload on loopID, pinning the
// route's ToolExecutionID to callID, and returns the open gate id. It is the
// known-ToolExecutionID counterpart to prepareAndActivate.
func activateOn(t *testing.T, s *Session, loopID, callID uuid.UUID, g gate.Gate, payload gate.Payload) gate.ID {
	t.Helper()
	gateID, err := s.PrepareGateOpen(context.Background(), loopID, g, payload)
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}
	if err := s.ActivateGate(context.Background(), gateID, gate.Route{GateID: gateID, LoopID: loopID, ToolExecutionID: callID}); err != nil {
		t.Fatalf("ActivateGate() error = %v", err)
	}
	return gateID
}

// mustJSON marshals v to a json.RawMessage, panicking on failure (test-only).
func mustJSON(v string) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// userApprove/userDeny/userAnswer build the human GateResponse for each gate
// kind, mirroring what the removed Approve/Deny/ProvideUserInput trio used to
// send: a user-sourced approve at the given scope, a user-sourced deny, and a
// user-sourced ask-user answer.
func userApprove(gateID gate.ID, scope string) gate.GateResponse {
	return gate.GateResponse{
		GateID: gateID,
		Action: "approve",
		Values: map[string]json.RawMessage{"scope": mustJSON(scope)},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}
}

func userDeny(gateID gate.ID) gate.GateResponse {
	return gate.GateResponse{
		GateID: gateID,
		Action: "deny",
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}
}

func userAnswer(gateID gate.ID, answer string) gate.GateResponse {
	return gate.GateResponse{
		GateID: gateID,
		Action: "answer",
		Values: map[string]json.RawMessage{"answer": mustJSON(answer)},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}
}

// gateSessionTwoLoops builds a struct-literal Session wired to a fakeGateAppender
// and TWO fake loops (A and B), each keyed by its own loop id with a buffered
// command channel, so a RespondGate sibling-isolation test can observe exactly
// which loop a gate reply was dispatched to. A is registered first; neither is
// the "primary" (RespondGate routes by the gate's stored route, not primary).
func gateSessionTwoLoops(t *testing.T) (s *Session, loopA, loopB uuid.UUID, cmdsA, cmdsB chan command.Command) {
	t.Helper()
	id := mustUUID()
	loopA = mustUUID()
	loopB = mustUUID()
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	t.Cleanup(sessionCancel)
	cmdsA = make(chan command.Command, 4)
	cmdsB = make(chan command.Command, 4)
	s = &Session{
		sessionID:     id,
		hub:           hub.New(id),
		sessionCtx:    sessionCtx,
		sessionCancel: sessionCancel,
		loops:         map[uuid.UUID]*loopHandle{},
		newID:         uuid.New,
		now:           pinnedClock(time.Date(2026, time.July, 7, 12, 0, 0, 0, time.UTC)),
		gateAppender:  &fakeGateAppender{},
		gates:         map[gate.ID]gateEntry{},
		gateTimers:    map[gate.ID]*time.Timer{},
	}
	s.factory = event.NewFactory(func() (uuid.UUID, error) { return s.newID() }, func() time.Time { return s.now() })
	s.loops[loopA] = &loopHandle{backend: &channelBackend{Commands: cmdsA, Done: make(chan struct{})}}
	s.loops[loopB] = &loopHandle{backend: &channelBackend{Commands: cmdsB, Done: make(chan struct{})}}
	return s, loopA, loopB, cmdsA, cmdsB
}

// TestRespondGateApproveDispatchesCommand proves a human approve response claims
// the open gate, appends GateResolved, and dispatches an ApproveToolCall command
// with the extracted scope and accepted_grants.
func TestRespondGateApproveDispatchesCommand(t *testing.T) {
	t.Parallel()
	s, app, loopID, cmds := gateSession(t)
	payload := gate.PermissionPayload{Request: tool.BashRequest{
		Command: "echo ok",
		Grants:  []tool.GrantDisplay{{Token: "t1", Description: "network egress"}, {Token: "t2", Description: "write to /out"}},
	}}
	gateID := prepareAndActivate(t, s, loopID, payload)

	resp := gate.GateResponse{
		GateID: gateID,
		Action: "approve",
		Values: map[string]json.RawMessage{
			"scope":           json.RawMessage(`"session"`),
			"accepted_grants": json.RawMessage(`["t1","t2"]`),
		},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}
	if err := s.RespondGate(context.Background(), resp); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}

	cmd := recvCommand(t, cmds)
	approve, ok := cmd.(command.ApproveToolCall)
	if !ok {
		t.Fatalf("dispatched command = %T, want ApproveToolCall", cmd)
	}
	if approve.Scope != tool.ScopeSession {
		t.Errorf("Scope = %d, want %d (ScopeSession)", approve.Scope, tool.ScopeSession)
	}
	if len(approve.AcceptedGrants) != 2 || approve.AcceptedGrants[0] != "t1" || approve.AcceptedGrants[1] != "t2" {
		t.Errorf("AcceptedGrants = %v, want [t1 t2]", approve.AcceptedGrants)
	}

	if len(app.resolved) != 1 {
		t.Errorf("appender recorded %d resolved events, want 1", len(app.resolved))
	}
	if app.resolved[0].Reason != gate.CloseAnswered {
		t.Errorf("resolved reason = %q, want %q", app.resolved[0].Reason, gate.CloseAnswered)
	}
	if app.resolved[0].ApprovalScope != tool.ScopeSession {
		t.Errorf("resolved approval scope = %d, want %d (ScopeSession)", app.resolved[0].ApprovalScope, tool.ScopeSession)
	}
}

func TestRespondGateApproveRequiresScope(t *testing.T) {
	t.Parallel()
	s, _, loopID, cmds := gateSession(t)
	payload := gate.PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}}
	gateID := prepareAndActivate(t, s, loopID, payload)

	err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: gateID,
		Action: "approve",
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
	if err == nil {
		t.Fatal("RespondGate() error = nil, want missing scope validation failure")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateActionInvalid {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateActionInvalid}", err)
	}

	select {
	case c := <-cmds:
		t.Errorf("missing scope dispatched a command %T, want none", c)
	default:
	}
}

// TestRespondGateDenyDispatchesCommand proves a human deny response dispatches a
// DenyToolCall command.
func TestRespondGateDenyDispatchesCommand(t *testing.T) {
	t.Parallel()
	s, _, loopID, cmds := gateSession(t)
	gateID := prepareAndActivate(t, s, loopID, gate.PermissionPayload{})

	if err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: gateID,
		Action: "deny",
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	}); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}

	cmd := recvCommand(t, cmds)
	if _, ok := cmd.(command.DenyToolCall); !ok {
		t.Fatalf("dispatched command = %T, want DenyToolCall", cmd)
	}
}

// TestRespondGateDuplicateIsStale proves a second response to an already-closed
// gate returns a typed not-found error and does not dispatch a second command.
func TestRespondGateDuplicateIsStale(t *testing.T) {
	t.Parallel()
	s, _, loopID, cmds := gateSession(t)
	gateID := prepareAndActivate(t, s, loopID, gate.PermissionPayload{})

	if err := s.RespondGate(context.Background(), gate.GateResponse{GateID: gateID, Action: "deny"}); err != nil {
		t.Fatalf("RespondGate() #1 error = %v", err)
	}
	recvCommand(t, cmds)

	err := s.RespondGate(context.Background(), gate.GateResponse{GateID: gateID, Action: "deny"})
	if err == nil {
		t.Fatal("RespondGate() #2 error = nil, want stale")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateNotFound {
		t.Fatalf("RespondGate() #2 error = %v, want *GateError{GateNotFound}", err)
	}

	select {
	case c := <-cmds:
		t.Errorf("second response dispatched a command %T, want none", c)
	default:
	}
}

// TestRespondGateAppendFailureLeavesGateAnswerable proves a failed GateResolved
// append reverts the claiming state back to open — the gate remains answerable
// and no command is dispatched.
func TestRespondGateAppendFailureLeavesGateAnswerable(t *testing.T) {
	t.Parallel()
	s, app, loopID, cmds := gateSession(t)
	app.resolveErr = errors.New("journal wedge")
	gateID := prepareAndActivate(t, s, loopID, gate.PermissionPayload{})

	err := s.RespondGate(context.Background(), gate.GateResponse{GateID: gateID, Action: "deny"})
	if err == nil {
		t.Fatal("RespondGate() error = nil, want append failure")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateAppendFailed {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateAppendFailed}", err)
	}

	select {
	case c := <-cmds:
		t.Errorf("failed append dispatched a command %T, want none", c)
	default:
	}

	got := s.ListGates(context.Background())
	if len(got) != 1 {
		t.Fatalf("ListGates() returned %d gates after failed append, want 1 (still answerable)", len(got))
	}
	if got[0].ID != gateID {
		t.Errorf("ListGates()[0].ID = %v, want %v", got[0].ID, gateID)
	}
}

// TestRespondGateValidatesAcceptedGrants proves a permission approve with
// accepted_grants not in the request's GrantDisplay.Token values is rejected.
func TestRespondGateValidatesAcceptedGrants(t *testing.T) {
	t.Parallel()
	s, _, loopID, cmds := gateSession(t)
	payload := gate.PermissionPayload{Request: tool.BashRequest{
		Command: "echo ok",
		Grants:  []tool.GrantDisplay{{Token: "real-token", Description: "network egress"}},
	}}
	gateID := prepareAndActivate(t, s, loopID, payload)

	err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: gateID,
		Action: "approve",
		Values: map[string]json.RawMessage{
			"scope":           json.RawMessage(`"once"`),
			"accepted_grants": json.RawMessage(`["fabricated-token"]`),
		},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
	if err == nil {
		t.Fatal("RespondGate() error = nil, want grant validation failure")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateActionInvalid {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateActionInvalid}", err)
	}

	select {
	case c := <-cmds:
		t.Errorf("invalid grants dispatched a command %T, want none", c)
	default:
	}
}

// TestRespondGateRejectsScopeOutsidePermissionRequest proves approve fails
// secure when the response asks for a scope the PermissionRequest did not offer.
func TestRespondGateRejectsScopeOutsidePermissionRequest(t *testing.T) {
	t.Parallel()
	s, app, loopID, cmds := gateSession(t)
	payload := gate.PermissionPayload{Request: tool.UnknownRequest{
		Tool:    "Mystery",
		Summary: "redacted call",
	}}
	gateID := prepareAndActivate(t, s, loopID, payload)

	err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: gateID,
		Action: "approve",
		Values: map[string]json.RawMessage{
			"scope": json.RawMessage(`"session"`),
		},
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
	})
	if err == nil {
		t.Fatal("RespondGate() error = nil, want scope validation failure")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateActionInvalid {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateActionInvalid}", err)
	}
	if len(app.resolved) != 0 {
		t.Errorf("appender recorded %d resolved events, want 0", len(app.resolved))
	}

	select {
	case c := <-cmds:
		t.Errorf("invalid scope dispatched a command %T, want none", c)
	default:
	}
}

func TestBuildGateResolvedUsesValidatedApprovalScope(t *testing.T) {
	t.Parallel()
	s, _, loopID, _ := gateSession(t)
	entry := gateEntry{
		coordinates: identity.Coordinates{
			SessionID: s.SessionID(),
			LoopID:    loopID,
			TurnID:    mustUUID(),
			StepID:    mustUUID(),
		},
	}
	gateID := gate.ID(mustUUID())
	validatedScope := tool.ScopeSession

	resolved, err := s.buildGateResolved(entry, gate.GateResponse{
		GateID: gateID,
		Action: "approve",
		Values: map[string]json.RawMessage{
			"scope": json.RawMessage(`"invalid raw scope"`),
		},
	}, gate.PermissionAudit{}, &validatedScope)
	if err != nil {
		t.Fatalf("buildGateResolved() error = %v", err)
	}
	if resolved.ApprovalScope != tool.ScopeSession {
		t.Fatalf("ApprovalScope = %d, want %d (ScopeSession)", resolved.ApprovalScope, tool.ScopeSession)
	}
}

// TestRespondGateReturnsNilAfterDurableAppendWhenDispatchFails proves a
// post-append route lookup failure does not make a durably accepted response look
// rejected to the caller.
func TestRespondGateReturnsNilAfterDurableAppendWhenDispatchFails(t *testing.T) {
	t.Parallel()
	s, app, loopID, cmds := gateSession(t)
	gateID := prepareAndActivate(t, s, loopID, gate.PermissionPayload{})
	delete(s.loops, loopID)

	if err := s.RespondGate(context.Background(), gate.GateResponse{GateID: gateID, Action: "deny"}); err != nil {
		t.Fatalf("RespondGate() error = %v, want nil after durable append", err)
	}
	if len(app.resolved) != 1 {
		t.Fatalf("appender recorded %d resolved events, want 1", len(app.resolved))
	}
	if got := s.ListGates(context.Background()); len(got) != 0 {
		t.Fatalf("ListGates() returned %d gates after durable append, want 0", len(got))
	}

	select {
	case c := <-cmds:
		t.Errorf("dispatch failure sent command %T, want none", c)
	default:
	}
}

// TestRespondGateUnknownGateFailsSecure proves responding to an unknown gate
// returns a typed not-found error and does not append or dispatch.
func TestRespondGateUnknownGateFailsSecure(t *testing.T) {
	t.Parallel()
	s, app, _, _ := gateSession(t)

	err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: gate.ID(mustUUID()),
		Action: "deny",
	})
	if err == nil {
		t.Fatal("RespondGate() error = nil, want not-found")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateNotFound {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateNotFound}", err)
	}
	if len(app.resolved) != 0 {
		t.Errorf("appender recorded %d resolved events, want 0", len(app.resolved))
	}
}

// TestRespondGatePreparingGateFailsSecure proves responding to a gate that is
// still preparing (not yet activated) returns a typed not-ready error.
func TestRespondGatePreparingGateFailsSecure(t *testing.T) {
	t.Parallel()
	s, _, loopID, _ := gateSession(t)
	gateID, err := s.PrepareGateOpen(context.Background(), loopID, permissionGate(), gate.PermissionPayload{})
	if err != nil {
		t.Fatalf("PrepareGateOpen() error = %v", err)
	}

	err = s.RespondGate(context.Background(), gate.GateResponse{GateID: gateID, Action: "deny"})
	if err == nil {
		t.Fatal("RespondGate() error = nil, want not-ready")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateNotReady {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateNotReady}", err)
	}
}

// TestRespondGateInvalidActionFailsSecure proves an action not in the gate's
// prompt controls is rejected with a typed action-invalid error.
func TestRespondGateInvalidActionFailsSecure(t *testing.T) {
	t.Parallel()
	s, _, loopID, cmds := gateSession(t)
	gateID := prepareAndActivate(t, s, loopID, gate.PermissionPayload{})

	err := s.RespondGate(context.Background(), gate.GateResponse{
		GateID: gateID,
		Action: "bogus",
	})
	if err == nil {
		t.Fatal("RespondGate() error = nil, want action-invalid")
	}
	var ge *GateError
	if !errors.As(err, &ge) || ge.Kind != GateActionInvalid {
		t.Fatalf("RespondGate() error = %v, want *GateError{GateActionInvalid}", err)
	}

	select {
	case c := <-cmds:
		t.Errorf("invalid action dispatched a command %T, want none", c)
	default:
	}
}
