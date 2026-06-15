package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/inventivepotter/urvi/tui/components"
)

// compile-time assertion that the test double satisfies the (widened) Agent
// interface; if a method is added or its signature drifts, this fails to build.
var _ Agent = (*fakeAgent)(nil)

// fakeAgent is a scriptable Agent test double. It records calls and returns the
// configured reader/error/bool so Screen behavior can be exercised without a real
// session.
type fakeAgent struct {
	streamReader *llm.StreamReader[event.Event]
	streamErr    error

	interruptCancelled bool
	interruptErr       error

	closeCalled  bool
	closeErr     error
	acceptsImage bool

	// gate-trio recorders: the configured error is returned, and the last call's
	// arguments are captured so a test can assert the wrapper forwarded them.
	approveErr    error
	denyErr       error
	answerErr     error
	approveCalled bool
	denyCalled    bool
	answerCalled  bool
	lastCallID    uuid.UUID
	lastScope     tool.ApprovalScope
	lastAnswer    string
}

func (f *fakeAgent) StreamBlocks(_ context.Context, _ []content.Block) (*llm.StreamReader[event.Event], error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return f.streamReader, nil
}

func (f *fakeAgent) Interrupt(_ context.Context) (bool, error) {
	return f.interruptCancelled, f.interruptErr
}

func (f *fakeAgent) Close(_ context.Context) error {
	f.closeCalled = true
	return f.closeErr
}

func (f *fakeAgent) AcceptsImages() bool { return f.acceptsImage }

func (f *fakeAgent) Approve(_ context.Context, callID uuid.UUID, scope tool.ApprovalScope) error {
	f.approveCalled = true
	f.lastCallID = callID
	f.lastScope = scope
	return f.approveErr
}

func (f *fakeAgent) Deny(_ context.Context, callID uuid.UUID) error {
	f.denyCalled = true
	f.lastCallID = callID
	return f.denyErr
}

func (f *fakeAgent) ProvideAnswer(_ context.Context, callID uuid.UUID, answer string) error {
	f.answerCalled = true
	f.lastCallID = callID
	f.lastAnswer = answer
	return f.answerErr
}

// scriptedReader builds a StreamReader that yields the given events in order,
// then io.EOF on every subsequent call.
func scriptedReader(evs ...event.Event) *llm.StreamReader[event.Event] {
	i := 0
	next := func() (event.Event, error) {
		if i >= len(evs) {
			return nil, io.EOF
		}
		ev := evs[i]
		i++
		return ev, nil
	}
	return llm.NewStreamReader(next, nil)
}

// fakeOpen returns an OpenAgent thunk that yields the given agent.
func fakeOpen(a Agent) OpenAgent {
	return func(context.Context) (Agent, error) { return a, nil }
}

// callID returns a deterministic non-zero UUID for a test, distinguishing gates by
// a single byte so CallID correlation can be asserted.
func callID(b byte) uuid.UUID {
	var u uuid.UUID
	u[0] = b
	return u
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

// runningScreen returns a fresh Screen wired for a live turn: a non-nil reader
// (readNext targets must be non-nil) and StatusRunning.
func runningScreen(t *testing.T, agent Agent) Screen {
	t.Helper()
	m := New(context.Background(), agent, fakeOpen(agent))
	m.reader = scriptedReader()
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
	m := New(context.Background(), agent, fakeOpen(agent))

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
	m := New(context.Background(), agent, fakeOpen(agent))

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
	var _ tea.Model = New(context.Background(), agent, fakeOpen(agent))
}

func TestWindowSizeMsg(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))

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
			m := New(context.Background(), agent, fakeOpen(agent))
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

// TestViewComposesSurfaceNoViewport asserts the View is the active surface (a
// separator rule + a bottom box), never a transcript viewport, and that committed
// entries are NOT re-rendered into the View (they live in native scrollback). A
// committed user line's text must not appear in the View.
func TestViewComposesSurfaceNoViewport(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 60, Height: 18})
	m.transcript = m.transcript.CommitUser([]content.Block{&content.TextBlock{Text: "committed-history-line"}})

	view := stripANSI(m.View().Content)
	if !strings.Contains(view, "─") {
		t.Errorf("View() missing the separator rule; got %q", view)
	}
	if strings.Contains(view, "committed-history-line") {
		t.Errorf("View() re-rendered a committed entry; it belongs in native scrollback, got %q", view)
	}
}

// TestViewRendersLiveTail asserts the in-progress (live) assistant segment renders
// in the View's capped live tail above the separator — it is NOT yet committed, so
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

// TestEventRoutesToBothReducers is the core router invariant: a single stream event
// reaches BOTH the transcript reducer (which accumulates the live segment) AND the
// interaction reducer, and readNext keeps draining (a non-nil cmd is returned so the
// next event is pulled even though the loop may be gated).
func TestEventRoutesToBothReducers(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)

	m, cmd := updateScreen(t, m, eventMsg{ev: event.TokenDelta{Chunk: &content.TextChunk{Text: "hello"}}})

	if m.transcript.live.Text != "hello" {
		t.Errorf("transcript live.Text = %q, want %q (event not routed to transcript)", m.transcript.live.Text, "hello")
	}
	if cmd == nil {
		t.Error("event cmd = nil, want non-nil (readNext keeps draining the stream)")
	}
}

// TestPermissionRequestedEnqueuesAndCommits covers the freeze fix's first half: a
// PermissionRequested both ENQUEUES a prompt in the interaction model (so the
// bottom box becomes the permission control) AND commits a promptRecord to the
// transcript that flushes to scrollback. The stream keeps draining.
func TestPermissionRequestedEnqueuesAndCommits(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)

	req := tool.BashRequest{Command: "rm -rf /tmp/x"}
	m, cmd := updateScreen(t, m, eventMsg{ev: event.PermissionRequested{CallID: callID(1), Request: req}})

	// Interaction: one pending prompt, mode switched to permission.
	if m.interaction.PendingCount() != 1 {
		t.Fatalf("PendingCount = %d, want 1 (prompt not enqueued)", m.interaction.PendingCount())
	}
	if m.interaction.mode != modePermissionPrompt {
		t.Errorf("interaction mode = %d, want modePermissionPrompt", m.interaction.mode)
	}
	// Transcript: a kindPromptRecord committed with the request payload.
	rec := lastCommitted(t, m)
	if rec.Kind != kindPromptRecord || rec.Prompt == nil {
		t.Fatalf("last committed = %+v, want a kindPromptRecord with a Prompt context", rec)
	}
	if rec.Prompt.ToolName != "Bash" {
		t.Errorf("prompt record ToolName = %q, want %q", rec.Prompt.ToolName, "Bash")
	}
	// The record flushed to scrollback exactly once (print-once map records its ID).
	if !m.scrollback.printed[rec.ID] {
		t.Error("prompt record not flushed to scrollback")
	}
	// The stream keeps draining (the gate, not the stream, blocks the loop).
	if cmd == nil {
		t.Error("PermissionRequested cmd = nil, want non-nil (readNext keeps draining)")
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
			m = feed(t, m, event.PermissionRequested{CallID: callID(7), Request: tool.BashRequest{Command: "ls"}})

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
				t.Errorf("dispatched CallID = %v, want %v", agent.lastCallID, callID(7))
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
	// A free-text AskUser (no choices) enters answer mode.
	m = feed(t, m, event.UserInputRequested{CallID: callID(3), Question: "name?"})
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
}

// TestChoiceEscInterrupts covers the Esc precedence in choice mode: Esc interrupts
// the turn (uiInterrupt → interruptTurn) WITHOUT popping the prompt; the model
// flips to Interrupting.
func TestChoiceEscInterrupts(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m = feed(t, m, event.UserInputRequested{CallID: callID(4), Question: "pick", Choices: []string{"a", "b"}})
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
			m = feed(t, m, event.PermissionRequested{CallID: callID(1), Request: tool.BashRequest{Command: "x"}})
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

// TestSubmitStartsTurnIdle covers uiSubmit at Idle: a user entry is committed and
// flushed to scrollback, the turn starts (StatusRunning), and the agent's reader is
// installed.
func TestSubmitStartsTurnIdle(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{streamReader: scriptedReader(event.TurnStarted{})}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.interaction.input.SetValue("hello there")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("submit cmd = nil, want non-nil (flush + readNext)")
	}
	if m.status != StatusRunning {
		t.Errorf("status = %d, want StatusRunning", m.status)
	}
	if m.reader == nil {
		t.Error("reader = nil after a successful start")
	}
	rec := lastCommitted(t, m)
	if rec.Kind != kindUser || committedText(rec) != "hello there" {
		t.Errorf("committed entry = (kind %d, text %q), want (kindUser, %q)", rec.Kind, committedText(rec), "hello there")
	}
	if !m.scrollback.printed[rec.ID] {
		t.Error("user entry not flushed to scrollback")
	}
	if m.interaction.input.Value() != "" {
		t.Errorf("composer = %q, want reset", m.interaction.input.Value())
	}
}

// TestSubmitQueuesWhileRunning covers uiSubmit at Running: the user entry is
// committed (lands in scrollback) and the blocks are queued; no new turn starts and
// the composer resets.
func TestSubmitQueuesWhileRunning(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)
	m.interaction.input.SetValue("queued one")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if cmd == nil {
		t.Error("queue submit cmd = nil, want non-nil (flush of the committed user entry)")
	}
	if len(m.queue) != 1 {
		t.Fatalf("queue len = %d, want 1", len(m.queue))
	}
	rec := lastCommitted(t, m)
	if rec.Kind != kindUser || committedText(rec) != "queued one" {
		t.Errorf("committed = (kind %d, text %q), want (kindUser, %q)", rec.Kind, committedText(rec), "queued one")
	}
	if m.status != StatusRunning {
		t.Errorf("status = %d, want StatusRunning (unchanged)", m.status)
	}
	if m.interaction.input.Value() != "" {
		t.Errorf("composer = %q, want reset", m.interaction.input.Value())
	}
}

// TestSubmitBadAttachmentCommitsError covers uiSubmit with a buildBlocks failure:
// no turn starts, an error entry is committed, and the agent is untouched. (The
// composer was already reset by the interaction model on Enter.)
func TestSubmitBadAttachmentCommitsError(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
	m.interaction.input.SetValue("@nope.pem") // .pem is a denied extension → buildBlocks error

	m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.status != StatusIdle {
		t.Errorf("status = %d, want StatusIdle (no turn)", m.status)
	}
	rec := lastCommitted(t, m)
	if rec.Kind != kindError {
		t.Errorf("committed kind = %d, want kindError", rec.Kind)
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
	m := New(context.Background(), agent, fakeOpen(agent))
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
	m := New(context.Background(), agent, fakeOpen(agent))
	m.interaction.input.SetValue("/help")

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Error("/help cmd = nil, want non-nil (flush of the system entry)")
	}
	rec := lastCommitted(t, m)
	if rec.Kind != kindSystem {
		t.Fatalf("committed kind = %d, want kindSystem", rec.Kind)
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
	m := New(context.Background(), agent, fakeOpen(agent))
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
// active prompt interrupts a running turn and clears the queue.
func TestEscWhileRunningInterrupts(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{interruptCancelled: true}
	m := runningScreen(t, agent)
	m.queue = [][]content.Block{{&content.TextBlock{Text: "queued"}}}

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Error("Esc cmd = nil, want non-nil (interruptTurn)")
	}
	if m.status != StatusInterrupting {
		t.Errorf("status = %d, want StatusInterrupting", m.status)
	}
	if len(m.queue) != 0 {
		t.Errorf("queue len = %d, want 0 (cleared on interrupt)", len(m.queue))
	}
}

// TestEscWhileIdleIsNoop covers Esc with no prompt and no turn: a no-op.
func TestEscWhileIdleIsNoop(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))

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
				m = feed(t, m, event.PermissionRequested{CallID: callID(1), Request: tool.BashRequest{Command: "x"}})
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
// touching the turn or the prompt queue.
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
			m := New(context.Background(), agent, fakeOpen(agent))
			m.status = tt.status
			if tt.status != StatusIdle {
				m.reader = scriptedReader()
			}
			if tt.withPrompt {
				m = feed(t, m, event.PermissionRequested{CallID: callID(1), Request: tool.BashRequest{Command: "x"}})
			}
			wantPending := m.interaction.PendingCount()

			m, cmd := updateScreen(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
			if cmd != nil {
				t.Errorf("ctrl+t cmd = non-nil, want nil (re-render only)")
			}
			if !m.expand {
				t.Errorf("expand = false after ctrl+t, want true")
			}
			if m.status != tt.status {
				t.Errorf("status = %d, want unchanged %d", m.status, tt.status)
			}
			if m.interaction.PendingCount() != wantPending {
				t.Errorf("PendingCount changed by ctrl+t: %d, want %d", m.interaction.PendingCount(), wantPending)
			}
			// Second toggle flips back.
			m, _ = updateScreen(t, m, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
			if m.expand {
				t.Errorf("expand = true after second ctrl+t, want false")
			}
		})
	}
}

// TestComposeBlinkCmdPlumbed covers the deferred Task-8 item: a printable key in
// compose mode forwards to the editor AND surfaces the textarea's blink Cmd, which
// Screen batches so the composer cursor keeps blinking.
func TestComposeBlinkCmdPlumbed(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))

	m, cmd := updateScreen(t, m, tea.KeyPressMsg{Text: "h", Code: 'h'})
	if cmd == nil {
		t.Fatal("typing cmd = nil, want non-nil (blink Cmd batched in)")
	}
	if m.interaction.input.Value() != "h" {
		t.Errorf("composer = %q, want %q", m.interaction.input.Value(), "h")
	}
}

func TestStartTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		agent      *fakeAgent
		wantCmd    bool
		wantOK     bool
		wantStatus Status
		wantReader bool
		wantErr    bool
	}{
		{
			name:       "success returns cmd and running",
			agent:      &fakeAgent{streamReader: scriptedReader(event.TurnStarted{})},
			wantCmd:    true,
			wantOK:     true,
			wantStatus: StatusRunning,
			wantReader: true,
		},
		{
			name:       "failure commits error and stays idle",
			agent:      &fakeAgent{streamErr: errors.New("boom")},
			wantStatus: StatusIdle,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := New(context.Background(), tt.agent, fakeOpen(tt.agent))
			cmd, ok := m.startTurn([]content.Block{&content.TextBlock{Text: "hi"}})

			if (cmd != nil) != tt.wantCmd {
				t.Errorf("startTurn cmd != nil = %v, want %v", cmd != nil, tt.wantCmd)
			}
			if ok != tt.wantOK {
				t.Errorf("startTurn ok = %v, want %v", ok, tt.wantOK)
			}
			if m.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", m.status, tt.wantStatus)
			}
			if (m.reader != nil) != tt.wantReader {
				t.Errorf("reader != nil = %v, want %v", m.reader != nil, tt.wantReader)
			}
			if tt.wantErr {
				rec := lastCommitted(t, m)
				if rec.Kind != kindError {
					t.Errorf("committed kind = %d, want kindError", rec.Kind)
				}
			} else if len(m.transcript.committed) != 0 {
				t.Errorf("committed = %d, want 0", len(m.transcript.committed))
			}
		})
	}
}

// TestStreamEOFAdvancesQueue covers the queue advance on EOF: an empty queue goes
// Idle; a non-empty queue starts the next turn (its user entry already printed at
// submit time); a failed restart keeps the head queued and stays Idle with an error.
func TestStreamEOFAdvancesQueue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		queue      [][]content.Block
		streamErr  error
		wantStatus Status
		wantCmd    bool
		wantQueue  int
		wantErr    bool
	}{
		{name: "empty queue goes idle", wantStatus: StatusIdle, wantQueue: 0},
		{
			name:       "non-empty queue starts next turn",
			queue:      [][]content.Block{{&content.TextBlock{Text: "q"}}},
			wantStatus: StatusRunning,
			wantCmd:    true,
			wantQueue:  0,
		},
		{
			name:       "restart failure keeps head and goes idle with error",
			queue:      [][]content.Block{{&content.TextBlock{Text: "q"}}},
			streamErr:  errors.New("busy"),
			wantStatus: StatusIdle,
			wantCmd:    true, // a flush of the committed error entry
			wantQueue:  1,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{streamReader: scriptedReader(event.TurnStarted{}), streamErr: tt.streamErr}
			m := runningScreen(t, agent)
			m.queue = tt.queue

			m, cmd := updateScreen(t, m, streamEOFMsg{})

			if m.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", m.status, tt.wantStatus)
			}
			if (cmd != nil) != tt.wantCmd {
				t.Errorf("cmd != nil = %v, want %v", cmd != nil, tt.wantCmd)
			}
			if len(m.queue) != tt.wantQueue {
				t.Errorf("queue len = %d, want %d", len(m.queue), tt.wantQueue)
			}
			if m.reader != nil && tt.wantStatus == StatusIdle {
				t.Error("reader != nil after idle EOF, want nil")
			}
			if tt.wantErr {
				rec := lastCommitted(t, m)
				if rec.Kind != kindError {
					t.Errorf("last committed kind = %d, want kindError", rec.Kind)
				}
			}
		})
	}
}

// TestStreamErrCommitsFailureAndAdvances covers a non-EOF stream read error: it
// commits a terminal failure (via the transcript's TurnFailed path), goes Idle, and
// closes the reader.
func TestStreamErrCommitsFailureAndAdvances(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := runningScreen(t, agent)

	m, _ = updateScreen(t, m, streamErrMsg{err: errors.New("read fail")})
	if m.status != StatusIdle {
		t.Errorf("status = %d, want StatusIdle", m.status)
	}
	if m.reader != nil {
		t.Error("reader != nil, want nil")
	}
	rec := lastCommitted(t, m)
	if rec.Kind != kindError || committedText(rec) != "read fail" {
		t.Errorf("last committed = (kind %d, text %q), want (kindError, %q)", rec.Kind, committedText(rec), "read fail")
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
			m := New(context.Background(), agent, fakeOpen(agent))
			m.status = tt.startStatus

			m, _ = updateScreen(t, m, tt.msg)
			if m.status != tt.wantStatus {
				t.Errorf("status = %d, want %d", m.status, tt.wantStatus)
			}
			if tt.wantErr {
				if rec := lastCommitted(t, m); rec.Kind != kindError {
					t.Errorf("committed kind = %d, want kindError", rec.Kind)
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
		m := New(context.Background(), old, fakeOpen(old))
		m.status = StatusResetting

		m, _ = updateScreen(t, m, reopenResultMsg{err: errors.New("x")})
		if m.Agent() != old {
			t.Errorf("agent swapped on error, want unchanged")
		}
		if m.status != StatusIdle {
			t.Errorf("status = %d, want StatusIdle", m.status)
		}
		if rec := lastCommitted(t, m); rec.Kind != kindError {
			t.Errorf("committed kind = %d, want kindError", rec.Kind)
		}
	})

	t.Run("success swaps agent resets state and closes old", func(t *testing.T) {
		t.Parallel()

		old := &fakeAgent{}
		fresh := &fakeAgent{}
		m := New(context.Background(), old, fakeOpen(old))
		m.status = StatusResetting
		m.transcript = m.transcript.CommitUser([]content.Block{&content.TextBlock{Text: "x"}})
		m.queue = [][]content.Block{{&content.TextBlock{Text: "q"}}}

		m, cmd := updateScreen(t, m, reopenResultMsg{agent: fresh})
		if m.Agent() != fresh {
			t.Errorf("agent = %p, want fresh %p", m.Agent(), fresh)
		}
		if len(m.transcript.committed) != 0 {
			t.Errorf("committed = %d, want 0 (reset)", len(m.transcript.committed))
		}
		if len(m.queue) != 0 {
			t.Errorf("queue len = %d, want 0", len(m.queue))
		}
		if m.interaction.PendingCount() != 0 {
			t.Errorf("PendingCount = %d, want 0 (prompts cleared)", m.interaction.PendingCount())
		}
		if m.status != StatusIdle {
			t.Errorf("status = %d, want StatusIdle", m.status)
		}
		if cmd == nil {
			t.Fatal("cmd = nil, want non-nil (closeAgent for old)")
		}
		cmd()
		if !old.closeCalled {
			t.Error("old agent Close() not called")
		}
		if fresh.closeCalled {
			t.Error("fresh agent Close() called, want not closed")
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
		if rec.Kind != kindError || committedText(rec) != "dispatch failed" {
			t.Errorf("committed = (kind %d, text %q), want (kindError, %q)", rec.Kind, committedText(rec), "dispatch failed")
		}
		// The turn is NOT ended by a prompt-dispatch error.
		if m.status != StatusRunning {
			t.Errorf("status = %d, want StatusRunning (non-fatal)", m.status)
		}
	})
}

func TestUpdateSystemReady(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))

	m, cmd := updateScreen(t, m, systemReadyMsg{})
	if cmd == nil {
		t.Error("systemReady cmd = nil, want non-nil (flush)")
	}
	rec := lastCommitted(t, m)
	if rec.Kind != kindSystem || committedText(rec) != "session ready" {
		t.Errorf("committed = (kind %d, text %q), want (kindSystem, %q)", rec.Kind, committedText(rec), "session ready")
	}
}

// TestFlushPrintsEachEntryOnce covers the print-once invariant at the Screen level:
// flushing twice over the same committed slice prints each entry exactly once (the
// second flush yields no new print actions).
func TestFlushPrintsEachEntryOnce(t *testing.T) {
	t.Parallel()

	agent := &fakeAgent{}
	m := New(context.Background(), agent, fakeOpen(agent))
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
