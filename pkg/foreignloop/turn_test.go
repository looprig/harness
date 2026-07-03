package foreignloop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/uuid"
)

// eventKind names a foreign event for an ordered-sequence assertion.
func eventKind(ev event.Event) string {
	switch ev.(type) {
	case event.TurnStarted:
		return "TurnStarted"
	case event.TokenDelta:
		return "TokenDelta"
	case event.ToolCallStarted:
		return "ToolCallStarted"
	case event.ToolCallCompleted:
		return "ToolCallCompleted"
	case event.StepDone:
		return "StepDone"
	case event.TurnDone:
		return "TurnDone"
	case event.TurnFailed:
		return "TurnFailed"
	case event.TurnInterrupted:
		return "TurnInterrupted"
	default:
		return fmt.Sprintf("%T", ev)
	}
}

func eventKinds(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = eventKind(ev)
	}
	return out
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// firstText returns the first TextBlock text of a conversation message, or "".
func firstText(t *testing.T, c content.Conversation) string {
	t.Helper()
	switch m := c.(type) {
	case *content.AIMessage:
		return firstBlockText(t, m.Blocks)
	case *content.UserMessage:
		return firstBlockText(t, m.Blocks)
	default:
		t.Fatalf("unexpected conversation type %T", c)
		return ""
	}
}

func firstBlockText(t *testing.T, blocks []content.Block) string {
	t.Helper()
	for _, b := range blocks {
		if tb, ok := b.(*content.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

// newDrivenLoop wires a loop to a scripted fakeAgent + fakePublisher and returns
// all three, with ctx cleanup registered.
func newDrivenLoop(t *testing.T, agent *fakeAgent) (*Loop, *fakePublisher) {
	t.Helper()
	pub := &fakePublisher{}
	l, _ := newTestLoop(t, Spec{Agent: agent}, pub)
	return l, pub
}

// waitTurnIndex polls Snapshot until the committed turnIndex reaches want, which
// proves the actor applied the turn outcome and returned to idle.
func waitTurnIndex(t *testing.T, l *Loop, want event.TurnIndex) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, ti, err := l.Snapshot(context.Background())
		if err == nil && ti == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("turnIndex did not reach %d within timeout", want)
}

// writeTranscript writes a single-assistant-record transcript file and returns its
// path. The record decodes to one AIMessage carrying the given text.
func writeTranscript(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	line := fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}`, text)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

// waitForKind polls the publisher until an event of the named kind appears.
func waitForKind(t *testing.T, pub *fakePublisher, kind string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range pub.snapshot() {
			if eventKind(ev) == kind {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no %s event published within timeout; got %v", kind, eventKinds(pub.snapshot()))
}

func submitUserInput(t *testing.T, l *Loop, text string) {
	t.Helper()
	submitUserInputWithID(t, l, text, mustID(t))
}

// submitUserInputWithID submits a UserInput carrying an explicit CommandID so a test
// can assert the published TurnStarted's Cause.CommandID echoes it (the session drain
// correlates the opening turn on exactly this field).
func submitUserInputWithID(t *testing.T, l *Loop, text string, commandID uuid.UUID) {
	t.Helper()
	cmd := command.UserInput{
		Header: command.Header{CommandID: commandID},
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}
	select {
	case l.Commands <- cmd:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out submitting UserInput")
	}
}

func TestUserInputHappyPath(t *testing.T) {
	t.Parallel()
	transcript := writeTranscript(t, "committed reply")
	agent := &fakeAgent{
		transcript: transcript,
		events: []ForeignEvent{
			{Kind: ForeignInit, SessionID: ""},
			{Kind: ForeignTextDelta, Text: "hi"},
			{Kind: ForeignToolUse, ToolUseID: "t1", ToolName: "Bash"},
			{Kind: ForeignToolResult, ToolUseID: "t1", ResultPreview: "ok"},
			{Kind: ForeignStepComplete, Message: aiMessage("step")},
			{Kind: ForeignTerminalOK, Message: aiMessage("done")},
		},
	}
	l, pub := newDrivenLoop(t, agent)

	var spawnAtCount int
	agent.onSpawn = func() { spawnAtCount = pub.count() }

	commandID := mustID(t)
	submitUserInputWithID(t, l, "do the thing", commandID)
	waitTurnIndex(t, l, 1)

	evs := pub.snapshot()
	want := []string{"TurnStarted", "TokenDelta", "ToolCallStarted", "ToolCallCompleted", "StepDone", "TurnDone"}
	if got := eventKinds(evs); !eqStrs(got, want) {
		t.Fatalf("published sequence = %v, want %v", got, want)
	}

	// TurnStarted carries the user blocks, and Spawn ran AFTER it was published.
	ts, ok := evs[0].(event.TurnStarted)
	if !ok {
		t.Fatalf("evs[0] = %T, want TurnStarted", evs[0])
	}
	if ts.Message == nil || firstText(t, ts.Message) != "do the thing" {
		t.Fatalf("TurnStarted.Message = %+v, want user blocks 'do the thing'", ts.Message)
	}
	// TurnStarted.Cause.CommandID echoes the submit id so the session drain can
	// correlate the opening turn (this Cause survives the publish chokepoint + Stamp).
	if got := ts.EventHeader().Cause.CommandID; got != commandID {
		t.Fatalf("TurnStarted.Cause.CommandID = %v, want submit id %v", got, commandID)
	}
	if spawnAtCount < 1 {
		t.Fatalf("Spawn observed %d published events, want >=1 (TurnStarted before Spawn)", spawnAtCount)
	}
	if agent.calls() != 1 {
		t.Fatalf("Spawn called %d times, want 1", agent.calls())
	}

	// StepDone carries the transcript-derived assistant message.
	sd, ok := evs[4].(event.StepDone)
	if !ok || len(sd.Messages) != 1 || firstText(t, sd.Messages[0]) != "committed reply" {
		t.Fatalf("StepDone = %+v, want transcript message 'committed reply'", sd)
	}

	// Snapshot reflects the committed (transcript-derived) assistant message.
	msgs, ti, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if ti != 1 {
		t.Fatalf("turnIndex = %d, want 1", ti)
	}
	if len(msgs) != 1 || firstText(t, msgs[0]) != "committed reply" {
		t.Fatalf("committed msgs = %v, want one AIMessage 'committed reply'", msgs)
	}
	shutdown(t, l)
}

func TestSpawnFailureTurnFailed(t *testing.T) {
	t.Parallel()
	agent := &fakeAgent{spawnErr: errors.New("agent failed to start")}
	l, pub := newDrivenLoop(t, agent)

	submitUserInput(t, l, "go")
	waitForKind(t, pub, "TurnFailed")

	evs := pub.snapshot()
	want := []string{"TurnStarted", "TurnFailed"}
	if got := eventKinds(evs); !eqStrs(got, want) {
		t.Fatalf("published sequence = %v, want %v", got, want)
	}
	tf, ok := evs[1].(event.TurnFailed)
	if !ok {
		t.Fatalf("evs[1] = %T, want TurnFailed", evs[1])
	}
	var spawnErr *SpawnError
	if !errors.As(tf.Err, &spawnErr) {
		t.Fatalf("TurnFailed.Err = %T %v, want *SpawnError", tf.Err, tf.Err)
	}
	// A failed turn commits nothing and does not advance the turn count.
	msgs, ti, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(msgs) != 0 || ti != 0 {
		t.Fatalf("after spawn failure: msgs=%d turnIndex=%d, want 0/0", len(msgs), ti)
	}
	shutdown(t, l)
}

func TestTranscriptLossSoftDegrade(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	agent := &fakeAgent{
		transcript: missing,
		events: []ForeignEvent{
			{Kind: ForeignTextDelta, Text: "thinking"},
			{Kind: ForeignStepComplete, Message: aiMessage("soft reply")},
			{Kind: ForeignTerminalOK},
		},
	}
	l, pub := newDrivenLoop(t, agent)

	submitUserInput(t, l, "go")
	waitTurnIndex(t, l, 1)

	evs := pub.snapshot()
	want := []string{"TurnStarted", "TokenDelta", "StepDone", "TurnDone"}
	if got := eventKinds(evs); !eqStrs(got, want) {
		t.Fatalf("published sequence = %v, want %v", got, want)
	}
	sd, ok := evs[2].(event.StepDone)
	if !ok || len(sd.Messages) != 1 || firstText(t, sd.Messages[0]) != "soft reply" {
		t.Fatalf("synthetic StepDone = %+v, want assistant 'soft reply'", sd)
	}
	// The soft-degraded assistant message is committed so restore keeps the reply.
	msgs, _, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(msgs) != 1 || firstText(t, msgs[0]) != "soft reply" {
		t.Fatalf("committed msgs = %v, want one AIMessage 'soft reply'", msgs)
	}
	shutdown(t, l)
}

// assertNoTerminalExcept fails if any TurnDone/TurnFailed was published.
func assertNoSuccessOrFailTerminal(t *testing.T, pub *fakePublisher) {
	t.Helper()
	for _, ev := range pub.snapshot() {
		switch ev.(type) {
		case event.TurnDone, event.TurnFailed:
			t.Fatalf("unexpected non-interrupt terminal %T published", ev)
		}
	}
}

func sendInterrupt(t *testing.T, l *Loop) {
	t.Helper()
	ack := make(chan bool, 1)
	select {
	case l.Commands <- command.Interrupt{Ack: ack}:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out submitting Interrupt")
	}
	select {
	case got := <-ack:
		if !got {
			t.Fatal("Interrupt ack = false, want true (a turn was running)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Interrupt never acked")
	}
}

func TestInterruptDuringTurn(t *testing.T) {
	t.Parallel()
	agent := &fakeAgent{block: true, events: []ForeignEvent{{Kind: ForeignInit}}}
	l, pub := newDrivenLoop(t, agent)

	submitUserInput(t, l, "long running task")
	waitForKind(t, pub, "TurnStarted")

	sendInterrupt(t, l)
	waitForKind(t, pub, "TurnInterrupted")
	assertNoSuccessOrFailTerminal(t, pub)

	want := []string{"TurnStarted", "TurnInterrupted"}
	if got := eventKinds(pub.snapshot()); !eqStrs(got, want) {
		t.Fatalf("published sequence = %v, want %v", got, want)
	}
	// An interrupted turn commits nothing.
	msgs, ti, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(msgs) != 0 || ti != 0 {
		t.Fatalf("after interrupt: msgs=%d turnIndex=%d, want 0/0", len(msgs), ti)
	}
	shutdown(t, l)
}

func TestDropCommandDuringTurnThenInterrupt(t *testing.T) {
	t.Parallel()
	agent := &fakeAgent{block: true, events: []ForeignEvent{{Kind: ForeignInit}}}
	l, pub := newDrivenLoop(t, agent)

	submitUserInput(t, l, "long running task")
	waitForKind(t, pub, "TurnStarted")

	// An un-honorable command mid-turn must be dropped without blocking the actor
	// and without publishing anything.
	approve := command.ApproveToolCall{
		Header:    command.Header{CommandID: mustID(t)},
		GateRoute: command.GateRoute{ToolExecutionID: mustID(t)},
	}
	select {
	case l.Commands <- approve:
	case <-time.After(2 * time.Second):
		t.Fatal("ApproveToolCall during turn was not consumed (actor blocked)")
	}

	// Interrupt still works after the drop.
	sendInterrupt(t, l)
	waitForKind(t, pub, "TurnInterrupted")

	want := []string{"TurnStarted", "TurnInterrupted"}
	if got := eventKinds(pub.snapshot()); !eqStrs(got, want) {
		t.Fatalf("published sequence = %v, want %v (drop must publish nothing)", got, want)
	}
	shutdown(t, l)
}

func TestShutdownDuringTurn(t *testing.T) {
	t.Parallel()
	agent := &fakeAgent{block: true, events: []ForeignEvent{{Kind: ForeignInit}}}
	l, pub := newDrivenLoop(t, agent)

	submitUserInput(t, l, "long running task")
	waitForKind(t, pub, "TurnStarted")

	ack := make(chan error, 1)
	select {
	case l.Commands <- command.Shutdown{Ack: ack}:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out submitting Shutdown during turn")
	}
	select {
	case err := <-ack:
		if err != nil {
			t.Fatalf("Shutdown ack = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown during turn never acked")
	}
	select {
	case <-l.Done:
	case <-time.After(3 * time.Second):
		t.Fatal("Done did not close after Shutdown during turn")
	}
}
