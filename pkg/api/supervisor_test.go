package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/transcript"
	"github.com/looprig/harness/pkg/uuid"
)

// pollDeadline / pollInterval bound the busy-wait used to observe the
// supervisor's goroutine effects. The assertion is the poll condition itself
// (recorded / dropped), never a fixed Sleep — the interval only paces the loop.
const (
	pollDeadline = 2 * time.Second
	pollInterval = 2 * time.Millisecond
)

// pollUntil spins cond on a bounded deadline, returning true as soon as cond
// holds (or a final check at expiry). It never asserts by sleeping a fixed
// duration — it waits only as long as the condition takes.
func pollUntil(t *testing.T, cond func() bool) bool {
	t.Helper()
	stop := time.Now().Add(pollDeadline)
	for time.Now().Before(stop) {
		if cond() {
			return true
		}
		time.Sleep(pollInterval)
	}
	return cond()
}

// mkID builds a distinct non-zero UUID from a single seed byte so table rows
// never collide (and a zero UUID never masquerades as a real id).
func mkID(b byte) uuid.UUID {
	var u uuid.UUID
	u[0] = b
	return u
}

// loopHeader stamps an event Header with just the producing loop id — the only
// coordinate the supervisor reads (ev.EventHeader().LoopID).
func loopHeader(loopID uuid.UUID) event.Header {
	return event.Header{Coordinates: identity.Coordinates{LoopID: loopID}}
}

// errSubscribe is a leaf sentinel for the Subscribe-failure path; errStreamLost
// stands in for a hub-forced subscription loss cause.
var (
	errSubscribe  = errors.New("api_test: subscribe boom")
	errStreamLost = errors.New("api_test: stream lost")
)

// fakeSub is a hand-fed event.Subscription: the test pushes events with feed and
// tears the stream down with Close (guarded against double-close by sync.Once).
// fail simulates a hub-forced loss — it sets the Err() cause and closes the
// channel WITHOUT an intentional stop(); Close reuses the same once so a later
// stop() is a safe no-op rather than a double-close panic.
type fakeSub struct {
	ch   chan event.Event
	once sync.Once

	mu  sync.Mutex
	err error
}

func newFakeSub() *fakeSub { return &fakeSub{ch: make(chan event.Event, 16)} }

func (s *fakeSub) Events() <-chan event.Event { return s.ch }

func (s *fakeSub) Close() error {
	s.once.Do(func() { close(s.ch) })
	return nil
}

func (s *fakeSub) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *fakeSub) feed(ev event.Event) { s.ch <- ev }

func (s *fakeSub) fail(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.once.Do(func() { close(s.ch) })
}

// fakeAgent implements the full api.Agent interface. Subscribe carries behavior
// (hands back the injected sub or a forced error) for the supervisor; Interrupt
// and Close carry behavior for the session-lifecycle endpoints; Submit/Approve/
// Deny/ProvideAnswer carry behavior for the input + gate-routing endpoints (each
// returns its injected result/err and records the arguments it was handed so a
// test can assert the LoopID came from the registry, not the client). The
// remaining methods are inert stubs.
type fakeAgent struct {
	sub    *fakeSub
	subErr error

	gotFilter event.EventFilter

	// interruptResult/interruptErr are what Interrupt returns — the interrupt
	// endpoint surfaces the bool and maps a non-nil error to 500.
	interruptResult bool
	interruptErr    error

	// submitID/submitErr are what Submit returns: the input endpoint echoes the id
	// back as input_id and maps a non-nil error to 500. approveErr/denyErr/answerErr
	// force the gate-dispatch 500 paths. These are set at construction (before the
	// server handles a request), so they are read without the mutex.
	submitID   uuid.UUID
	submitErr  error
	approveErr error
	denyErr    error
	answerErr  error

	// exportSrc/exportPrompts/exportErr are what ExportSource returns for the export
	// endpoint: the source+resolver Reconstruct+Render fold into an HTML transcript,
	// or exportErr forces the 409 (ExportUnavailableError) / 500 (any other) paths.
	// Set at construction, read without the mutex.
	exportSrc     transcript.RecordSource
	exportPrompts transcript.SystemPromptResolver
	exportErr     error

	// mu guards the recorded-call fields below (closed + the Submit/Approve/Deny/
	// ProvideAnswer arguments). The endpoints run in the server's handler goroutine
	// while the test asserts from its own goroutine, so every recorded write must be
	// synchronized to stay clean under -race.
	mu     sync.Mutex
	closed bool

	submittedBlocks []content.Block

	approveCalled bool
	approveLoopID uuid.UUID
	approveCallID uuid.UUID
	approveScope  tool.ApprovalScope

	denyCalled bool
	denyLoopID uuid.UUID
	denyCallID uuid.UUID

	answerCalled bool
	answerLoopID uuid.UUID
	answerCallID uuid.UUID
	answerText   string
}

func (a *fakeAgent) Subscribe(filter event.EventFilter) (event.Subscription, error) {
	a.gotFilter = filter
	if a.subErr != nil {
		return nil, a.subErr
	}
	return a.sub, nil
}

func (a *fakeAgent) Submit(_ context.Context, blocks []content.Block) (uuid.UUID, error) {
	a.mu.Lock()
	a.submittedBlocks = blocks
	a.mu.Unlock()
	return a.submitID, a.submitErr
}
func (a *fakeAgent) PrimaryLoopID() uuid.UUID { return uuid.UUID{} }
func (a *fakeAgent) Approve(_ context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error {
	a.mu.Lock()
	a.approveCalled, a.approveLoopID, a.approveCallID, a.approveScope = true, loopID, callID, scope
	a.mu.Unlock()
	return a.approveErr
}
func (a *fakeAgent) Deny(_ context.Context, loopID, callID uuid.UUID) error {
	a.mu.Lock()
	a.denyCalled, a.denyLoopID, a.denyCallID = true, loopID, callID
	a.mu.Unlock()
	return a.denyErr
}
func (a *fakeAgent) ProvideAnswer(_ context.Context, loopID, callID uuid.UUID, answer string) error {
	a.mu.Lock()
	a.answerCalled, a.answerLoopID, a.answerCallID, a.answerText = true, loopID, callID, answer
	a.mu.Unlock()
	return a.answerErr
}
func (a *fakeAgent) Interrupt(_ context.Context) (bool, error) {
	return a.interruptResult, a.interruptErr
}
func (a *fakeAgent) Close(_ context.Context) error {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	return nil
}
func (a *fakeAgent) ExportSource(_ context.Context) (transcript.RecordSource, transcript.SystemPromptResolver, error) {
	return a.exportSrc, a.exportPrompts, a.exportErr
}

// wasClosed reports whether Close has been called (mutex-guarded so the delete
// handler's off-lock Close synchronizes cleanly with the test assertion under
// -race).
func (a *fakeAgent) wasClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

// submittedBlocksSnapshot returns the blocks the last Submit was handed (mutex-
// guarded so the input handler's write synchronizes with the test read).
func (a *fakeAgent) submittedBlocksSnapshot() []content.Block {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.submittedBlocks
}

// approveArgs / denyArgs / answerArgs report whether the corresponding gate
// method was dispatched and the arguments it received, so a test can assert the
// LoopID was pulled from the registry (not the client body) and routed with the
// tool-execution id + scope/answer. All are mutex-guarded for -race.
func (a *fakeAgent) approveArgs() (called bool, loopID, callID uuid.UUID, scope tool.ApprovalScope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.approveCalled, a.approveLoopID, a.approveCallID, a.approveScope
}

func (a *fakeAgent) denyArgs() (called bool, loopID, callID uuid.UUID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.denyCalled, a.denyLoopID, a.denyCallID
}

func (a *fakeAgent) answerArgs() (called bool, loopID, callID uuid.UUID, answer string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.answerCalled, a.answerLoopID, a.answerCallID, a.answerText
}

// TestSupervisor_RecordsAndDropsGate proves the registry records a gate from a
// channel-fed request event (with NO SSE client attached) and drops it on the
// resolving/terminal event — covering both the tool-lifecycle drop path
// (ToolCallStarted/Completed by ToolExecutionID) and the turn-terminal backstop
// (TurnDone/TurnFailed/TurnInterrupted by LoopID).
func TestSupervisor_RecordsAndDropsGate(t *testing.T) {
	tests := []struct {
		name       string
		teid       uuid.UUID
		lid        uuid.UUID
		request    func(teid, lid uuid.UUID) event.Event
		resolve    func(teid, lid uuid.UUID) event.Event
		wantKind   string
		wantPrompt string
	}{
		{
			name: "user-input recorded, dropped by tool-completed",
			teid: mkID(0x11),
			lid:  mkID(0xA1),
			request: func(teid, lid uuid.UUID) event.Event {
				return event.UserInputRequested{Header: loopHeader(lid), ToolExecutionID: teid, Question: "pick one"}
			},
			resolve: func(teid, lid uuid.UUID) event.Event {
				return event.ToolCallCompleted{Header: loopHeader(lid), ToolExecutionID: teid}
			},
			wantKind:   kindUserInput,
			wantPrompt: "pick one",
		},
		{
			name: "permission recorded, dropped by tool-started",
			teid: mkID(0x22),
			lid:  mkID(0xA2),
			request: func(teid, lid uuid.UUID) event.Event {
				return event.PermissionRequested{Header: loopHeader(lid), ToolExecutionID: teid, Request: tool.BashRequest{Command: "rm -rf /tmp/x"}}
			},
			resolve: func(teid, lid uuid.UUID) event.Event {
				return event.ToolCallStarted{Header: loopHeader(lid), ToolExecutionID: teid}
			},
			wantKind:   kindPermission,
			wantPrompt: "rm -rf /tmp/x",
		},
		{
			name: "permission recorded, dropped by TurnDone backstop",
			teid: mkID(0x33),
			lid:  mkID(0xA3),
			request: func(teid, lid uuid.UUID) event.Event {
				return event.PermissionRequested{Header: loopHeader(lid), ToolExecutionID: teid, Request: tool.FetchRequest{Method: "GET", URL: "https://example.com"}}
			},
			resolve: func(_, lid uuid.UUID) event.Event {
				return event.TurnDone{Header: loopHeader(lid)}
			},
			wantKind:   kindPermission,
			wantPrompt: "GET https://example.com",
		},
		{
			name: "user-input recorded, dropped by TurnInterrupted backstop",
			teid: mkID(0x44),
			lid:  mkID(0xA4),
			request: func(teid, lid uuid.UUID) event.Event {
				return event.UserInputRequested{Header: loopHeader(lid), ToolExecutionID: teid, Question: "continue?"}
			},
			resolve: func(_, lid uuid.UUID) event.Event {
				return event.TurnInterrupted{Header: loopHeader(lid)}
			},
			wantKind:   kindUserInput,
			wantPrompt: "continue?",
		},
		{
			name: "permission with nil Request records empty prompt, dropped by TurnFailed",
			teid: mkID(0x55),
			lid:  mkID(0xA5),
			request: func(teid, lid uuid.UUID) event.Event {
				return event.PermissionRequested{Header: loopHeader(lid), ToolExecutionID: teid, Request: nil}
			},
			resolve: func(_, lid uuid.UUID) event.Event {
				return event.TurnFailed{Header: loopHeader(lid)}
			},
			wantKind:   kindPermission,
			wantPrompt: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fs := newFakeSub()
			fa := &fakeAgent{sub: fs}

			sup, err := newSupervisor(fa)
			if err != nil {
				t.Fatalf("newSupervisor() unexpected error = %v", err)
			}
			if !fa.gotFilter.Ephemeral.All || !fa.gotFilter.Enduring.All {
				t.Fatalf("supervisor subscribed with %+v, want whole-session (Ephemeral.All && Enduring.All)", fa.gotFilter)
			}

			wantGate := pendingGate{LoopID: tt.lid, Kind: tt.wantKind, Prompt: tt.wantPrompt}

			// Record: feed the request, poll until the registry holds it. No SSE
			// client is attached — the supervisor owns its own subscription.
			fs.feed(tt.request(tt.teid, tt.lid))
			if !pollUntil(t, func() bool { _, ok := sup.lookup(tt.teid); return ok }) {
				t.Fatalf("gate %v not recorded within %v", tt.teid, pollDeadline)
			}
			got, ok := sup.lookup(tt.teid)
			if !ok {
				t.Fatalf("lookup(%v) ok = false after record", tt.teid)
			}
			if got != wantGate {
				t.Errorf("lookup(%v) = %+v, want %+v", tt.teid, got, wantGate)
			}

			// list() snapshot must surface the open gate for reconnect.
			want := openGate{ToolExecutionID: tt.teid, LoopID: wantGate.LoopID, Kind: wantGate.Kind, Prompt: wantGate.Prompt}
			if !containsGate(sup.list(), want) {
				t.Errorf("list() = %+v, want it to contain %+v", sup.list(), want)
			}

			// Drop: feed the resolving/terminal event, poll until gone.
			fs.feed(tt.resolve(tt.teid, tt.lid))
			if !pollUntil(t, func() bool { _, ok := sup.lookup(tt.teid); return !ok }) {
				t.Fatalf("gate %v not dropped within %v", tt.teid, pollDeadline)
			}
			if g := sup.list(); containsGate(g, want) {
				t.Errorf("list() still contains dropped gate: %+v", g)
			}

			// stop() joins the run goroutine and tolerates a double call.
			if err := sup.stop(); err != nil {
				t.Errorf("stop() error = %v", err)
			}
			if err := sup.stop(); err != nil {
				t.Errorf("second stop() error = %v", err)
			}
			// stop() joins run(), so done is already closed on return.
			select {
			case <-sup.done:
			default:
				t.Fatal("run goroutine did not exit after stop()")
			}
			// An INTENTIONAL stop leaves exitError nil (hub Close => sub.Err() nil).
			if got := sup.exitError(); got != nil {
				t.Errorf("exitError() after intentional stop = %v, want nil", got)
			}
		})
	}
}

// TestSupervisor_SubscribeError proves newSupervisor surfaces the Subscribe
// failure and starts no goroutine (nothing to leak) when the agent refuses.
func TestSupervisor_SubscribeError(t *testing.T) {
	t.Parallel()
	fa := &fakeAgent{subErr: errSubscribe}
	sup, err := newSupervisor(fa)
	if !errors.Is(err, errSubscribe) {
		t.Fatalf("newSupervisor() error = %v, want %v", err, errSubscribe)
	}
	if sup != nil {
		t.Fatalf("newSupervisor() = %v on error, want nil supervisor", sup)
	}
}

// TestSupervisor_DropsAreScoped proves neither drop path is a global flush:
// a per-id drop removes only its own ToolExecutionID, and the turn-terminal
// backstop sweeps only the terminating loop — a gate on another loop survives.
func TestSupervisor_DropsAreScoped(t *testing.T) {
	t.Parallel()

	fs := newFakeSub()
	fa := &fakeAgent{sub: fs}
	sup, err := newSupervisor(fa)
	if err != nil {
		t.Fatalf("newSupervisor() unexpected error = %v", err)
	}

	teidA, teidB := mkID(0x01), mkID(0x02)
	loopA, loopB := mkID(0xF1), mkID(0xF2)

	// Open one gate per loop.
	fs.feed(event.PermissionRequested{Header: loopHeader(loopA), ToolExecutionID: teidA, Request: tool.BashRequest{Command: "a"}})
	fs.feed(event.UserInputRequested{Header: loopHeader(loopB), ToolExecutionID: teidB, Question: "b"})
	if !pollUntil(t, func() bool {
		_, okA := sup.lookup(teidA)
		_, okB := sup.lookup(teidB)
		return okA && okB
	}) {
		t.Fatalf("both gates not recorded within %v", pollDeadline)
	}

	// Per-id drop is scoped: completing teidA must leave teidB intact.
	fs.feed(event.ToolCallCompleted{Header: loopHeader(loopA), ToolExecutionID: teidA})
	if !pollUntil(t, func() bool { _, ok := sup.lookup(teidA); return !ok }) {
		t.Fatalf("teidA not dropped by tool-completed within %v", pollDeadline)
	}
	if _, ok := sup.lookup(teidB); !ok {
		t.Fatal("per-id drop of teidA also removed teidB; drop is not scoped")
	}

	// Loop-scoped backstop is scoped: re-open a gate on loopA, then a TurnDone
	// for loopB must sweep only loopB, leaving loopA's gate standing.
	teidC := mkID(0x03)
	fs.feed(event.PermissionRequested{Header: loopHeader(loopA), ToolExecutionID: teidC, Request: tool.BashRequest{Command: "c"}})
	if !pollUntil(t, func() bool { _, ok := sup.lookup(teidC); return ok }) {
		t.Fatalf("teidC not recorded within %v", pollDeadline)
	}
	fs.feed(event.TurnDone{Header: loopHeader(loopB)})
	if !pollUntil(t, func() bool { _, ok := sup.lookup(teidB); return !ok }) {
		t.Fatalf("teidB not dropped by TurnDone{loopB} within %v", pollDeadline)
	}
	if _, ok := sup.lookup(teidC); !ok {
		t.Fatal("TurnDone{loopB} also swept loopA's gate teidC; dropLoop is a global flush, not scoped")
	}

	if err := sup.stop(); err != nil {
		t.Errorf("stop() error = %v", err)
	}
	if got := sup.exitError(); got != nil {
		t.Errorf("exitError() after intentional stop = %v, want nil", got)
	}
}

// TestSupervisor_ExitErrorOnLoss proves a hub-forced subscription loss (channel
// closes WITHOUT an intentional stop) surfaces the loss cause via exitError, so
// Task 13's reconnect endpoint can refuse to serve a stale, frozen registry.
func TestSupervisor_ExitErrorOnLoss(t *testing.T) {
	t.Parallel()

	fs := newFakeSub()
	fa := &fakeAgent{sub: fs}
	sup, err := newSupervisor(fa)
	if err != nil {
		t.Fatalf("newSupervisor() unexpected error = %v", err)
	}

	// Simulate the hub dropping this subscriber: Err() now reports the cause and
	// the channel closes, without any stop() call.
	fs.fail(errStreamLost)

	select {
	case <-sup.done:
	case <-time.After(pollDeadline):
		t.Fatalf("run goroutine did not exit after simulated loss within %v", pollDeadline)
	}
	if got := sup.exitError(); !errors.Is(got, errStreamLost) {
		t.Fatalf("exitError() after loss = %v, want %v", got, errStreamLost)
	}
	// stop() after a loss is a safe no-op (fail reused the close-once).
	if err := sup.stop(); err != nil {
		t.Errorf("stop() after loss error = %v", err)
	}
}

// containsGate reports whether snapshot holds want (order-independent).
func containsGate(snapshot []openGate, want openGate) bool {
	for _, g := range snapshot {
		if g == want {
			return true
		}
	}
	return false
}
