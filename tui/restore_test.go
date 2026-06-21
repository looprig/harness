package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// foldBacklog folds backlog through the SAME pure reducers the background fold uses,
// from the same zero state restoreBacklogCmd starts at, so a test can assert the
// command's reducer state matches a direct, per-event fold. It mirrors the production
// fold exactly (transcript.ApplyEvent + interaction.ApplyEvent), which is the point:
// the repaint is correct iff the background fold equals this fold.
func foldBacklog(primary uuid.UUID, backlog []event.Event) (transcriptModel, interactionModel) {
	tr := transcriptModel{primaryLoopID: primary}
	in := newInteractionModel()
	for _, ev := range backlog {
		tr = tr.ApplyEvent(ev)
		in = in.ApplyEvent(ev)
	}
	return tr, in
}

// runRestoreCmd executes restoreBacklogCmd off the update loop the way the runtime
// would, returning the single restoredMsg it produces. It fails the test if the
// command yields any other message type — the fold must surface exactly one result.
func runRestoreCmd(t *testing.T, cmd tea.Cmd) restoredMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("restoreBacklogCmd returned a nil command")
	}
	msg, ok := cmd().(restoredMsg)
	if !ok {
		t.Fatalf("restoreBacklogCmd produced %T, want restoredMsg", cmd())
	}
	return msg
}

// TestReplayBacklogSeam covers the narrow Agent backlog seam: a NEW (non-restored)
// session returns an empty/nil backlog (no repaint), a restored session returns its
// historical Enduring events, and a read failure surfaces a typed error the fold maps
// onto the restore-error path.
func TestReplayBacklogSeam(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)

	tests := []struct {
		name    string
		backlog []event.Event
		err     error
		wantLen int
		wantErr bool
	}{
		{name: "new session returns nil backlog", backlog: nil, wantLen: 0},
		{name: "new session returns empty backlog", backlog: []event.Event{}, wantLen: 0},
		{
			name:    "restored session returns its enduring backlog",
			backlog: []event.Event{event.TurnStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: primary}}}},
			wantLen: 1,
		},
		{name: "read failure surfaces a typed error", err: errors.New("replay read"), wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := &fakeAgent{primaryLoopID: primary, backlog: tt.backlog, replayErr: tt.err}
			got, err := agent.ReplayBacklog(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("ReplayBacklog() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !agent.replayCalled {
				t.Error("ReplayBacklog seam not exercised")
			}
			if !tt.wantErr && len(got) != tt.wantLen {
				t.Errorf("backlog len = %d, want %d", len(got), tt.wantLen)
			}
		})
	}
}

// TestRestoreBacklogFoldsOffLoopOnce is the headline no-UI-hang property: a LARGE
// backlog folds inside the background tea.Cmd (off the update loop) and yields EXACTLY
// ONE restoredMsg — the reducers are applied per-event INSIDE the command, never via N
// per-event update-loop messages. The Screen's Update is driven O(1) times (once with
// the single restoredMsg), not O(N) in backlog size, so a 5–10k-event backlog cannot
// hang the UI by flooding it with per-event messages.
func TestRestoreBacklogFoldsOffLoopOnce(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)

	// A large backlog: alternating TurnStarted + StepDone for the primary loop, so the
	// fold exercises the real commit path many thousands of times inside the command.
	const turns = 6000
	backlog := make([]event.Event, 0, turns*2)
	for i := 0; i < turns; i++ {
		backlog = append(backlog,
			event.TurnStarted{
				Header:  event.Header{Coordinates: identity.Coordinates{LoopID: primary}},
				Message: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "q"}}}},
			},
			event.StepDone{
				Header:   event.Header{Coordinates: identity.Coordinates{LoopID: primary}},
				Messages: content.AgenticMessages{aiMessage("", "a")},
			},
		)
	}

	agent := &fakeAgent{primaryLoopID: primary, backlog: backlog}

	// The fold runs OFF the update loop in restoreBacklogCmd. Executing it once yields a
	// SINGLE restoredMsg carrying the already-folded reducer state — no per-event message.
	msg := runRestoreCmd(t, restoreBacklogCmd(context.Background(), agent, primary))
	if msg.err != nil {
		t.Fatalf("restoredMsg err = %v, want nil", msg.err)
	}

	// Folding the same backlog directly through the reducers must equal the command's
	// result: the command folded every event itself, off-loop.
	wantTr, _ := foldBacklog(primary, backlog)
	if got, want := len(msg.transcript.committed), len(wantTr.committed); got != want {
		t.Fatalf("folded committed = %d, want %d (the command must fold the WHOLE backlog off-loop)", got, want)
	}

	// Applying the single restoredMsg drives the update loop ONCE. It must NOT re-fold
	// per event: the committed count after the single Update equals the pre-folded count,
	// proving the reducers ran inside the command, not N times on the loop.
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	before := len(m.transcript.committed)
	m, _ = updateScreen(t, m, msg)
	if got := len(m.transcript.committed) - before; got != len(wantTr.committed) {
		t.Errorf("committed installed by ONE restoredMsg = %d, want %d (state must arrive pre-folded, applied once)", got, len(wantTr.committed))
	}
}

// TestRestoredMsgRepaintCorrectness covers the repaint-correctness property: a backlog
// of TurnStarted + StepDone (+ TurnFoldedInto) folds into the EXACT committed transcript
// you get by feeding those same events through ApplyEvent directly, and a pending gate
// in the backlog is reflected in the rebuilt interaction model.
func TestRestoredMsgRepaintCorrectness(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	hdr := event.Header{Coordinates: identity.Coordinates{LoopID: primary}}

	backlog := []event.Event{
		event.TurnStarted{
			Header:  hdr,
			Message: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "first question"}}}},
		},
		event.StepDone{Header: hdr, Messages: content.AgenticMessages{aiMessage("", "first answer")}},
		event.TurnFoldedInto{
			Header:  hdr,
			Message: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "folded follow-up"}}}},
		},
		event.StepDone{Header: hdr, Messages: content.AgenticMessages{aiMessage("", "second answer")}},
		event.PermissionRequested{Header: hdr, ToolExecutionID: callID(7), Request: tool.BashRequest{Command: "ls"}},
	}

	agent := &fakeAgent{primaryLoopID: primary, backlog: backlog}

	msg := runRestoreCmd(t, restoreBacklogCmd(context.Background(), agent, primary))
	if msg.err != nil {
		t.Fatalf("restoredMsg err = %v, want nil", msg.err)
	}

	wantTr, wantIn := foldBacklog(primary, backlog)

	// The committed transcript must match the direct fold entry-for-entry.
	if got, want := len(msg.transcript.committed), len(wantTr.committed); got != want {
		t.Fatalf("committed = %d, want %d", got, want)
	}
	for i := range wantTr.committed {
		g, w := msg.transcript.committed[i], wantTr.committed[i]
		if g.Kind != w.Kind {
			t.Errorf("committed[%d].Kind = %d, want %d", i, g.Kind, w.Kind)
		}
		if committedText(g) != committedText(w) {
			t.Errorf("committed[%d] text = %q, want %q", i, committedText(g), committedText(w))
		}
	}

	// The pending permission gate from the backlog is reflected in the interaction model.
	if got, want := msg.interaction.PendingCount(), wantIn.PendingCount(); got != want {
		t.Errorf("pending prompts = %d, want %d (backlog gate must repaint as pending)", got, want)
	}
}

// TestRestoredMsgFlushesScrollbackOnce covers the cold-restore handoff repaint (10.2):
// applying a restoredMsg installs the rebuilt transcript/interaction and flushes the
// committed transcript to scrollback ONCE (a single non-nil print command), then leaves
// the Screen ready for the live Subscribe path.
func TestRestoredMsgFlushesScrollbackOnce(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	hdr := event.Header{Coordinates: identity.Coordinates{LoopID: primary}}
	backlog := []event.Event{
		event.TurnStarted{
			Header:  hdr,
			Message: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "q"}}}},
		},
		event.StepDone{Header: hdr, Messages: content.AgenticMessages{aiMessage("", "a")}},
	}
	agent := &fakeAgent{primaryLoopID: primary, backlog: backlog}

	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	msg := runRestoreCmd(t, restoreBacklogCmd(context.Background(), agent, primary))
	m, cmd := updateScreen(t, m, msg)

	// The rebuilt transcript is installed.
	wantTr, _ := foldBacklog(primary, backlog)
	if got, want := len(m.transcript.committed), len(wantTr.committed); got != want {
		t.Fatalf("installed committed = %d, want %d", got, want)
	}
	// Exactly one repaint: a single non-nil flush command for the committed backlog.
	if cmd == nil {
		t.Fatal("restoredMsg cmd = nil, want a single flush command repainting the backlog")
	}
	// A SECOND apply of the same already-installed state is a no-op (print-once): the
	// flush engine must not reprint entries it already committed to scrollback.
	m2, cmd2 := updateScreen(t, m, msg)
	if cmd2 != nil {
		t.Error("re-applying restoredMsg reprinted scrollback; the print-once engine must flush each entry exactly once")
	}
	if len(m2.transcript.committed) != len(wantTr.committed) {
		// installing the same pre-folded transcript twice is idempotent (it overwrites)
		t.Errorf("second apply committed = %d, want %d (install is idempotent)", len(m2.transcript.committed), len(wantTr.committed))
	}
}

// TestRestoredMsgHandoffToLiveSubscribe covers the handoff (10.2): after a restoredMsg
// repaints the backlog, the live Subscribe path is attached (subscribedMsg installs the
// stream) and a subsequently-delivered live event updates the transcript NORMALLY,
// continuing from the repainted state — no backlog/live overlap, no dedup needed.
func TestRestoredMsgHandoffToLiveSubscribe(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	hdr := event.Header{Coordinates: identity.Coordinates{LoopID: primary}}
	backlog := []event.Event{
		event.TurnStarted{
			Header:  hdr,
			Message: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "history q"}}}},
		},
		event.StepDone{Header: hdr, Messages: content.AgenticMessages{aiMessage("", "history a")}},
	}
	sub := newFakeSubscription()
	agent := &fakeAgent{primaryLoopID: primary, backlog: backlog, subStream: sub}

	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	// Repaint the cold backlog.
	msg := runRestoreCmd(t, restoreBacklogCmd(context.Background(), agent, primary))
	m, _ = updateScreen(t, m, msg)
	repaintCount := len(m.transcript.committed)
	if repaintCount == 0 {
		t.Fatal("backlog did not repaint any committed entries")
	}

	// Attach the live subscription (the existing Subscribe path), exactly as the runtime
	// would after restore.
	m, _ = updateScreen(t, m, subscribedMsg{sub: sub})
	if m.sub == nil {
		t.Fatal("live subscription not installed after restore handoff")
	}

	// A live turn arrives AFTER restore and continues from the repainted state: a new
	// user turn commits ON TOP of the restored history.
	m = feed(t, m, event.TurnStarted{
		Header:  hdr,
		Message: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "live q"}}}},
	})
	if got := len(m.transcript.committed); got != repaintCount+1 {
		t.Errorf("committed after live turn = %d, want %d (live event must extend the repainted transcript)", got, repaintCount+1)
	}
	if last := lastCommitted(t, m); committedText(last) != "live q" {
		t.Errorf("last committed = %q, want %q (live continues from repainted state)", committedText(last), "live q")
	}
}

// TestRestoredMsgError covers the restore-read failure path: a backlog read failure
// surfaces a faint, non-fatal error notice (the user learns the restore could not
// repaint history) WITHOUT installing any committed entries.
func TestRestoredMsgError(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	agent := &fakeAgent{primaryLoopID: primary, replayErr: errors.New("replay read")}

	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	msg := runRestoreCmd(t, restoreBacklogCmd(context.Background(), agent, primary))
	if msg.err == nil {
		t.Fatal("restoredMsg err = nil on a read failure, want the typed error")
	}

	m, cmd := updateScreen(t, m, msg)
	if cmd == nil {
		t.Error("restoredMsg error cmd = nil, want a flush of the error notice")
	}
	rec := lastCommitted(t, m)
	if rec.Kind != kindNotice || rec.Level != noticeError {
		t.Errorf("committed = (kind %d, level %d), want (kindNotice, noticeError)", rec.Kind, rec.Level)
	}
}

// TestNewSessionNoRepaint covers the new-session case (empty backlog): the restore
// command yields an empty restoredMsg, applying it commits NOTHING and prints NOTHING
// — a new session behaves exactly as today (no repaint), so the existing screen tests
// remain valid.
func TestNewSessionNoRepaint(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	agent := &fakeAgent{primaryLoopID: primary, backlog: nil}

	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})
	m, _ = updateScreen(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})

	msg := runRestoreCmd(t, restoreBacklogCmd(context.Background(), agent, primary))
	if msg.err != nil {
		t.Fatalf("restoredMsg err = %v, want nil (empty backlog is not a failure)", msg.err)
	}
	if len(msg.transcript.committed) != 0 {
		t.Fatalf("empty-backlog fold committed = %d, want 0", len(msg.transcript.committed))
	}

	m, cmd := updateScreen(t, m, msg)
	if cmd != nil {
		t.Error("empty restoredMsg cmd = non-nil, want nil (a new session does not repaint)")
	}
	if len(m.transcript.committed) != 0 {
		t.Errorf("committed = %d, want 0 (no repaint for a new session)", len(m.transcript.committed))
	}
}

// TestInitTriggersRestore pins that Init schedules the restore-repaint BEFORE the live
// subscription drains: Init must batch restoreBacklogCmd alongside the subscribe so the
// cold backlog repaints first, then the live Subscribe path takes over.
func TestInitTriggersRestore(t *testing.T) {
	t.Parallel()

	primary := callID(0xAA)
	agent := &fakeAgent{primaryLoopID: primary}
	m := New(context.Background(), agent, fakeOpen(agent), AgentBanner{})

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() = nil, want non-nil (restore + subscribe batched)")
	}
	// Draining Init's batch must exercise the restore seam (ReplayBacklog) — the cold
	// restore-repaint is scheduled at startup, not lazily.
	drainCmd(t, cmd)
	if !agent.replayCalled {
		t.Error("Init did not schedule the restore-repaint (ReplayBacklog not called)")
	}
}
