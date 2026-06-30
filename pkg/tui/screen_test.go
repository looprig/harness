package tui

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/hub"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/transcript"
	"github.com/ciram-co/looprig/pkg/tui/components"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// compile-time assertion that the test double satisfies the (widened) Agent
// interface; if a method is added or its signature drifts, this fails to build.
var _ Agent = (*fakeAgent)(nil)

// fakeAgent is a scriptable Agent test double. It records calls and returns the
// configured reader/error/bool so Screen behavior can be exercised without a real
// session.
type fakeAgent struct {
	// submit recorder: a configured id/error is returned, and the last call's
	// blocks are captured so a test can assert the wrapper forwarded them. When
	// submitID is zero it defaults to a fixed deterministic id so callers always get
	// a usable correlation id.
	submitID         uuid.UUID
	submitErr        error
	submitCalled     bool
	lastSubmitBlocks []content.Block

	// primaryLoopID is returned by PrimaryLoopID; zero is a valid fixed id for the
	// single-loop default filter.
	primaryLoopID uuid.UUID

	interruptCancelled bool
	interruptErr       error

	closeCalled  bool
	closeErr     error
	acceptsImage bool

	// subscribe recorder: the configured stream/error is returned, the last filter
	// is captured so a test can assert the wrapper forwarded it, and subscribeCount
	// counts Subscribe calls so a test can assert the TUI subscribes exactly ONCE per
	// session (one session-lifetime subscription, never re-subscribed per turn).
	subStream      event.Subscription
	subErr         error
	subFilter      event.EventFilter
	subscribeCount int

	// gate-trio recorders: the configured error is returned, and the last call's
	// arguments are captured so a test can assert the wrapper forwarded them.
	approveErr    error
	denyErr       error
	answerErr     error
	approveCalled bool
	denyCalled    bool
	answerCalled  bool
	lastLoopID    uuid.UUID
	lastCallID    uuid.UUID
	lastScope     tool.ApprovalScope
	lastAnswer    string

	// replay-backlog recorder: backlog is returned verbatim (a restored session's
	// historical Enduring events for repaint; nil for a NEW session), replayErr is the
	// configured failure, and replayCalled records that the restore seam was exercised.
	backlog      []event.Event
	replayErr    error
	replayCalled bool

	// export recorder: exportSrc/exportPrompts are returned by ExportSource on success;
	// exportErr (e.g. a *journalsource.ExportUnavailableError) is returned instead when
	// set, and exportCalled records that the seam was exercised.
	exportSrc     transcript.RecordSource
	exportPrompts transcript.SystemPromptResolver
	exportErr     error
	exportCalled  bool
}

// fixedFakeSubmitID is the deterministic InputID a fakeAgent returns when no
// submitID is configured, so a test always gets a non-zero correlation id.
var fixedFakeSubmitID = uuid.UUID{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00}

func (f *fakeAgent) Submit(_ context.Context, blocks []content.Block) (uuid.UUID, error) {
	f.submitCalled = true
	f.lastSubmitBlocks = blocks
	if f.submitErr != nil {
		return uuid.UUID{}, f.submitErr
	}
	if f.submitID.IsZero() {
		return fixedFakeSubmitID, nil
	}
	return f.submitID, nil
}

func (f *fakeAgent) PrimaryLoopID() uuid.UUID { return f.primaryLoopID }

func (f *fakeAgent) Interrupt(_ context.Context) (bool, error) {
	return f.interruptCancelled, f.interruptErr
}

func (f *fakeAgent) Close(_ context.Context) error {
	f.closeCalled = true
	return f.closeErr
}

func (f *fakeAgent) AcceptsImages() bool { return f.acceptsImage }

func (f *fakeAgent) Subscribe(filter event.EventFilter) (EventStream, error) {
	f.subscribeCount++
	f.subFilter = filter
	if f.subErr != nil {
		return nil, f.subErr
	}
	return f.subStream, nil
}

func (f *fakeAgent) Approve(_ context.Context, loopID, callID uuid.UUID, scope tool.ApprovalScope) error {
	f.approveCalled = true
	f.lastLoopID = loopID
	f.lastCallID = callID
	f.lastScope = scope
	return f.approveErr
}

func (f *fakeAgent) Deny(_ context.Context, loopID, callID uuid.UUID) error {
	f.denyCalled = true
	f.lastLoopID = loopID
	f.lastCallID = callID
	return f.denyErr
}

func (f *fakeAgent) ProvideAnswer(_ context.Context, loopID, callID uuid.UUID, answer string) error {
	f.answerCalled = true
	f.lastLoopID = loopID
	f.lastCallID = callID
	f.lastAnswer = answer
	return f.answerErr
}

func (f *fakeAgent) ReplayBacklog(_ context.Context) ([]event.Event, error) {
	f.replayCalled = true
	if f.replayErr != nil {
		return nil, f.replayErr
	}
	return f.backlog, nil
}

func (f *fakeAgent) ExportSource(context.Context) (transcript.RecordSource, transcript.SystemPromptResolver, error) {
	f.exportCalled = true
	if f.exportErr != nil {
		return nil, nil, f.exportErr
	}
	return f.exportSrc, f.exportPrompts, nil
}

// fakeSubscription is a test-controlled event.Subscription: a buffered channel a
// test pushes events onto (push) plus an idempotent Close and a configurable Err.
// It models the session-lifetime stream the Screen reads via subNext. The channel
// is buffered so push never blocks the test goroutine; closeErr is what Err reports
// after a hub-forced loss (nil mimics an intentional Close).
type fakeSubscription struct {
	ch       chan event.Event
	closeErr error
	closed   bool
}

// newFakeSubscription builds a fakeSubscription with a generously buffered channel
// so a test can stage several events without a reader draining them.
func newFakeSubscription() *fakeSubscription {
	return &fakeSubscription{ch: make(chan event.Event, 64)}
}

func (s *fakeSubscription) Events() <-chan event.Event { return s.ch }

// Close is the consumer's idempotent teardown: it closes the channel once so a
// subsequent subNext receives !ok. It records no error (Err stays whatever was set).
func (s *fakeSubscription) Close() error {
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	return nil
}

func (s *fakeSubscription) Err() error { return s.closeErr }

// push stages an event on the subscription channel (non-blocking — the buffer is
// large enough for the tests). It panics if the channel is full so a test bug
// surfaces loudly rather than hanging.
func (s *fakeSubscription) push(ev event.Event) {
	select {
	case s.ch <- ev:
	default:
		panic("fakeSubscription buffer full")
	}
}

// compile-time assertion that fakeSubscription satisfies the consumer contract.
var _ event.Subscription = (*fakeSubscription)(nil)

// fakeOpen returns an OpenAgent thunk that yields the given agent.
func fakeOpen(a Agent) OpenAgent {
	return func(context.Context) (Agent, error) { return a, nil }
}

// callID returns a deterministic non-zero UUID for a test, distinguishing gates by
// a single byte so ToolExecutionID correlation can be asserted.
func callID(b byte) uuid.UUID {
	var u uuid.UUID
	u[0] = b
	return u
}

// drainCmd executes cmd, recursively running any BatchMsg/sequenceMsg leaves it
// produces so the underlying I/O closures (e.g. submitCmd's agent.Submit call) all
// run. A nil cmd is a no-op. It is the test-side analogue of the Bubble Tea runtime
// fanning out a batched command.
func drainCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			drainCmd(t, c)
		}
	}
}

// firstBlockText returns the first text-block text in blocks, or "".
func firstBlockText(blocks []content.Block) string {
	for _, b := range blocks {
		if tb, ok := b.(*content.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

// updateScreen drives m.Update with msg and returns the concrete Screen plus the
// cmd, failing the test if the model is not a Screen.
func updateScreen(t *testing.T, m Screen, msg tea.Msg) (Screen, tea.Cmd) {
	t.Helper()
	model, cmd := m.Update(msg)
	got, ok := model.(Screen)
	if !ok {
		t.Fatalf("Update returned %T, want Screen", model)
	}
	return got, cmd
}

// feed drives one synthetic stream event through Update and returns the new Screen.
func feed(t *testing.T, m Screen, ev event.Event) Screen {
	t.Helper()
	m, _ = updateScreen(t, m, eventMsg{ev: ev})
	return m
}

// runningScreen returns a fresh Screen wired for a live turn: a non-nil session
// subscription (subNext targets must be non-nil) and StatusRunning. The returned
// fakeSubscription lets a test stage further events; most tests feed via the feed
// helper (a direct eventMsg) and only check the re-arm cmd is non-nil.
func runningScreen(t *testing.T, agent Agent) Screen {
	t.Helper()
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m.sub = newFakeSubscription()
	m.status = StatusRunning
	return m
}

// lastCommitted returns the most recently committed transcript entry, failing if
// there are none.
func lastCommitted(t *testing.T, m Screen) entry {
	t.Helper()
	if len(m.transcript.committed) == 0 {
		t.Fatal("no committed transcript entries")
	}
	return m.transcript.committed[len(m.transcript.committed)-1]
}

// committedText returns the first text-block text of e, or "".
func committedText(e entry) string {
	for _, b := range e.Blocks {
		if tb, ok := b.(*content.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

func TestNew(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})

	if m.status != StatusIdle {
		t.Errorf("New status = %d, want StatusIdle (%d)", m.status, StatusIdle)
	}
	if m.Agent() != agent {
		t.Errorf("New Agent() = %p, want %p", m.Agent(), agent)
	}
	if m.interaction.mode != modeCompose {
		t.Errorf("New interaction mode = %d, want modeCompose", m.interaction.mode)
	}
}

func TestInit(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})

	// Init focuses the composer (cursor blink) and queues the system-ready entry,
	// so it must return a non-nil batched command.
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() = nil, want non-nil cmd")
	}
}

func TestScreenIsTeaModel(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	var _ tea.Model = New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
}

func TestWindowSizeMsg(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})

	// Before any WindowSizeMsg, the view is empty (not ready).
	if v := m.View().Content; v != "" {
		t.Errorf("View() before ready = %q, want empty", v)
	}

	got, _ := updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	if got.width != 80 || got.height != 24 {
		t.Errorf("width,height = %d,%d, want 80,24", got.width, got.height)
	}
	if !got.ready {
		t.Error("ready = false after WindowSizeMsg, want true")
	}
	if got.scrollback.width != 80 {
		t.Errorf("scrollback width = %d, want 80 (propagated)", got.scrollback.width)
	}
	if v := got.View().Content; v == "" {
		t.Error("View() after ready = empty, want non-empty")
	}
}

// TestWindowSizeMsgNoScrollbackPrint pins half of the resize-artifact root cause: an
// ordinary WindowSizeMsg must ONLY update dimensions + repaint the View — it must not
// return a command (which, on the flush paths, would emit a tea.Println / insertAbove
// that writes to native scrollback). A nil command guarantees a resize cannot itself
// print to scrollback; combined with the width clamp (the other half — see
// TestSurfaceViewNeverExceedsWidth), the resize cascade is eliminated. The startup
// banner path is covered separately by TestStartupBannerWaitsForFirstFrame and still
// returns no print command. The case with committed content present proves ordinary
// resizes stay no-op even when there is history that a stray flush could reprint.
func TestWindowSizeMsgNoScrollbackPrint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		withCommits bool
	}{
		{name: "fresh screen", withCommits: false},
		{name: "with committed history", withCommits: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
			m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
			if tt.withCommits {
				// Commit a couple of entries so a stray flush WOULD have something to print.
				m.transcript = m.transcript.CommitSystem("first")
				m.transcript = m.transcript.CommitSystem("second")
				m, _ = updateScreen(t, m, systemReadyMsg{})
			}

			// A resize (and several drag steps) must each return a nil command.
			for _, size := range []tea.WindowSizeMsg{
				{Width: 70, Height: 24}, {Width: 50, Height: 24}, {Width: 30, Height: 24}, {Width: 90, Height: 40},
			} {
				var cmd tea.Cmd
				m, cmd = updateScreen(t, m, size)
				if cmd != nil {
					t.Errorf("WindowSizeMsg(%dx%d) returned a non-nil command; a resize must not flush/print to scrollback", size.Width, size.Height)
				}
			}
		})
	}
}

// TestViewScrollbackFirstInvariant pins the scrollback-first guarantee at the
// place it now lives: Screen.View() must return a tea.View that keeps the program
// on the NORMAL screen (AltScreen == false) and never captures the mouse
// (MouseMode == tea.MouseModeNone). Both are the v2 zero values, but asserting them
// here — on the actual View() output — proves the intent is realized rather than
// merely defaulted, and catches any future code that flips an alt-screen/mouse field.
func TestViewScrollbackFirstInvariant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		resize *tea.WindowSizeMsg // nil = no WindowSizeMsg (view not yet ready)
	}{
		{name: "before window size (not ready)", resize: nil},
		{name: "after window size (ready, composed)", resize: &tea.WindowSizeMsg{Width: 80, Height: 24}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
			if tt.resize != nil {
				m, _ = updateScreen(t, m, *tt.resize)
			}

			v := m.View()
			if v.AltScreen {
				t.Error("View().AltScreen = true, want false (scrollback-first stays on the normal screen so tea.Println writes to native scrollback)")
			}
			if v.MouseMode != tea.MouseModeNone {
				t.Errorf("View().MouseMode = %v, want tea.MouseModeNone (scrollback-first never captures the mouse)", v.MouseMode)
			}
		})
	}
}

// TestViewRequestsAllKeysAsEscapeCodes pins the keyboard-enhancement request needed
// for Shift+Enter in the composer. v2's default Kitty flag is just "disambiguate
// escape codes" (flag 1), under which the Kitty spec keeps Enter/Tab/Backspace as
// their legacy bytes — so Shift+Enter arrives as a plain Enter and submits instead of
// inserting a newline. Only ReportAllKeysAsEscapeCodes (flag 8) makes the terminal
// report a MODIFIED Enter as a distinct escape code (CSI 13;2u → KeyEnter+ModShift).
// View() must request it. The request is harmless on terminals that don't support the
// Kitty protocol (they ignore it); the Ctrl+J fallback covers those. Enabling it must
// NOT disturb the scrollback-first invariant (AltScreen off, mouse off).
func TestViewRequestsAllKeysAsEscapeCodes(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	v := m.View()
	if !v.KeyboardEnhancements.ReportAllKeysAsEscapeCodes {
		t.Error("View().KeyboardEnhancements.ReportAllKeysAsEscapeCodes = false, want true (required for Shift+Enter to be reported as a distinct key on Kitty-protocol terminals)")
	}
	// The enhancement must not flip the scrollback-first invariant.
	if v.AltScreen {
		t.Error("View().AltScreen = true, want false (keyboard enhancements must not enable alt-screen)")
	}
	if v.MouseMode != tea.MouseModeNone {
		t.Errorf("View().MouseMode = %v, want tea.MouseModeNone (keyboard enhancements must not capture the mouse)", v.MouseMode)
	}
}

// TestViewComposesSurfaceNoViewport asserts the View is the active surface (the
// borderless composer panel + status line), never a transcript viewport, and that
// committed entries are NOT re-rendered into the View (they live in native
// scrollback). A committed user line's text must not appear in the View.
func TestViewComposesSurfaceNoViewport(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 60, Height: 18})
	m.transcript = m.transcript.CommitUser([]content.Block{&content.TextBlock{Text: "committed-history-line"}})

	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "Type a message…") {
		t.Errorf("View() missing the composer surface; got %q", view)
	}
	if strings.Contains(view, "committed-history-line") {
		t.Errorf("View() re-rendered a committed entry; it belongs in native scrollback, got %q", view)
	}
}

func TestViewShowsUnprintedStartupBanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		banner AgentBanner
		text   string
	}{
		{name: "agent name", banner: AgentBanner{Name: "SWE"}, text: "SWE"},
		{name: "name and description", banner: AgentBanner{Name: "SWE", Description: "coding agent"}, text: "SWE — coding agent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent), tt.banner)
			m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 60, Height: 18})
			m, cmd := updateScreen(t, m, systemReadyMsg{})
			if cmd != nil {
				t.Fatal("systemReady cmd = non-nil, want nil until a real scrollback entry commits")
			}

			if got := len(m.transcript.committed); got != 1 {
				t.Fatalf("committed startup entries = %d, want 1", got)
			}
			if got := committedText(m.transcript.committed[0]); got != tt.text {
				t.Fatalf("startup banner text = %q, want %q", got, tt.text)
			}

			view := stripANSI(m.View().Content)
			if !strings.Contains(view, tt.text) {
				t.Errorf("View() missing unprinted startup banner %q in active surface; got %q", tt.text, view)
			}
			if !strings.Contains(view, "Type a message…") {
				t.Errorf("View() missing composer; got %q", view)
			}
		})
	}
}

// TestViewRendersLiveTail asserts the in-progress (live) assistant segment renders
// in the View's capped live tail above the composer panel — it is NOT yet committed, so
// it cannot be in scrollback.
func TestViewRendersLiveTail(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 60, Height: 18})
	m = feed(t, m, event.TokenDelta{Chunk: &content.TextChunk{Text: "live narration"}})

	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "live narration") {
		t.Errorf("View() missing the live tail narration; got %q", view)
	}
}

// TestEventRoutesToBothReducers is the core router invariant: a single subscription
// event reaches BOTH the transcript reducer (which accumulates the live segment) AND
// the interaction reducer, and subNext keeps reading (a non-nil cmd is returned so
// the next event is pulled even though the loop may be gated).
func TestEventRoutesToBothReducers(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)

	m, cmd := updateScreen(t, m, eventMsg{ev: event.TokenDelta{Chunk: &content.TextChunk{Text: "hello"}}})

	if m.transcript.live.Text != "hello" {
		t.Errorf("transcript live.Text = %q, want %q (event not routed to transcript)", m.transcript.live.Text, "hello")
	}
	if cmd == nil {
		t.Error("event cmd = nil, want non-nil (subNext keeps reading the subscription)")
	}
}

// TestPermissionRequestedEnqueuesAndTracksGate covers the gate-open path: a
// PermissionRequested ENQUEUES a prompt in the interaction model (so the bottom box
// becomes the permission control) and REMEMBERS the gate in the transcript (so the
// call's committed card can read "Approved …"/"Denied …"), but commits NOTHING —
// committing at the gate would duplicate the step's prose/card in append-only
// scrollback. The stream keeps draining.
func TestPermissionRequestedEnqueuesAndTracksGate(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)

	req := tool.BashRequest{Command: "rm -rf /tmp/x"}
	m, cmd := updateScreen(t, m, eventMsg{ev: event.PermissionRequested{ToolExecutionID: callID(1), Request: req}})

	// Interaction: one pending prompt, mode switched to permission.
	if m.interaction.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1 (prompt not enqueued)", m.interaction.PendingCount())
	}
	if m.interaction.mode != modePermissionPrompt {
		t.Errorf("interaction mode = %d, want modePermissionPrompt", m.interaction.mode)
	}
	// Transcript: the gate commits NOTHING but is remembered (gatePending) by ToolExecutionID.
	if len(m.transcript.committed) != 0 {
		t.Fatalf("committed = %d entries, want 0 (the gate must not commit)", len(m.transcript.committed))
	}
	if got := m.transcript.live.gateDecisions[callID(1)]; got != gatePending {
		t.Errorf("gateDecisions[callID(1)] = %v, want gatePending", got)
	}
	// The stream keeps draining (the gate, not the stream, blocks the loop).
	if cmd == nil {
		t.Error("PermissionRequested cmd = nil, want non-nil (subNext keeps reading)")
	}
}

// TestPermissionKeyDispatchesTrio covers the freeze fix's second half: a key in
// permission mode produces the right bounded command, which when executed calls the
// agent's Approve/Deny (the trio), and the prompt is popped from the queue.
func TestPermissionKeyDispatchesTrio(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		key         tea.KeyPressMsg
		wantApprove bool
		wantDeny    bool
		wantScope   tool.ApprovalScope
	}{
		{name: "y approves once", key: runeKey('y'), wantApprove: true, wantScope: tool.ScopeOnce},
		{name: "s approves session", key: runeKey('s'), wantApprove: true, wantScope: tool.ScopeSession},
		{name: "n denies", key: runeKey('n'), wantDeny: true},
		{name: "esc denies", key: tea.KeyPressMsg{Code: tea.KeyEsc}, wantDeny: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := runningScreen(t, agent)
			// The request carries a producing loop id on its Header; the prompt is
			// stamped with it and the gate reply must be dispatched to THAT loop, so a
			// subagent loop's gate is never answered by routing to the primary.
			gateLoop := callID(9)
			m = feed(t, m, event.PermissionRequested{
				Header:          event.Header{Coordinates: identity.Coordinates{LoopID: gateLoop}},
				ToolExecutionID: callID(7),
				Request:         tool.BashRequest{Command: "ls"},
			})

			m, cmd := updateScreen(t, m, tt.key)
			if cmd == nil {
				t.Fatal("permission key cmd = nil, want a bounded dispatch cmd")
			}
			// The prompt is popped optimistically (queue empties, back to compose).
			if m.interaction.PendingCount() != 0 {
				t.Errorf("PendingCount = %d, want 0 (prompt popped)", m.interaction.PendingCount())
			}
			// Execute the cmd: it must reach the agent's trio.
			cmd()
			if tt.wantApprove {
				if !agent.approveCalled {
					t.Error("Approve not called")
				}
				if agent.lastScope != tt.wantScope {
					t.Errorf("Approve scope = %v, want %v", agent.lastScope, tt.wantScope)
				}
			}
			if tt.wantDeny && !agent.denyCalled {
				t.Error("Deny not called")
			}
			if agent.lastCallID != callID(7) {
				t.Errorf("dispatched ToolExecutionID = %v, want %v", agent.lastCallID, callID(7))
			}
			if agent.lastLoopID != gateLoop {
				t.Errorf("dispatched LoopID = %v, want producing loop %v", agent.lastLoopID, gateLoop)
			}
		})
	}
}

// TestAnswerKeyDispatchesProvideAnswer covers the AskUser free-text gate: typing an
// answer and submitting dispatches provideAnswerCmd, which forwards the typed text
// to the agent and pops the prompt.
func TestAnswerKeyDispatchesProvideAnswer(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	// A free-text AskUser (no choices) enters answer mode. The request carries a
	// producing loop id, which the answer must be dispatched back to.
	gateLoop := callID(9)
	m = feed(t, m, event.UserInputRequested{
		Header:          event.Header{Coordinates: identity.Coordinates{LoopID: gateLoop}},
		ToolExecutionID: callID(3),
		Question:        "name?",
	})
	if m.interaction.mode != modeAnswerPrompt {
		t.Fatalf("mode = %d, want modeAnswerPrompt", m.interaction.mode)
	}

	// Type "neo" then submit.
	for _, r := range "neo" {
		m, _ = updateScreen(t, m, runeKey(r))
	}
	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("answer submit cmd = nil, want provideAnswerCmd")
	}
	if m.interaction.PendingCount() != 0 {
		t.Errorf("PendingCount = %d, want 0 (answer popped)", m.interaction.PendingCount())
	}
	cmd()
	if !agent.answerCalled || agent.lastAnswer != "neo" || agent.lastCallID != callID(3) {
		t.Errorf("ProvideAnswer call = (called %v, answer %q, id %v), want (true, %q, %v)",
			agent.answerCalled, agent.lastAnswer, agent.lastCallID, "neo", callID(3))
	}
	if agent.lastLoopID != gateLoop {
		t.Errorf("dispatched LoopID = %v, want producing loop %v", agent.lastLoopID, gateLoop)
	}
}

// TestChoiceEscInterrupts covers the Esc precedence in choice mode: Esc interrupts
// the turn (uiInterrupt → interruptTurn) WITHOUT popping the prompt; the model
// flips to Interrupting.
func TestChoiceEscInterrupts(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m = feed(t, m, event.UserInputRequested{ToolExecutionID: callID(4), Question: "pick", Choices: []string{"a", "b"}})
	if m.interaction.mode != modeChoicePrompt {
		t.Fatalf("mode = %d, want modeChoicePrompt", m.interaction.mode)
	}

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("choice esc cmd = nil, want interruptTurn")
	}
	if m.status != StatusInterrupting {
		t.Errorf("status = %d, want StatusInterrupting", m.status)
	}
}

// TestTerminalEventClearsPromptQueue covers the queue-clearing invariant: a terminal
// stream event (TurnDone/TurnFailed/TurnInterrupted) clears every pending prompt and
// returns the interaction surface to compose.
func TestTerminalEventClearsPromptQueue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		term event.Event
	}{
		{name: "turn done", term: event.TurnDone{}},
		{name: "turn failed", term: event.TurnFailed{Err: errors.New("x")}},
		{name: "turn interrupted", term: event.TurnInterrupted{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := runningScreen(t, agent)
			m = feed(t, m, event.PermissionRequested{ToolExecutionID: callID(1), Request: tool.BashRequest{Command: "x"}})
			if m.interaction.PendingCount() != 1 {
				t.Fatalf("setup: PendingCount = %d, want 1", m.interaction.PendingCount())
			}

			m = feed(t, m, tt.term)
			if m.interaction.PendingCount() != 0 {
				t.Errorf("PendingCount = %d, want 0 (terminal clears the queue)", m.interaction.PendingCount())
			}
			if m.interaction.mode != modeCompose {
				t.Errorf("mode = %d, want modeCompose (restored)", m.interaction.mode)
			}
		})
	}
}

// TestSubmitFireAndForgetIdle covers uiSubmit at Idle: NO user row is committed at
// submit (the optimistic commit is gone — the row is event-driven now), Submit is
// fired fire-and-forget, and the status stays Idle — it flips to Running only when
// the loop's TurnStarted arrives on the subscription. The composer resets.
func TestSubmitFireAndForgetIdle(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m.interaction.input.SetValue("hello there")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("submit cmd = nil, want non-nil (submitCmd)")
	}
	if m.status != StatusIdle {
		t.Errorf("status = %d, want StatusIdle (status follows TurnStarted, not submit)", m.status)
	}
	// No optimistic user row: the authoritative row comes from the loop's TurnStarted
	// Message, not from submit.
	if len(m.transcript.committed) != 0 {
		t.Errorf("committed = %d, want 0 (no optimistic user row; it is event-driven)", len(m.transcript.committed))
	}
	if m.interaction.input.Value() != "" {
		t.Errorf("composer = %q, want reset", m.interaction.input.Value())
	}
	// Executing the cmd must reach Submit (fire-and-forget).
	drainCmd(t, cmd)
	if !agent.submitCalled {
		t.Error("Submit not called; the fire-and-forget path must call agent.Submit")
	}
	if got := firstBlockText(agent.lastSubmitBlocks); got != "hello there" {
		t.Errorf("Submit blocks text = %q, want %q", got, "hello there")
	}
}

// TestSubmitFireAndForgetWhileRunning covers uiSubmit at Running: NO user row is
// committed at submit (event-driven now) and Submit is fired — the LOOP owns
// queueing, so Screen keeps no queue and the status stays Running. The composer
// resets.
func TestSubmitFireAndForgetWhileRunning(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m.interaction.input.SetValue("second one")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Error("submit cmd = nil, want non-nil (submitCmd)")
	}
	if len(m.transcript.committed) != 0 {
		t.Errorf("committed = %d, want 0 (no optimistic user row; it is event-driven)", len(m.transcript.committed))
	}
	if m.status != StatusRunning {
		t.Errorf("status = %d, want StatusRunning (unchanged; the loop queues)", m.status)
	}
	if m.interaction.input.Value() != "" {
		t.Errorf("composer = %q, want reset", m.interaction.input.Value())
	}
	drainCmd(t, cmd)
	if !agent.submitCalled {
		t.Error("Submit not called while Running; the loop owns queueing now")
	}
}

// TestSubmitBadAttachmentCommitsError covers uiSubmit with a buildBlocks failure:
// no turn starts, an error entry is committed, and the agent is untouched. (The
// composer was already reset by the interaction model on Enter.)
func TestSubmitBadAttachmentCommitsError(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m.interaction.input.SetValue("@nope.pem") // .pem is a denied extension → buildBlocks error

	m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.status != StatusIdle {
		t.Errorf("status = %d, want StatusIdle (no turn)", m.status)
	}
	rec := lastCommitted(t, m)
	if rec.Kind != kindNotice || rec.Level != noticeError {
		t.Errorf("committed = (kind %d, level %d), want (kindNotice, noticeError)", rec.Kind, rec.Level)
	}
	if agent.closeCalled {
		t.Error("agent touched on a buildBlocks error, want untouched")
	}
}

// TestSubmitEmptyIsNoop covers uiSubmit on whitespace-only input: a no-op, no commit,
// no turn. (The interaction model returns uiNoop, so Screen does nothing.)
func TestSubmitEmptyIsNoop(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m.interaction.input.SetValue("   ")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd != nil {
		t.Errorf("cmd = non-nil, want nil")
	}
	if len(m.transcript.committed) != 0 {
		t.Errorf("committed = %d, want 0", len(m.transcript.committed))
	}
	if m.status != StatusIdle {
		t.Errorf("status = %d, want StatusIdle", m.status)
	}
}

// TestRunSlashHelp covers uiRunSlash for /help: the help listing is committed as a
// system entry (and flushed); no turn starts.
func TestRunSlashHelp(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m.interaction.input.SetValue("/help")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Error("/help cmd = nil, want non-nil (flush of the system entry)")
	}
	rec := lastCommitted(t, m)
	if rec.Kind != kindNotice || rec.Level != noticeInfo {
		t.Fatalf("committed = (kind %d, level %d), want (kindNotice, noticeInfo)", rec.Kind, rec.Level)
	}
	text := committedText(rec)
	for _, c := range components.SlashCommands {
		if !strings.Contains(text, c.Name) {
			t.Errorf("help text missing %q; got %q", c.Name, text)
		}
	}
}

// TestRunSlashClearWhileIdle covers uiRunSlash for /clear at Idle: it flips to
// Resetting and returns the reopen cmd.
func TestRunSlashClearWhileIdle(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m.interaction.input.SetValue("/clear")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Error("/clear cmd = nil, want non-nil (reopen)")
	}
	if m.status != StatusResetting {
		t.Errorf("status = %d, want StatusResetting", m.status)
	}
}

// TestRunSlashClearWhileRunningIsNoop covers uiRunSlash for /clear at Running: a
// no-op (the reopen is blocked while a turn is live).
func TestRunSlashClearWhileRunningIsNoop(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m.interaction.input.SetValue("/clear")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Errorf("/clear-while-running cmd = non-nil, want nil (no-op)")
	}
	if m.status != StatusRunning {
		t.Errorf("status = %d, want StatusRunning (unchanged)", m.status)
	}
}

// TestEscWhileRunningInterrupts covers the no-prompt Esc precedence: Esc with no
// active prompt interrupts a running turn (flips to Interrupting + dispatches the
// bounded Interrupt). The loop owns queueing now, so there is no Screen-side queue
// to clear.
func TestEscWhileRunningInterrupts(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{interruptCancelled: true}
	m := runningScreen(t, agent)

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Error("Esc cmd = nil, want non-nil (interruptTurn)")
	}
	if m.status != StatusInterrupting {
		t.Errorf("status = %d, want StatusInterrupting", m.status)
	}
}

// TestEscWhileIdleIsNoop covers Esc with no prompt and no turn: a no-op.
func TestEscWhileIdleIsNoop(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd != nil {
		t.Errorf("Esc-while-idle cmd = non-nil, want nil")
	}
	if m.status != StatusIdle {
		t.Errorf("status = %d, want StatusIdle", m.status)
	}
}

// TestCtrlCQuits covers the GLOBAL ctrl+c: it returns a close+quit sequence even
// when a prompt is active (the global keys outrank prompt routing).
func TestCtrlCQuits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		withPrompt bool
	}{
		{name: "no prompt", withPrompt: false},
		{name: "with prompt active", withPrompt: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := runningScreen(t, agent)
			if tt.withPrompt {
				m = feed(t, m, event.PermissionRequested{ToolExecutionID: callID(1), Request: tool.BashRequest{Command: "x"}})
			}
			_, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			if cmd == nil {
				t.Fatal("Ctrl+C cmd = nil, want non-nil (close + quit sequence)")
			}
		})
	}
}

// TestCtrlTTogglesExpandGlobally covers the GLOBAL ctrl+t: it flips the expand flag
// (re-render only, nil cmd) in any status AND even with a prompt active, never
// touching the turn or the prompt queue. The default is EXPANDED (append-only
// scrollback can't retroactively expand printed output), so the FIRST ctrl+t
// collapses and the SECOND expands again.
func TestCtrlTTogglesExpandGlobally(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     Status
		withPrompt bool
	}{
		{name: "idle", status: StatusIdle},
		{name: "running", status: StatusRunning},
		{name: "running with prompt", status: StatusRunning, withPrompt: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
			m.status = tt.status
			if tt.status != StatusIdle {
				m.sub = newFakeSubscription()
			}
			if tt.withPrompt {
				m = feed(t, m, event.PermissionRequested{ToolExecutionID: callID(1), Request: tool.BashRequest{Command: "x"}})
			}
			wantPending := m.interaction.PendingCount()

			// A fresh Screen defaults to expanded.
			if !m.expand {
				t.Fatalf("fresh Screen expand = false, want true (default expanded)")
			}

			// First ctrl+t collapses.
			m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
			if cmd != nil {
				t.Errorf("ctrl+t cmd = non-nil, want nil (re-render only)")
			}
			if m.expand {
				t.Errorf("expand = true after first ctrl+t, want false (collapsed)")
			}
			if m.status != tt.status {
				t.Errorf("status = %d, want unchanged %d", m.status, tt.status)
			}
			if m.interaction.PendingCount() != wantPending {
				t.Errorf("PendingCount changed by ctrl+t: %d, want %d", m.interaction.PendingCount(), wantPending)
			}
			// Second toggle expands again.
			m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
			if !m.expand {
				t.Errorf("expand = false after second ctrl+t, want true (expanded)")
			}
		})
	}
}

// TestCtrlTFlipsThinkingNotToolOutputInLiveTail pins that ctrl+t flips the live tail's
// THINKING block (full "│ " body ↔ compact "thinking · N lines" summary), while the
// tool-result preview stays HARD-capped to previewLineCap lines in BOTH states — the
// ctrl+t fold no longer un-caps tool output (so a huge result can't fill the live tail).
func TestCtrlTFlipsThinkingNotToolOutputInLiveTail(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 40})

	// A live segment with multi-line thinking AND a completed tool card whose result
	// exceeds previewLineCap, so the thinking fold flips and the tool cap is observable.
	resultLines := make([]string, 0, previewLineCap+3)
	for i := 0; i < previewLineCap+3; i++ {
		resultLines = append(resultLines, "result-line-"+strconv.Itoa(i))
	}
	m.transcript.live = liveSeg{
		Thinking: "first reasoning line\nsecond reasoning line\nthird reasoning line",
		Text:     "the narration",
		Calls: []ToolCallView{{
			ToolName: "Bash",
			Summary:  "ls",
			Status:   ToolOK,
			Result:   resultLines,
		}},
		active: true,
	}

	thinkingBody := "first reasoning line"                            // full thinking body line (expanded only)
	cappedTailLine := "result-line-" + strconv.Itoa(previewLineCap+2) // a tool line past the cap (never shown)

	capped := func(out string) bool { // tool result is hard-capped: more-marker present, past-cap line absent
		return strings.Contains(out, "more lines") && !strings.Contains(out, cappedTailLine)
	}

	// Default: thinking expanded (full "│ " body); tool output ALREADY hard-capped.
	def := stripANSI(m.renderLiveTail())
	if !strings.Contains(def, thinkingBody) {
		t.Errorf("default live tail missing full thinking body %q; got %q", thinkingBody, def)
	}
	if !capped(def) {
		t.Errorf("default live tail tool result not hard-capped; got %q", def)
	}

	// First ctrl+t: thinking collapses to its compact summary; tool output UNCHANGED (capped).
	m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	col := stripANSI(m.renderLiveTail())
	if !strings.Contains(col, "thinking"+hintSeparator) {
		t.Errorf("collapsed live tail missing the compact thinking summary; got %q", col)
	}
	if strings.Contains(col, thinkingBody) {
		t.Errorf("collapsed live tail still shows the full thinking body; got %q", col)
	}
	if !capped(col) {
		t.Errorf("tool result must stay hard-capped after ctrl+t; got %q", col)
	}

	// Second ctrl+t: thinking expands again; tool output STILL capped.
	m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	exp := stripANSI(m.renderLiveTail())
	if !strings.Contains(exp, thinkingBody) {
		t.Errorf("re-expanded live tail missing full thinking body; got %q", exp)
	}
	if !capped(exp) {
		t.Errorf("tool result must stay hard-capped after second ctrl+t; got %q", exp)
	}
}

// TestLiveTailStaysBoundedAcrossSteps is the architectural guard against the "input block
// repainted twice / stranded" bug. In an inline (non-alt-screen) TUI the live tail is the
// renderer's MANAGED region; if it grows toward the screen height the relative-cursor
// renderer can't track the terminal scroll and strands a copy of the bottom region (input +
// status + prompt). So the live tail MUST stay small regardless of how much a step streams.
// This drives two pathologically large in-progress steps (100-line reasoning + many big tool
// results) and asserts the live tail height stays under a small bound at each — even though
// the UNcapped render would be 500+ lines. (The actual stranding is a bubbletea renderer
// behavior that needs a real TTY to observe; this asserts the invariant that prevents it.)
func TestLiveTailStaysBoundedAcrossSteps(t *testing.T) {
	t.Parallel()

	m := runningScreen(t, &fakeAgent{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 40})

	bigThinking := func() string {
		var b strings.Builder
		for i := 0; i < 100; i++ {
			b.WriteString("a long reasoning line that the model streamed\n")
		}
		return b.String()
	}
	manyBigCalls := func(n int) []ToolCallView {
		calls := make([]ToolCallView, 0, n)
		for i := 0; i < n; i++ {
			out := make([]string, 20)
			for j := range out {
				out[j] = "a line of tool output"
			}
			calls = append(calls, ToolCallView{ToolName: "Bash", Summary: "cmd", Status: ToolOK, Result: out})
		}
		return calls
	}

	// Generous ceiling: thinking cap (~10) + body + "… earlier" + liveCallCap cards ×
	// (header + previewLineCap output) + separators. The uncapped render would be 100 +
	// 20×21 ≈ 520 lines.
	const bound = 35

	height := func() int { return strings.Count(m.renderLiveTail(), "\n") + 1 }

	// Step 1: a huge in-progress step (long reasoning + many big tool results).
	m.transcript.live = liveSeg{Thinking: bigThinking(), Text: "the narration", Calls: manyBigCalls(20), active: true}
	if h := height(); h > bound {
		t.Fatalf("step 1 live tail height = %d, want <= %d (a tall live tail strands the input region)", h, bound)
	}

	// Step 2: the prior step committed (live resets) and a NEW huge step begins. The live
	// tail must not have accumulated across steps and must still be bounded.
	m.transcript.live = liveSeg{Thinking: bigThinking(), Calls: manyBigCalls(15), active: true}
	if h := height(); h > bound {
		t.Fatalf("step 2 live tail height = %d, want <= %d", h, bound)
	}
}

// TestComposeBlinkCmdPlumbed covers the deferred Task-8 item: a printable key in
// compose mode forwards to the editor AND surfaces the textarea's blink Cmd, which
// Screen batches so the composer cursor keeps blinking.
func TestComposeBlinkCmdPlumbed(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Text: "h", Code: 'h'})
	if cmd == nil {
		t.Fatal("typing cmd = nil, want non-nil (blink Cmd batched in)")
	}
	if m.interaction.input.Value() != "h" {
		t.Errorf("composer = %q, want %q", m.interaction.input.Value(), "h")
	}
}

// TestSubmitResultMsg covers the fire-and-forget Submit outcome arm: a nil err is a
// success that records the submit (no committed entry, no cmd — the queued affordance
// only surfaces later on InputQueued); a non-nil err commits a faint, non-fatal error
// entry (no user row was ever committed) and does NOT change the turn status.
func TestSubmitResultMsg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantCmd    bool
		wantErr    bool
		wantStatus Status
	}{
		{name: "nil err is silent", err: nil, wantCmd: false, wantStatus: StatusRunning},
		{name: "err commits a faint error entry", err: errors.New("send failed"), wantCmd: true, wantErr: true, wantStatus: StatusRunning},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := runningScreen(t, agent)

			m, cmd := updateScreen(t, m, submitResultMsg{err: tt.err})
			if (cmd != nil) != tt.wantCmd {
				t.Errorf("cmd != nil = %v, want %v", cmd != nil, tt.wantCmd)
			}
			if m.status != tt.wantStatus {
				t.Errorf("status = %d, want %d (a send failure is non-fatal)", m.status, tt.wantStatus)
			}
			if tt.wantErr {
				rec := lastCommitted(t, m)
				if rec.Kind != kindNotice || rec.Level != noticeError || committedText(rec) != "send failed" {
					t.Errorf("committed = (kind %d, level %d, text %q), want (kindNotice, noticeError, %q)", rec.Kind, rec.Level, committedText(rec), "send failed")
				}
			} else if len(m.transcript.committed) != 0 {
				t.Errorf("committed = %d, want 0 (silent success)", len(m.transcript.committed))
			}
		})
	}
}

// TestSubscribedMsg covers the subscription install arm: on success the stream is
// stored and the continuous reader is armed (a non-nil cmd); on error a fatal error
// entry is committed (the TUI cannot observe the session without a subscription).
func TestSubscribedMsg(t *testing.T) {
	t.Parallel()

	t.Run("success installs sub and arms reader", func(t *testing.T) {
		t.Parallel()
		agent := &fakeAgent{}
		m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
		sub := newFakeSubscription()

		m, cmd := updateScreen(t, m, subscribedMsg{sub: sub})
		if m.sub != sub {
			t.Errorf("sub = %p, want %p (stream not installed)", m.sub, sub)
		}
		if cmd == nil {
			t.Error("cmd = nil, want non-nil (subNext arms the continuous reader)")
		}
		if len(m.transcript.committed) != 0 {
			t.Errorf("committed = %d, want 0 (success commits nothing)", len(m.transcript.committed))
		}
	})

	t.Run("error commits a fatal entry", func(t *testing.T) {
		t.Parallel()
		agent := &fakeAgent{}
		m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})

		m, cmd := updateScreen(t, m, subscribedMsg{err: errors.New("no hub")})
		if m.sub != nil {
			t.Error("sub installed on error, want nil")
		}
		if cmd == nil {
			t.Error("cmd = nil, want non-nil (flush of the error entry)")
		}
		rec := lastCommitted(t, m)
		if rec.Kind != kindNotice || rec.Level != noticeError || committedText(rec) != "no hub" {
			t.Errorf("committed = (kind %d, level %d, text %q), want (kindNotice, noticeError, %q)", rec.Kind, rec.Level, committedText(rec), "no hub")
		}
	})
}

// TestSubClosedMsg covers the continuous reader's terminal: a nil err (intentional
// Close — a /clear swap or quit teardown) is silent (no commit, no cmd); a hub-forced
// loss surfaces an error entry so the user learns the live stream was dropped.
func TestSubClosedMsg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		wantCmd bool
		wantErr bool
	}{
		{name: "intentional close is silent", err: nil, wantCmd: false},
		{
			name:    "hub-forced loss surfaces an error",
			err:     &hub.SubscriptionLossError{DroppedClass: event.Enduring},
			wantCmd: true,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := runningScreen(t, agent)

			m, cmd := updateScreen(t, m, subClosedMsg{err: tt.err})
			if (cmd != nil) != tt.wantCmd {
				t.Errorf("cmd != nil = %v, want %v", cmd != nil, tt.wantCmd)
			}
			if tt.wantErr {
				rec := lastCommitted(t, m)
				if rec.Kind != kindNotice || rec.Level != noticeError {
					t.Errorf("committed = (kind %d, level %d), want (kindNotice, noticeError)", rec.Kind, rec.Level)
				}
			} else if len(m.transcript.committed) != 0 {
				t.Errorf("committed = %d, want 0 (silent close)", len(m.transcript.committed))
			}
		})
	}
}

// TestPrimaryTurnEventsDriveStatus covers the status derivation from turn-lifecycle
// events for the PRIMARY loop: a primary TurnStarted goes Running (and arms the blink
// tick); each primary terminal returns to Idle. The blink-arming cmd is non-nil on
// TurnStarted (subNext + startBlink batched).
func TestPrimaryTurnEventsDriveStatus(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)

	tests := []struct {
		name        string
		ev          event.Event
		startStatus Status
		wantStatus  Status
	}{
		{name: "TurnStarted goes running", ev: event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: primary}}}, startStatus: StatusIdle, wantStatus: StatusRunning},
		{name: "TurnDone goes idle", ev: event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: primary}}}, startStatus: StatusRunning, wantStatus: StatusIdle},
		{name: "TurnFailed goes idle", ev: event.TurnFailed{Header: event.Header{Coordinates: identity.Coordinates{LoopID: primary}}, Err: errors.New("x")}, startStatus: StatusRunning, wantStatus: StatusIdle},
		{name: "TurnInterrupted from interrupting goes idle", ev: event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: primary}}}, startStatus: StatusInterrupting, wantStatus: StatusIdle},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{primaryLoopID: primary}
			m := runningScreen(t, agent)
			m.status = tt.startStatus

			m = feed(t, m, tt.ev)
			if m.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", m.status, tt.wantStatus)
			}
		})
	}
}

// TestSubagentTurnEventsDoNotFlipStatus pins the primary-loop guard: a turn event
// from a SUBAGENT loop (a different Header.LoopID) must NOT change the primary turn
// status — the subagent's output surfaces via Enduring StepDone, not by hijacking the
// primary status line.
func TestSubagentTurnEventsDoNotFlipStatus(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	subagent := callID(0xBB)

	tests := []struct {
		name string
		ev   event.Event
	}{
		{name: "subagent TurnStarted", ev: event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: subagent}}}},
		{name: "subagent TurnDone", ev: event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: subagent}}}},
		{name: "subagent TurnInterrupted", ev: event.TurnInterrupted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: subagent}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{primaryLoopID: primary}
			m := runningScreen(t, agent) // StatusRunning
			m = feed(t, m, tt.ev)
			if m.status != StatusRunning {
				t.Errorf("status = %d, want StatusRunning (a subagent turn event must not flip primary status)", m.status)
			}
		})
	}
}

// TestSubscribesOncePerSession locks the one-session-lifetime-subscription invariant:
// the TUI subscribes EXACTLY ONCE (subscribeCmd, batched into Init) and never
// re-subscribes per turn. The continuous reader re-arms via subNext after each event,
// so feeding many turns' worth of lifecycle events must NOT trigger another Subscribe.
// It is count-based (fakeAgent.subscribeCount) and drives multiple turns so a stray
// per-turn re-subscribe would push the count above 1.
func TestSubscribesOncePerSession(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	sub := newFakeSubscription()
	agent := &fakeAgent{primaryLoopID: primary, subStream: sub}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})

	// Init batches subscribeCmd; the runtime would resolve it to a subscribedMsg.
	// Run the batched command's leaves and deliver the resulting subscribedMsg so the
	// session-lifetime subscription is installed exactly as the runtime would.
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init() = nil, want non-nil (subscribeCmd batched)")
	}
	// subscribeCmd is the only command that performs the Subscribe; invoke it the way
	// the runtime would and feed its message back into Update.
	m, _ = updateScreen(t, m, subscribeCmd(agent)())
	if agent.subscribeCount != 1 {
		t.Fatalf("after Init subscribe = %d, want 1 (one session-lifetime subscription)", agent.subscribeCount)
	}
	if m.sub == nil {
		t.Fatal("subscription not installed after subscribedMsg")
	}

	// Feed several turns' worth of events through Update. Each turn: TurnStarted ->
	// StepDone -> TurnDone for the primary loop. The continuous reader re-arms via
	// subNext on every event; none of this may re-subscribe.
	for turn := 0; turn < 4; turn++ {
		m = feed(t, m, event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: primary}}})
		m = feed(t, m, event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: primary}}})
		m = feed(t, m, event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{LoopID: primary}}})
	}

	if agent.subscribeCount != 1 {
		t.Errorf("subscribe count after %d turns = %d, want exactly 1 (the continuous reader re-arms via subNext, it never re-subscribes per turn)", 4, agent.subscribeCount)
	}
}

func TestUpdateInterruptResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		msg         interruptResultMsg
		startStatus Status
		wantStatus  Status
		wantErr     bool
	}{
		{
			name:        "error sets running and commits error",
			msg:         interruptResultMsg{err: errors.New("x")},
			startStatus: StatusInterrupting,
			wantStatus:  StatusRunning,
			wantErr:     true,
		},
		{
			name:        "success stays interrupting",
			msg:         interruptResultMsg{cancelled: true},
			startStatus: StatusInterrupting,
			wantStatus:  StatusInterrupting,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
			m.status = tt.startStatus

			m, _ = updateScreen(t, m, tt.msg)
			if m.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", m.status, tt.wantStatus)
			}
			if tt.wantErr {
				if rec := lastCommitted(t, m); rec.Kind != kindNotice || rec.Level != noticeError {
					t.Errorf("committed = (kind %d, level %d), want (kindNotice, noticeError)", rec.Kind, rec.Level)
				}
			} else if len(m.transcript.committed) != 0 {
				t.Errorf("committed = %d, want 0", len(m.transcript.committed))
			}
		})
	}
}

func TestUpdateReopenResult(t *testing.T) {
	t.Parallel()

	t.Run("error keeps old agent and goes idle", func(t *testing.T) {
		t.Parallel()

		old := &fakeAgent{}
		m := New(context.Background(), old, fakeOpen(old), AgentBanner{})
		m.status = StatusResetting

		m, _ = updateScreen(t, m, reopenResultMsg{err: errors.New("x")})
		if m.Agent() != old {
			t.Errorf("agent swapped on error, want unchanged")
		}
		if m.status != StatusIdle {
			t.Errorf("status = %d, want StatusIdle", m.status)
		}
		if rec := lastCommitted(t, m); rec.Kind != kindNotice || rec.Level != noticeError {
			t.Errorf("committed = (kind %d, level %d), want (kindNotice, noticeError)", rec.Kind, rec.Level)
		}
	})

	t.Run("success swaps agent resets state closes old and re-subscribes", func(t *testing.T) {
		t.Parallel()

		old := &fakeAgent{}
		fresh := &fakeAgent{}
		m := New(context.Background(), old, fakeOpen(old), AgentBanner{})
		m.status = StatusResetting
		m.transcript = m.transcript.CommitUser([]content.Block{&content.TextBlock{Text: "x"}})
		oldSub := newFakeSubscription()
		m.sub = oldSub

		m, cmd := updateScreen(t, m, reopenResultMsg{agent: fresh})
		if m.Agent() != fresh {
			t.Errorf("agent = %p, want fresh %p", m.Agent(), fresh)
		}
		if len(m.transcript.committed) != 0 {
			t.Errorf("committed = %d, want 0 (reset)", len(m.transcript.committed))
		}
		if m.sub != nil {
			t.Errorf("sub = %p, want nil (old sub dropped; the re-subscribe installs the new one)", m.sub)
		}
		if !oldSub.closed {
			t.Error("old subscription not closed on /clear swap")
		}
		if m.interaction.PendingCount() != 0 {
			t.Errorf("PendingCount = %d, want 0 (prompts cleared)", m.interaction.PendingCount())
		}
		if m.status != StatusIdle {
			t.Errorf("status = %d, want StatusIdle", m.status)
		}
		if cmd == nil {
			t.Fatal("cmd = nil, want non-nil (closeAgent + re-subscribe)")
		}
		// Draining the batch must close the OLD agent and re-subscribe against the FRESH one.
		drainCmd(t, cmd)
		if !old.closeCalled {
			t.Error("old agent Close() not called")
		}
		if fresh.closeCalled {
			t.Error("fresh agent Close() called, want not closed")
		}
		if fresh.subscribeCount != 1 {
			t.Errorf("fresh agent Subscribe() count = %d, want 1; /clear must re-subscribe exactly once to the new agent", fresh.subscribeCount)
		}
		if old.subscribeCount != 0 {
			t.Errorf("old agent Subscribe() count = %d, want 0; the re-subscribe must target the fresh agent", old.subscribeCount)
		}
	})
}

// TestPromptResultMsgNonFatal covers the promptResultMsg handling: a nil err is a
// silent success (no commit, no cmd); a non-nil err commits a faint, non-fatal error
// entry and does NOT panic or hang.
func TestPromptResultMsgNonFatal(t *testing.T) {
	t.Parallel()

	t.Run("nil err is silent", func(t *testing.T) {
		t.Parallel()
		agent := &fakeAgent{}
		m := runningScreen(t, agent)
		m, cmd := updateScreen(t, m, promptResultMsg{err: nil})
		if cmd != nil {
			t.Errorf("cmd = non-nil, want nil (silent success)")
		}
		if len(m.transcript.committed) != 0 {
			t.Errorf("committed = %d, want 0", len(m.transcript.committed))
		}
	})

	t.Run("err commits a faint error entry", func(t *testing.T) {
		t.Parallel()
		agent := &fakeAgent{}
		m := runningScreen(t, agent)
		m, cmd := updateScreen(t, m, promptResultMsg{err: errors.New("dispatch failed")})
		if cmd == nil {
			t.Error("cmd = nil, want non-nil (flush of the error entry)")
		}
		rec := lastCommitted(t, m)
		if rec.Kind != kindNotice || rec.Level != noticeError || committedText(rec) != "dispatch failed" {
			t.Errorf("committed = (kind %d, level %d, text %q), want (kindNotice, noticeError, %q)", rec.Kind, rec.Level, committedText(rec), "dispatch failed")
		}
		// The turn is NOT ended by a prompt-dispatch error.
		if m.status != StatusRunning {
			t.Errorf("status = %d, want StatusRunning (non-fatal)", m.status)
		}
	})
}

// TestStartupBannerWaitsForFirstFrame covers the startup race that placed the banner
// just above the active screen: systemReadyMsg may arrive before Bubble Tea has sent
// the first WindowSizeMsg. In that ordering the banner must be deferred until the
// first sized frame exists. Startup commits are kept unprinted and rendered in the
// active surface until the first real transcript row flushes them into scrollback.
func TestStartupBannerWaitsForFirstFrame(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		first               tea.Msg
		second              tea.Msg
		wantFirstCmd        bool
		wantFirstCommitted  int
		wantSecondCmd       bool
		wantSecondCommitted int
	}{
		{
			name:                "system ready before window size waits",
			first:               systemReadyMsg{},
			second:              tea.WindowSizeMsg{Width: 80, Height: 24},
			wantFirstCmd:        false,
			wantFirstCommitted:  0,
			wantSecondCmd:       false,
			wantSecondCommitted: 1,
		},
		{
			name:                "window size before system ready prints",
			first:               tea.WindowSizeMsg{Width: 80, Height: 24},
			second:              systemReadyMsg{},
			wantFirstCmd:        false,
			wantFirstCommitted:  0,
			wantSecondCmd:       false,
			wantSecondCommitted: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{Name: "SWE"})

			m, cmd := updateScreen(t, m, tt.first)
			if got := cmd != nil; got != tt.wantFirstCmd {
				t.Fatalf("first cmd non-nil = %v, want %v", got, tt.wantFirstCmd)
			}
			if got := len(m.transcript.committed); got != tt.wantFirstCommitted {
				t.Fatalf("first committed entries = %d, want %d", got, tt.wantFirstCommitted)
			}

			m, cmd = updateScreen(t, m, tt.second)
			if got := cmd != nil; got != tt.wantSecondCmd {
				t.Fatalf("second cmd non-nil = %v, want %v", got, tt.wantSecondCmd)
			}
			if got := len(m.transcript.committed); got != tt.wantSecondCommitted {
				t.Fatalf("second committed entries = %d, want %d", got, tt.wantSecondCommitted)
			}
			if got := committedText(m.transcript.committed[0]); got != "SWE" {
				t.Fatalf("startup banner text = %q, want %q", got, "SWE")
			}

			view := stripANSI(m.View().Content)
			if !strings.Contains(view, "SWE") {
				t.Fatalf("View() missing unprinted startup banner; got %q", view)
			}

			m, cmd = updateScreen(t, m, systemReadyMsg{})
			if cmd != nil {
				t.Fatal("duplicate systemReady cmd = non-nil, want nil")
			}
			if got := len(m.transcript.committed); got != 1 {
				t.Fatalf("committed entries after duplicate systemReady = %d, want 1", got)
			}
		})
	}
}

// TestUpdateStartupBanner covers the startup-ready path: after the first terminal
// size is known, systemReadyMsg commits an INFO-level notice carrying the agent name
// + description (NOT the bland "session ready"), threaded in at construction via
// AgentBanner. It also covers the empty-description edge: the banner degrades to the
// bare name. The startup entry is intentionally not flushed yet; while idle it is
// rendered in the active surface, then the first real entry flushes it to scrollback.
func TestUpdateStartupBanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		banner AgentBanner
		want   string
	}{
		{
			name:   "name and description",
			banner: AgentBanner{Name: "coding", Description: "a careful software engineer"},
			want:   "coding — a careful software engineer",
		},
		{
			name:   "empty description degrades to bare name",
			banner: AgentBanner{Name: "coding"},
			want:   "coding",
		},
		{
			name:   "both empty falls back to a neutral marker",
			banner: AgentBanner{},
			want:   "session ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent), tt.banner)
			m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

			m, cmd := updateScreen(t, m, systemReadyMsg{})
			if cmd != nil {
				t.Error("systemReady cmd = non-nil, want nil until a real scrollback entry commits")
			}
			rec := lastCommitted(t, m)
			if rec.Kind != kindNotice || rec.Level != noticeInfo {
				t.Errorf("committed = (kind %d, level %d), want (kindNotice, noticeInfo)", rec.Kind, rec.Level)
			}
			if got := committedText(rec); got != tt.want {
				t.Errorf("banner text = %q, want %q", got, tt.want)
			}
			if got := committedText(rec); strings.Contains(got, "session ready") && tt.want != "session ready" {
				t.Errorf("banner text = %q, must not be the bland session-ready string", got)
			}
		})
	}
}

// TestUpdateStartupGreeting covers the OPTIONAL, UI-only startup greeting (§5a): when
// a non-empty Greeting is threaded in via AgentBanner, systemReadyMsg commits TWO
// opening info notices after the first terminal size is known — the banner first, then
// the greeting — both visible in the active surface until a real transcript entry
// flushes them to scrollback. When the greeting is empty (the default-OFF case) ONLY
// the banner is committed, so behavior is identical to today. The greeting is a pure
// opening transcript entry: it is committed via the same notice path, never a turn or
// command.
func TestUpdateStartupGreeting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		banner       AgentBanner
		wantCommits  int
		wantGreeting string // when wantCommits == 2, the second committed entry's text
	}{
		{
			name:         "greeting on commits banner then greeting",
			banner:       AgentBanner{Name: "SWE", Greeting: "SWE can help with: orchestrator — delegates."},
			wantCommits:  2,
			wantGreeting: "SWE can help with: orchestrator — delegates.",
		},
		{
			name:        "greeting off commits only the banner",
			banner:      AgentBanner{Name: "SWE"},
			wantCommits: 1,
		},
		{
			name:        "whitespace-only greeting is treated as off",
			banner:      AgentBanner{Name: "SWE", Greeting: "   \n\t  "},
			wantCommits: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := &fakeAgent{}
			m := New(context.Background(), agent, fakeOpen(agent), tt.banner)
			m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

			m, cmd := updateScreen(t, m, systemReadyMsg{})
			if cmd != nil {
				t.Fatal("systemReady cmd = non-nil, want nil until a real scrollback entry commits")
			}

			if got := len(m.transcript.committed); got != tt.wantCommits {
				t.Fatalf("committed entries = %d, want %d", got, tt.wantCommits)
			}

			// The banner is always the FIRST opening entry.
			banner := m.transcript.committed[0]
			if banner.Kind != kindNotice || banner.Level != noticeInfo {
				t.Errorf("banner entry = (kind %d, level %d), want (kindNotice, noticeInfo)", banner.Kind, banner.Level)
			}

			if tt.wantCommits == 2 {
				greeting := m.transcript.committed[1]
				if greeting.Kind != kindNotice || greeting.Level != noticeInfo {
					t.Errorf("greeting entry = (kind %d, level %d), want (kindNotice, noticeInfo)", greeting.Kind, greeting.Level)
				}
				if got := committedText(greeting); got != tt.wantGreeting {
					t.Errorf("greeting text = %q, want %q", got, tt.wantGreeting)
				}
			}

			// Lifecycle-neutral: the greeting is NOT a turn or a command. It must never
			// submit anything and must not flip the model off Idle.
			if agent.submitCalled {
				t.Error("greeting must not call agent.Submit (it is not a turn/command)")
			}
			if m.status != StatusIdle {
				t.Errorf("status after greeting = %d, want StatusIdle (greeting is lifecycle-neutral)", m.status)
			}
		})
	}
}

func TestStartupBannerFlushesWithFirstTranscriptEntry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
	}{
		{name: "first user row carries startup banner into scrollback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			primary := callID(0xAA)
			agent := &fakeAgent{primaryLoopID: primary}
			m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{Name: "SWE"})
			m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
			m, cmd := updateScreen(t, m, systemReadyMsg{})
			if cmd != nil {
				t.Fatal("systemReady cmd = non-nil, want nil")
			}

			startup := m.transcript.committed[0]
			if m.scrollback.printed[startup.ID] {
				t.Fatal("startup banner printed before the first real transcript entry")
			}
			if view := stripANSI(m.View().Content); !strings.Contains(view, "SWE") {
				t.Fatalf("View() missing unprinted startup banner; got %q", view)
			}

			m, cmd = updateScreen(t, m, eventMsg{ev: event.TurnStarted{
				Header:  event.Header{Coordinates: identity.Coordinates{LoopID: primary}, Cause: identity.Cause{CommandID: fixedFakeSubmitID}},
				Message: userMsg("hello"),
			}})
			if cmd == nil {
				t.Fatal("first transcript event cmd = nil, want flush")
			}

			if got := len(m.transcript.committed); got != 2 {
				t.Fatalf("committed entries = %d, want startup + user", got)
			}
			if !m.scrollback.printed[startup.ID] {
				t.Fatal("startup banner was not flushed with the first real transcript entry")
			}
			user := m.transcript.committed[1]
			if user.Kind != kindUser || committedText(user) != "hello" {
				t.Fatalf("second committed = (kind %d, text %q), want user %q", user.Kind, committedText(user), "hello")
			}
			if !m.scrollback.printed[user.ID] {
				t.Fatal("first user row was not flushed to scrollback")
			}
			if view := stripANSI(m.View().Content); strings.Contains(view, "SWE") {
				t.Fatalf("View() still shows startup banner after it was flushed; got %q", view)
			}
		})
	}
}

// TestSubmitUserRowFromEventNotSubmit is the Screen-level proof of the event-driven
// user row: after a submit there is NO kindUser row (the optimistic commit is gone);
// feeding the loop's TurnStarted (genuine input, Message present) commits exactly one
// kindUser row equal to the event's Message blocks, and flushes it to scrollback.
func TestSubmitUserRowFromEventNotSubmit(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	agent := &fakeAgent{primaryLoopID: primary}
	m := runningScreen(t, agent)
	m.interaction.input.SetValue("from the event")

	// Submit: fires submitCmd, commits NO row.
	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	drainCmd(t, cmd)
	if got := len(m.transcript.committed); got != 0 {
		t.Fatalf("committed after submit = %d, want 0 (no optimistic user row)", got)
	}

	// The loop's TurnStarted carries the authoritative user message (genuine input:
	// Cause.LoopID == 0 AND Header.LoopID == the agent's primary loop id, which
	// New threaded into the transcript).
	m = feed(t, m, event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: primary}, Cause: identity.Cause{CommandID: fixedFakeSubmitID}}, Message: userMsg("from the event")})

	rec := lastCommitted(t, m)
	if rec.Kind != kindUser || committedText(rec) != "from the event" {
		t.Fatalf("committed = (kind %d, text %q), want (kindUser, %q)", rec.Kind, committedText(rec), "from the event")
	}
	if got := userRowCount(m); got != 1 {
		t.Errorf("kindUser rows = %d, want exactly 1 (one TurnStarted -> one row)", got)
	}
	if !m.scrollback.printed[rec.ID] {
		t.Error("event-committed user row not flushed to scrollback")
	}
}

// TestFirstUserRowSurvivesEmptyRestore is the regression test for the startup
// displayID collision that silently dropped the FIRST user message from the TUI.
//
// At startup commitStartup commits the banner (an opening UI-only notice) as transcript
// displayID 1. While idle it may still be active-surface-only; after any early flush it
// may already be recorded by the scrollback print-once engine. The background restore
// fold then lands: for a NEW session it folds to an EMPTY backlog. The bug was that
// handleRestored installed that empty transcript WHOLESALE — discarding the banner and
// resetting the displayID counter to 0. If the banner had already printed, the first
// user row reused displayID 1, which scrollback.Flush treated as "already printed" and
// skipped; if the banner had not printed, the visible startup marker disappeared before
// it could hand off into scrollback. This test reproduces the empty-restore ordering
// and asserts the first user row reaches scrollback with a displayID that was NOT
// already printed.
func TestFirstUserRowSurvivesEmptyRestore(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	agent := &fakeAgent{primaryLoopID: primary}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{Name: "SWE"})

	// Real startup ordering: size, then the banner commit (systemReady), then the
	// background restore fold landing for a NEW session (empty backlog).
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	m, cmd := updateScreen(t, m, systemReadyMsg{})
	drainCmd(t, cmd)
	if got := userRowCount(m); got != 0 {
		t.Fatalf("user rows after banner = %d, want 0", got)
	}

	// The new-session restore fold: an empty backlog (FoldDisplay over no events).
	m, _ = updateScreen(t, m, restoredMsg{transcript: transcriptModel{primaryLoopID: primary}, interaction: newInteractionModel()})

	// Snapshot the displayIDs already emitted to scrollback BEFORE the first user turn.
	// The first user row must not reuse any of them, or the print-once engine drops it.
	alreadyPrinted := make(map[displayID]bool, len(m.scrollback.printed))
	for id := range m.scrollback.printed {
		alreadyPrinted[id] = true
	}

	// The loop's TurnStarted for the genuine first user message (Cause.LoopID == 0,
	// Header.LoopID == primary): commits the authoritative user row and flushes it.
	m = feed(t, m, event.TurnStarted{
		Header:  event.Header{Coordinates: identity.Coordinates{LoopID: primary}, Cause: identity.Cause{CommandID: fixedFakeSubmitID}},
		Message: userMsg("Hello"),
	})

	rec := lastCommitted(t, m)
	if rec.Kind != kindUser || committedText(rec) != "Hello" {
		t.Fatalf("last committed = (kind %d, text %q), want (kindUser, %q)", rec.Kind, committedText(rec), "Hello")
	}
	if alreadyPrinted[rec.ID] {
		t.Fatalf("first user row reused displayID %d already printed by the banner — the print-once engine skips it, so the first user message never reaches scrollback", rec.ID)
	}
	if !m.scrollback.printed[rec.ID] {
		t.Error("first user row was not flushed to scrollback")
	}
}

// TestSubagentHandbackCommitsNoUserRow is the Screen-level proof that a SubagentResult
// hand-back (TurnStarted/TurnFoldedInto with a non-zero Cause.LoopID) commits NO
// user row — only genuine user input (Cause.LoopID == 0) gets a row.
func TestSubagentHandbackCommitsNoUserRow(t *testing.T) {
	t.Parallel()

	subagent := callID(0xBB)

	tests := []struct {
		name string
		ev   event.Event
	}{
		{name: "TurnStarted hand-back", ev: event.TurnStarted{Header: event.Header{Cause: identity.Cause{CommandID: callID(1), Coordinates: identity.Coordinates{LoopID: subagent}}}, Message: userMsg("handback")}},
		{name: "TurnFoldedInto hand-back", ev: event.TurnFoldedInto{Header: event.Header{Cause: identity.Cause{CommandID: callID(1), Coordinates: identity.Coordinates{LoopID: subagent}}}, Message: userMsg("handback")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := &fakeAgent{}
			m := runningScreen(t, agent)
			m = feed(t, m, tt.ev)
			if got := userRowCount(m); got != 0 {
				t.Errorf("kindUser rows = %d, want 0 (a subagent hand-back commits no user row)", got)
			}
		})
	}
}

// TestSubagentOwnTurnCommitsNoUserRow is the Screen-level proof of the loop-scoping
// fix: a SUBAGENT loop's OWN initial task arrives as an untriggered TurnStarted /
// TurnFoldedInto (Cause.LoopID == 0) carrying a Message, but with
// Header.LoopID == the subagent loop (NOT the primary). The DefaultEventFilter
// delivers it (Enduring from every loop), so it reaches ApplyEvent — but it must NOT
// commit a human user row (it surfaces only via the collapsed StepDone, §5/§6). New
// threaded the agent's primary loop id into the transcript, so a non-matching LoopID
// is rejected even though Cause.LoopID is zero.
func TestSubagentOwnTurnCommitsNoUserRow(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	subLoop := callID(0xCC) // a different (subagent) loop id

	tests := []struct {
		name string
		ev   event.Event
	}{
		{name: "subagent TurnStarted (own initial task)", ev: event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: subLoop}, Cause: identity.Cause{CommandID: callID(1)}}, Message: userMsg("subagent task")}},
		{name: "subagent TurnFoldedInto", ev: event.TurnFoldedInto{Header: event.Header{Coordinates: identity.Coordinates{LoopID: subLoop}, Cause: identity.Cause{CommandID: callID(1)}}, Message: userMsg("subagent fold")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := &fakeAgent{primaryLoopID: primary}
			m := runningScreen(t, agent)
			m = feed(t, m, tt.ev)
			if got := userRowCount(m); got != 0 {
				t.Errorf("kindUser rows = %d, want 0 (a subagent's own turn must not commit a user row)", got)
			}
		})
	}
}

// TestSubmitQueuedAffordancePromotes is the Screen-level lifecycle: a successful
// submitResultMsg records the submit, the loop's InputQueued reveals the dim
// affordance in the View, and the loop's TurnStarted promotes it to a committed user
// row (the affordance gone from the View, the row in scrollback).
func TestSubmitQueuedAffordancePromotes(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 60, Height: 24})

	id := callID(0x42)
	// A successful submit result records the submit (no row, no affordance yet).
	m, _ = updateScreen(t, m, submitResultMsg{inputID: id, blocks: userBlocks("pending msg")})
	if strings.Contains(stripANSI(m.View().Content), "pending msg") {
		t.Fatal("affordance shown before InputQueued; it must wait for the event")
	}

	// InputQueued reveals the affordance below the live tail (above the composer panel).
	m = feed(t, m, event.InputQueued{Header: event.Header{Cause: identity.Cause{CommandID: id}}})
	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "pending msg") {
		t.Fatalf("queued affordance missing from View after InputQueued; got %q", view)
	}

	// TurnStarted promotes to a committed row and clears the affordance.
	m = feed(t, m, event.TurnStarted{Header: event.Header{Cause: identity.Cause{CommandID: id}}, Message: userMsg("pending msg")})
	if got := userRowCount(m); got != 1 {
		t.Errorf("kindUser rows = %d, want 1 (promoted once)", got)
	}
	if strings.Contains(stripANSI(m.View().Content), "pending msg") {
		t.Error("queued affordance still in View after TurnStarted; it must be cleared (the row is in scrollback)")
	}
}

// TestTurnRejectedSurfacesNotice is the Screen-level proof that a rejected submit is
// not silent: after recording a submit and revealing the affordance, a TurnRejected
// drops the affordance and commits an error notice mentioning the reason.
func TestTurnRejectedSurfacesNotice(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 60, Height: 24})

	id := callID(0x66)
	m, _ = updateScreen(t, m, submitResultMsg{inputID: id, blocks: userBlocks("rejected msg")})
	m = feed(t, m, event.InputQueued{Header: event.Header{Cause: identity.Cause{CommandID: id}}})

	m = feed(t, m, event.TurnRejected{Header: event.Header{Cause: identity.Cause{CommandID: id}}, Reason: event.RejectQueueFull})

	rec := lastCommitted(t, m)
	if rec.Kind != kindNotice || rec.Level != noticeError {
		t.Fatalf("committed = (kind %d, level %d), want (kindNotice, noticeError)", rec.Kind, rec.Level)
	}
	if got := committedText(rec); !strings.Contains(got, "queue full") {
		t.Errorf("rejection notice = %q, want it to mention the reason %q", got, "queue full")
	}
	if got := userRowCount(m); got != 0 {
		t.Errorf("kindUser rows = %d, want 0 (a rejected message surfaces as a notice, not a row)", got)
	}
}

// userRowCount counts the committed kindUser rows in m's transcript.
func userRowCount(m Screen) int {
	n := 0
	for _, e := range m.transcript.committed {
		if e.Kind == kindUser {
			n++
		}
	}
	return n
}

// TestFlushPrintsEachEntryOnce covers the print-once invariant at the Screen level:
// flushing twice over the same committed slice prints each entry exactly once (the
// second flush yields no new print actions).
func TestFlushPrintsEachEntryOnce(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m.transcript = m.transcript.CommitUser([]content.Block{&content.TextBlock{Text: "one"}})

	first := m.flush()
	if first == nil {
		t.Fatal("first flush cmd = nil, want non-nil (the new entry)")
	}
	second := m.flush()
	if second != nil {
		t.Error("second flush cmd = non-nil, want nil (entry already printed once)")
	}
}
