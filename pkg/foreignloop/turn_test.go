package foreignloop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
)

// eventKind names a foreign event for an ordered-sequence assertion.
func eventKind(ev event.Event) string {
	switch ev.(type) {
	case event.TurnStarted:
		return "TurnStarted"
	case event.ForeignSessionBound:
		return "ForeignSessionBound"
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

// waitForKindCount polls until the publisher has recorded want events of kind.
func waitForKindCount(t *testing.T, pub *fakePublisher, kind string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got := 0
		for _, ev := range pub.snapshot() {
			if eventKind(ev) == kind {
				got++
			}
		}
		if got >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s count did not reach %d within timeout; got %v", kind, want, eventKinds(pub.snapshot()))
}

// waitLoopIdle uses Interrupt's false acknowledgement as the actor-owned idle seam.
func waitLoopIdle(t *testing.T, l *Loop) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ack := make(chan bool, 1)
		select {
		case l.Commands <- command.Interrupt{Ack: ack}:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out probing loop idle state")
		}
		select {
		case active := <-ack:
			if !active {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("idle probe was not acknowledged")
		}
	}
	t.Fatal("loop did not become idle within timeout")
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

func TestLateBoundSessionPublishesForeignSessionBound(t *testing.T) {
	t.Parallel()
	agent := &fakeAgent{
		transcript: filepath.Join(t.TempDir(), "missing.jsonl"),
		events: []ForeignEvent{
			{Kind: ForeignInit, SessionID: "codex-thread-1"},
			{Kind: ForeignStepComplete, Message: aiMessage("ok")},
			{Kind: ForeignTerminalOK},
		},
	}
	pub := &fakePublisher{}
	l, sid := newTestLoop(t, Spec{
		Agent:   agent,
		Cwd:     t.TempDir(),
		SIDMode: SIDLateBound,
	}, pub)
	if sid != "" {
		t.Fatalf("initial sid = %q, want empty for late-bound loop", sid)
	}

	submitUserInput(t, l, "first")
	waitTurnIndex(t, l, 1)

	evs := pub.snapshot()
	want := []string{"TurnStarted", "ForeignSessionBound", "StepDone", "TurnDone"}
	if got := eventKinds(evs); !eqStrs(got, want) {
		t.Fatalf("published sequence = %v, want %v", got, want)
	}
	bound, ok := evs[1].(event.ForeignSessionBound)
	if !ok {
		t.Fatalf("evs[1] = %T, want ForeignSessionBound", evs[1])
	}
	if bound.ForeignSID != "codex-thread-1" {
		t.Fatalf("ForeignSessionBound.ForeignSID = %q, want codex-thread-1", bound.ForeignSID)
	}
	if err := event.ValidateEvent(bound); err != nil {
		t.Fatalf("ForeignSessionBound failed validation: %v", err)
	}
	if !bound.EventHeader().Coordinates.TurnID.IsZero() {
		t.Fatalf("ForeignSessionBound TurnID = %v, want zero", bound.EventHeader().Coordinates.TurnID)
	}
	if !bound.EventHeader().Coordinates.StepID.IsZero() {
		t.Fatalf("ForeignSessionBound StepID = %v, want zero", bound.EventHeader().Coordinates.StepID)
	}

	submitUserInput(t, l, "second")
	waitTurnIndex(t, l, 2)

	ft := agent.lastForeignTurn()
	if ft.StartNew {
		t.Fatal("second ForeignTurn.StartNew = true, want false")
	}
	if ft.ForeignSID != "codex-thread-1" {
		t.Fatalf("second ForeignTurn.ForeignSID = %q, want codex-thread-1", ft.ForeignSID)
	}
	shutdown(t, l)
}

type orderedLock struct {
	name  string
	held  bool
	trace *[]string
}

func (l *orderedLock) release() {
	*l.trace = append(*l.trace, "release "+l.name)
	l.held = false
}

type orderedStream struct {
	events  chan ForeignEvent
	durable *orderedLock
	trace   *[]string
	once    sync.Once
}

func (s *orderedStream) Events() <-chan ForeignEvent { return s.events }
func (s *orderedStream) TranscriptPath() string      { return "" }
func (s *orderedStream) Close() error {
	s.once.Do(func() {
		if s.durable.held {
			*s.trace = append(*s.trace, "close stream while durable held")
		} else {
			*s.trace = append(*s.trace, "close stream after durable release")
		}
	})
	return nil
}

type orderedAgent struct{ stream ForeignStream }

func (a orderedAgent) Spawn(context.Context, ForeignTurn) (ForeignStream, error) {
	return a.stream, nil
}

func TestLateBoundTurnLockLifecycleOrder(t *testing.T) {
	t.Parallel()
	var trace []string
	temporary := &orderedLock{name: "temporary", held: true, trace: &trace}
	durable := &orderedLock{name: "durable", trace: &trace}
	locks := turnLockOps{
		acquireTemporary: func(string, string) (turnLock, error) {
			trace = append(trace, "acquire temporary")
			return temporary, nil
		},
		acquireDurable: func(string, string) (turnLock, error) {
			trace = append(trace, "acquire durable")
			durable.held = true
			return durable, nil
		},
	}
	events := make(chan ForeignEvent, 2)
	events <- ForeignEvent{Kind: ForeignInit, SessionID: "foreign-session"}
	events <- ForeignEvent{Kind: ForeignTerminalOK}
	close(events)
	stream := &orderedStream{events: events, durable: durable, trace: &trace}
	l := &Loop{spec: Spec{Agent: orderedAgent{stream: stream}, Cwd: t.TempDir()}}
	result := make(chan turnOutcome, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pub := func(ev event.Event) {
		if _, ok := ev.(event.ForeignSessionBound); ok {
			trace = append(trace, "publish ForeignSessionBound")
		}
	}

	go l.driveTurnWithLocks(ctx, cancel, ForeignTurn{}, 1, false, pub, result, locks)
	select {
	case <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for turn outcome")
	}
	trace = append(trace, "actor receives outcome")

	want := []string{
		"acquire temporary",
		"acquire durable",
		"release temporary",
		"publish ForeignSessionBound",
		"close stream while durable held",
		"release durable",
		"actor receives outcome",
	}
	if !eqStrs(trace, want) {
		t.Fatalf("lifecycle trace = %v, want %v", trace, want)
	}
}

func TestLateBoundFirstTurnHoldsBoundSIDLock(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sid  string
	}{
		{name: "bound event implies durable sid is locked", sid: "codex-thread-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cwd := t.TempDir()
			agent := &fakeAgent{
				transcript: filepath.Join(t.TempDir(), "missing.jsonl"),
				block:      true,
				events: []ForeignEvent{
					{Kind: ForeignInit, SessionID: tt.sid},
				},
			}
			pub := &fakePublisher{}
			l, _ := newTestLoop(t, Spec{Agent: agent, Cwd: cwd, SIDMode: SIDLateBound}, pub)

			submitUserInput(t, l, "first")
			waitForKind(t, pub, "ForeignSessionBound")

			lk, err := acquireForeignLock(tt.sid, cwd)
			if lk != nil {
				lk.release()
			}
			var busy *ForeignSessionBusyError
			if !errors.As(err, &busy) {
				t.Fatalf("bound sid acquire err = %T %v, want *ForeignSessionBusyError", err, err)
			}
			shutdown(t, l)
		})
	}
}

func TestLateBoundFirstTurnsUseIndependentTemporaryLocks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		firstSID  string
		secondSID string
	}{
		{name: "same cwd loops bind different sessions concurrently", firstSID: "codex-thread-1", secondSID: "codex-thread-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cwd := t.TempDir()
			firstAgent := &fakeAgent{block: true, events: []ForeignEvent{{Kind: ForeignInit, SessionID: tt.firstSID}}}
			secondAgent := &fakeAgent{block: true, events: []ForeignEvent{{Kind: ForeignInit, SessionID: tt.secondSID}}}
			firstPub := &fakePublisher{}
			secondPub := &fakePublisher{}
			first, _ := newTestLoop(t, Spec{Agent: firstAgent, Cwd: cwd, SIDMode: SIDLateBound}, firstPub)
			second, _ := newTestLoop(t, Spec{Agent: secondAgent, Cwd: cwd, SIDMode: SIDLateBound}, secondPub)

			submitUserInput(t, first, "first")
			waitForKind(t, firstPub, "ForeignSessionBound")
			submitUserInput(t, second, "second")
			waitForKind(t, secondPub, "ForeignSessionBound")

			shutdown(t, first)
			shutdown(t, second)
		})
	}
}

func TestLateBoundLockTransitionFailurePersistsSID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sid  string
	}{
		{name: "busy durable sid fails turn after recording learned sid", sid: "codex-thread-busy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cwd := t.TempDir()
			preWriteLock(t, tt.sid, cwd, fmt.Sprint(os.Getpid()))
			agent := &fakeAgent{block: true, events: []ForeignEvent{{Kind: ForeignInit, SessionID: tt.sid}}}
			pub := &fakePublisher{}
			l, _ := newTestLoop(t, Spec{Agent: agent, Cwd: cwd, SIDMode: SIDLateBound}, pub)

			submitUserInput(t, l, "first")
			waitForKind(t, pub, "TurnFailed")

			want := []string{"TurnStarted", "ForeignSessionBound", "TurnFailed"}
			if got := eventKinds(pub.snapshot()); !eqStrs(got, want) {
				t.Fatalf("published sequence = %v, want %v", got, want)
			}
			var busy *ForeignSessionBusyError
			if tf := findTurnFailed(t, pub); !errors.As(tf.Err, &busy) {
				t.Fatalf("TurnFailed.Err = %T %v, want *ForeignSessionBusyError", tf.Err, tf.Err)
			}

			waitLoopIdle(t, l)
			submitUserInput(t, l, "resume")
			waitForKindCount(t, pub, "TurnFailed", 2)
			if agent.calls() != 1 {
				t.Fatalf("agent spawned %d times, want 1 (resume must use learned busy sid before spawn)", agent.calls())
			}
			shutdown(t, l)
		})
	}
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

type streamScript struct {
	events   []ForeignEvent
	block    bool
	closeErr error
}

type scriptedCloseStream struct {
	*fakeStream
	closeErr error
	mu       sync.Mutex
	closes   int
}

func (s *scriptedCloseStream) Close() error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	_ = s.fakeStream.Close()
	return s.closeErr
}

func (s *scriptedCloseStream) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

type scriptedCloseAgent struct {
	mu      sync.Mutex
	scripts []streamScript
	turns   []ForeignTurn
	streams []*scriptedCloseStream
}

func (a *scriptedCloseAgent) Spawn(ctx context.Context, turn ForeignTurn) (ForeignStream, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.turns) >= len(a.scripts) {
		return nil, errors.New("unexpected spawn")
	}
	script := a.scripts[len(a.turns)]
	stream := &scriptedCloseStream{
		fakeStream: &fakeStream{
			events: script.events,
			block:  script.block,
			ctx:    ctx,
			stop:   make(chan struct{}),
		},
		closeErr: script.closeErr,
	}
	a.turns = append(a.turns, turn)
	a.streams = append(a.streams, stream)
	return stream, nil
}

func (a *scriptedCloseAgent) turn(n int) ForeignTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turns[n]
}

func (a *scriptedCloseAgent) stream(n int) *scriptedCloseStream {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.streams[n]
}

func TestCloseErrorFailsTurn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		closeErr error
		assert   func(*testing.T, error)
	}{
		{
			name:     "foreign exit",
			closeErr: &ForeignExitError{Code: 7},
			assert: func(t *testing.T, err error) {
				var target *ForeignExitError
				if !errors.As(err, &target) || target.Code != 7 {
					t.Fatalf("TurnFailed.Err = %T %v, want ForeignExitError code 7", err, err)
				}
			},
		},
		{
			name:     "decode error",
			closeErr: &DecodeError{Cause: errors.New("bad jsonl")},
			assert: func(t *testing.T, err error) {
				var target *DecodeError
				if !errors.As(err, &target) {
					t.Fatalf("TurnFailed.Err = %T %v, want DecodeError", err, err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := &scriptedCloseAgent{scripts: []streamScript{{
				events:   []ForeignEvent{{Kind: ForeignTerminalOK}},
				closeErr: tt.closeErr,
			}}}
			pub := &fakePublisher{}
			l, _ := newTestLoop(t, Spec{Agent: agent}, pub)

			submitUserInput(t, l, "go")
			waitForKind(t, pub, "TurnFailed")
			waitLoopIdle(t, l)

			if got, want := eventKinds(pub.snapshot()), []string{"TurnStarted", "TurnFailed"}; !eqStrs(got, want) {
				t.Fatalf("published sequence = %v, want %v", got, want)
			}
			tt.assert(t, findTurnFailed(t, pub).Err)
			if got := agent.stream(0).closeCount(); got != 1 {
				t.Fatalf("Close called %d times, want 1", got)
			}
			_, turnIndex, err := l.Snapshot(context.Background())
			if err != nil || turnIndex != 0 {
				t.Fatalf("Snapshot turnIndex/error = %d/%v, want 0/nil", turnIndex, err)
			}
			shutdown(t, l)
		})
	}
}

func TestTerminalAndCloseErrorsRetainBothTypedCauses(t *testing.T) {
	t.Parallel()
	decodeErr := &DecodeError{Cause: errors.New("bad jsonl")}
	agent := &scriptedCloseAgent{scripts: []streamScript{{
		events:   []ForeignEvent{{Kind: ForeignTerminalError, ErrText: "error_max_turns"}},
		closeErr: decodeErr,
	}}}
	pub := &fakePublisher{}
	l, _ := newTestLoop(t, Spec{Agent: agent}, pub)

	submitUserInput(t, l, "go")
	waitForKind(t, pub, "TurnFailed")
	waitLoopIdle(t, l)

	err := findTurnFailed(t, pub).Err
	var resultErr *ForeignResultError
	if !errors.As(err, &resultErr) {
		t.Fatalf("TurnFailed.Err = %T %v, want ForeignResultError", err, err)
	}
	var gotDecode *DecodeError
	if !errors.As(err, &gotDecode) {
		t.Fatalf("TurnFailed.Err = %T %v, want DecodeError", err, err)
	}
	if got := agent.stream(0).closeCount(); got != 1 {
		t.Fatalf("Close called %d times, want 1", got)
	}
	shutdown(t, l)
}

func TestEOFWithoutForeignTerminalFailsTurn(t *testing.T) {
	t.Parallel()
	agent := &scriptedCloseAgent{scripts: []streamScript{{events: []ForeignEvent{{Kind: ForeignStepComplete, Message: aiMessage("partial")}}}}}
	pub := &fakePublisher{}
	l, _ := newTestLoop(t, Spec{Agent: agent}, pub)

	submitUserInput(t, l, "go")
	waitForKind(t, pub, "TurnFailed")
	waitLoopIdle(t, l)

	if got := eventKinds(pub.snapshot()); !eqStrs(got, []string{"TurnStarted", "TurnFailed"}) {
		t.Fatalf("published sequence = %v, want TurnFailed without TurnDone", got)
	}
	var protocolErr *ForeignProtocolError
	if err := findTurnFailed(t, pub).Err; !errors.As(err, &protocolErr) {
		t.Fatalf("TurnFailed.Err = %T %v, want typed foreign protocol error", err, err)
	}
	if got := agent.stream(0).closeCount(); got != 1 {
		t.Fatalf("Close called %d times, want 1", got)
	}
	_, turnIndex, err := l.Snapshot(context.Background())
	if err != nil || turnIndex != 0 {
		t.Fatalf("Snapshot turnIndex/error = %d/%v, want 0/nil", turnIndex, err)
	}
	shutdown(t, l)
}

func TestLateBoundFailureBeforeInitRetriesStartNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		first     streamScript
		interrupt bool
	}{
		{name: "EOF", first: streamScript{}},
		{name: "close failure", first: streamScript{events: []ForeignEvent{{Kind: ForeignTerminalOK}}, closeErr: &ForeignExitError{Code: 9}}},
		{name: "interruption", first: streamScript{block: true}, interrupt: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agent := &scriptedCloseAgent{scripts: []streamScript{
				tt.first,
				{events: []ForeignEvent{{Kind: ForeignInit, SessionID: "codex-thread-2"}, {Kind: ForeignTerminalOK}}},
			}}
			pub := &fakePublisher{}
			l, _ := newTestLoop(t, Spec{Agent: agent, Cwd: t.TempDir(), SIDMode: SIDLateBound}, pub)

			submitUserInput(t, l, "first")
			if tt.interrupt {
				waitForKind(t, pub, "TurnStarted")
				sendInterrupt(t, l)
				waitForKind(t, pub, "TurnInterrupted")
			} else {
				waitForKind(t, pub, "TurnFailed")
				waitLoopIdle(t, l)
			}

			submitUserInput(t, l, "second")
			waitTurnIndex(t, l, 1)
			second := agent.turn(1)
			if !second.StartNew || second.ForeignSID != "" {
				t.Fatalf("second ForeignTurn = {StartNew:%t ForeignSID:%q}, want true/empty", second.StartNew, second.ForeignSID)
			}
			if got := agent.stream(0).closeCount(); got != 1 {
				t.Fatalf("first stream Close called %d times, want 1", got)
			}
			shutdown(t, l)
		})
	}
}

func TestLateBoundTerminalOKBeforeInitFailsProtocolAndRetriesStartNew(t *testing.T) {
	t.Parallel()
	agent := &scriptedCloseAgent{scripts: []streamScript{
		{events: []ForeignEvent{{Kind: ForeignTerminalOK}, {Kind: ForeignInit, SessionID: "too-late"}}},
		{events: []ForeignEvent{{Kind: ForeignInit, SessionID: "codex-thread-2"}, {Kind: ForeignTerminalOK}}},
	}}
	pub := &fakePublisher{}
	l, _ := newTestLoop(t, Spec{Agent: agent, Cwd: t.TempDir(), SIDMode: SIDLateBound}, pub)

	submitUserInput(t, l, "first")
	waitForKind(t, pub, "TurnFailed")
	waitLoopIdle(t, l)
	if got, want := eventKinds(pub.snapshot()), []string{"TurnStarted", "TurnFailed"}; !eqStrs(got, want) {
		t.Fatalf("first-turn published sequence = %v, want %v", got, want)
	}

	var protocolErr *ForeignProtocolError
	if err := findTurnFailed(t, pub).Err; !errors.As(err, &protocolErr) {
		t.Fatalf("TurnFailed.Err = %T %v, want ForeignProtocolError", err, err)
	}
	_, turnIndex, err := l.Snapshot(context.Background())
	if err != nil || turnIndex != 0 {
		t.Fatalf("Snapshot turnIndex/error = %d/%v, want 0/nil", turnIndex, err)
	}

	submitUserInput(t, l, "second")
	waitTurnIndex(t, l, 1)
	second := agent.turn(1)
	if !second.StartNew || second.ForeignSID != "" {
		t.Fatalf("second ForeignTurn = {StartNew:%t ForeignSID:%q}, want true/empty", second.StartNew, second.ForeignSID)
	}
	shutdown(t, l)
}

func TestLateBoundTerminalErrorBeforeInitPreservesResultAndProtocolErrors(t *testing.T) {
	t.Parallel()
	agent := &scriptedCloseAgent{scripts: []streamScript{
		{events: []ForeignEvent{{Kind: ForeignTerminalError, ErrText: "error_max_turns"}, {Kind: ForeignInit, SessionID: "too-late"}}},
		{events: []ForeignEvent{{Kind: ForeignInit, SessionID: "codex-thread-2"}, {Kind: ForeignTerminalOK}}},
	}}
	pub := &fakePublisher{}
	l, _ := newTestLoop(t, Spec{Agent: agent, Cwd: t.TempDir(), SIDMode: SIDLateBound}, pub)

	submitUserInput(t, l, "first")
	waitForKind(t, pub, "TurnFailed")
	waitLoopIdle(t, l)
	if got, want := eventKinds(pub.snapshot()), []string{"TurnStarted", "TurnFailed"}; !eqStrs(got, want) {
		t.Fatalf("first-turn published sequence = %v, want %v", got, want)
	}

	turnErr := findTurnFailed(t, pub).Err
	var resultErr *ForeignResultError
	if !errors.As(turnErr, &resultErr) {
		t.Fatalf("TurnFailed.Err = %T %v, want ForeignResultError", turnErr, turnErr)
	}
	var protocolErr *ForeignProtocolError
	if !errors.As(turnErr, &protocolErr) {
		t.Fatalf("TurnFailed.Err = %T %v, want ForeignProtocolError", turnErr, turnErr)
	}
	_, turnIndex, err := l.Snapshot(context.Background())
	if err != nil || turnIndex != 0 {
		t.Fatalf("Snapshot turnIndex/error = %d/%v, want 0/nil", turnIndex, err)
	}

	submitUserInput(t, l, "second")
	waitTurnIndex(t, l, 1)
	second := agent.turn(1)
	if !second.StartNew || second.ForeignSID != "" {
		t.Fatalf("second ForeignTurn = {StartNew:%t ForeignSID:%q}, want true/empty", second.StartNew, second.ForeignSID)
	}
	shutdown(t, l)
}

func TestPreboundTerminalWithoutInitSucceeds(t *testing.T) {
	t.Parallel()
	agent := &scriptedCloseAgent{scripts: []streamScript{{events: []ForeignEvent{{Kind: ForeignTerminalOK}}}}}
	pub := &fakePublisher{}
	l, _ := newTestLoop(t, Spec{Agent: agent}, pub)

	submitUserInput(t, l, "go")
	waitTurnIndex(t, l, 1)

	if got, want := eventKinds(pub.snapshot()), []string{"TurnStarted", "TurnDone"}; !eqStrs(got, want) {
		t.Fatalf("published sequence = %v, want %v", got, want)
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

func TestLateBoundInterruptedFirstTurnResumesBoundSession(t *testing.T) {
	t.Parallel()
	agent := &fakeAgent{
		transcript: filepath.Join(t.TempDir(), "missing.jsonl"),
		block:      true,
		events: []ForeignEvent{
			{Kind: ForeignInit, SessionID: "codex-thread-1"},
		},
	}
	pub := &fakePublisher{}
	l, sid := newTestLoop(t, Spec{
		Agent:   agent,
		Cwd:     t.TempDir(),
		SIDMode: SIDLateBound,
	}, pub)
	if sid != "" {
		t.Fatalf("initial sid = %q, want empty for late-bound loop", sid)
	}

	submitUserInput(t, l, "first")
	waitForKind(t, pub, "ForeignSessionBound")

	sendInterrupt(t, l)
	waitForKind(t, pub, "TurnInterrupted")

	agent.mu.Lock()
	agent.block = false
	agent.events = []ForeignEvent{
		{Kind: ForeignInit, SessionID: "codex-thread-1"},
		{Kind: ForeignStepComplete, Message: aiMessage("ok")},
		{Kind: ForeignTerminalOK},
	}
	agent.mu.Unlock()

	submitUserInput(t, l, "second")
	waitTurnIndex(t, l, 1)

	ft := agent.lastForeignTurn()
	if ft.StartNew {
		t.Fatal("second ForeignTurn.StartNew = true, want false")
	}
	if ft.ForeignSID != "codex-thread-1" {
		t.Fatalf("second ForeignTurn.ForeignSID = %q, want codex-thread-1", ft.ForeignSID)
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
