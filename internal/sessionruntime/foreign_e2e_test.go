package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreignloop"
	"github.com/looprig/harness/pkg/loop"
)

// This file is the end-to-end proof that the REAL foreign actor (wired through the
// session via foreignloop.BuildWith) runs both as the PRIMARY loop and as a SUBAGENT
// under a native primary, driven by a FAKE ForeignAgent. The foreignloop package's own
// fakes are unexported, so these fakes are defined fresh against the EXPORTED interfaces
// (ForeignAgent / ForeignStream / ForeignEvent / Spec).

// aiMsg builds a one-text-block assistant message — the authoritative round content a
// foreign stream hands back on ForeignStepComplete / ForeignTerminalOK.
func aiMsg(s string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: s}},
	}}
}

// foreignScript is a deterministic stream that ends in a text result: an init, a live
// text delta (mapped to an Ephemeral TokenDelta), one authoritative assistant round,
// and a clean terminal. ForeignTerminalOK carries NO message so the turn collects
// EXACTLY ONE assistant message (matching the package's TestTranscriptLossSoftDegrade
// convention): the soft-degrade path then commits a single synthetic StepDone and the
// terminal's TurnDone.Message is that same round.
func foreignScript(text string) []foreignloop.ForeignEvent {
	return []foreignloop.ForeignEvent{
		{Kind: foreignloop.ForeignInit},
		{Kind: foreignloop.ForeignTextDelta, Text: text},
		{Kind: foreignloop.ForeignStepComplete, Message: aiMsg(text)},
		{Kind: foreignloop.ForeignTerminalOK},
	}
}

func foreignLateBoundScript(sid, text string) []foreignloop.ForeignEvent {
	return []foreignloop.ForeignEvent{
		{Kind: foreignloop.ForeignInit, SessionID: sid},
		{Kind: foreignloop.ForeignTextDelta, Text: text},
		{Kind: foreignloop.ForeignStepComplete, Message: aiMsg(text)},
		{Kind: foreignloop.ForeignTerminalOK},
	}
}

// fakeForeignStream is a scripted foreignloop.ForeignStream. It feeds its events on
// Events() from a goroutine (honoring the spawn/turn ctx and Close), reports a fixed
// transcript path, and Close is an idempotent no-op returning nil. The transcript path
// points at a NON-EXISTENT file so the actor takes the documented soft-degrade commit
// path (empty "" would NOT: os.Open(".") succeeds and yields no StepDone).
type fakeForeignStream struct {
	events     []foreignloop.ForeignEvent
	transcript string
	ctx        context.Context

	ch        chan foreignloop.ForeignEvent
	stop      chan struct{}
	once      sync.Once
	closeOnce sync.Once
}

func (s *fakeForeignStream) Events() <-chan foreignloop.ForeignEvent {
	s.once.Do(func() {
		s.ch = make(chan foreignloop.ForeignEvent)
		go s.feed()
	})
	return s.ch
}

func (s *fakeForeignStream) feed() {
	defer close(s.ch)
	for _, fe := range s.events {
		select {
		case s.ch <- fe:
		case <-s.stop:
			return
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *fakeForeignStream) TranscriptPath() string { return s.transcript }

func (s *fakeForeignStream) Close() error {
	s.closeOnce.Do(func() { close(s.stop) })
	return nil
}

// fakeForeignAgent implements foreignloop.ForeignAgent. It returns a fresh scripted
// stream per Spawn and records the ForeignTurn it received (so a test can assert
// StartNew / ForeignSID). It mints no process — the whole foreign turn is in-memory.
type fakeForeignAgent struct {
	mu         sync.Mutex
	transcript string
	script     []foreignloop.ForeignEvent

	spawnCalls int
	lastTurn   foreignloop.ForeignTurn
}

func (a *fakeForeignAgent) Spawn(ctx context.Context, t foreignloop.ForeignTurn) (foreignloop.ForeignStream, error) {
	a.mu.Lock()
	a.spawnCalls++
	a.lastTurn = t
	a.mu.Unlock()
	return &fakeForeignStream{
		events:     a.script,
		transcript: a.transcript,
		ctx:        ctx,
		stop:       make(chan struct{}),
	}, nil
}

// foreignTurn returns the most recent ForeignTurn passed to Spawn, read under the mutex.
func (a *fakeForeignAgent) foreignTurn() foreignloop.ForeignTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastTurn
}

// missingTranscript returns a path under t.TempDir() that is guaranteed not to exist, so
// the foreign actor's transcript decode fails with TranscriptUnavailableError and it
// soft-degrades to a synthetic StepDone from the stream-accumulated assistant message.
func missingTranscript(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "no-transcript.jsonl")
}

// foreignEventKind names a turn-lifecycle event for an ordered-sequence assertion.
func foreignEventKind(ev event.Event) string {
	switch ev.(type) {
	case event.TurnStarted:
		return "TurnStarted"
	case event.ForeignSessionBound:
		return "ForeignSessionBound"
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

// drainForeignTurn reads sub until a turn terminal (TurnDone/TurnFailed/TurnInterrupted)
// arrives, returning the turn-LIFECYCLE events seen so the caller can assert the
// sequence. Session-scoped quiescence transitions (SessionActive/SessionIdle) also ride
// an Enduring subscription, so they are skipped — the assertion is about the foreign
// loop's turn shape. The foreign loop emits no LoopIdle, so the turn is drained to its
// terminal, never to a quiescence signal.
func drainForeignTurn(t *testing.T, sub interface {
	Events() <-chan event.Delivery
}) []event.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	var out []event.Event
	for {
		select {
		case d, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed before a turn terminal; got %v", foreignKinds(out))
			}
			ev := d.Event
			if !isForeignTurnEvent(ev) {
				continue // skip session-scoped quiescence transitions on the fan-in.
			}
			out = append(out, ev)
			switch ev.(type) {
			case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
				return out
			}
		case <-deadline:
			t.Fatalf("timed out draining foreign turn; got %v", foreignKinds(out))
		}
	}
}

// isForeignTurnEvent reports whether ev is one of the foreign loop's turn-lifecycle
// events (the sequence under test), excluding session-scoped quiescence transitions.
func isForeignTurnEvent(ev event.Event) bool {
	switch ev.(type) {
	case event.TurnStarted, event.ForeignSessionBound, event.StepDone, event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		return true
	default:
		return false
	}
}

func foreignKinds(evs []event.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = foreignEventKind(ev)
	}
	return out
}

// foreignSubLoopStarted scans the durable tap for the LoopStarted of the loop that is
// NOT the primary — the foreign subagent the test just ran.
func foreignSubLoopStarted(r *recordingEventAppender, primary uuid.UUID) (event.LoopStarted, bool) {
	for _, ev := range r.snapshot() {
		if ls, ok := ev.(event.LoopStarted); ok && ls.Coordinates.LoopID != primary {
			return ls, true
		}
	}
	return event.LoopStarted{}, false
}

// foreignPrimaryCfg builds a foreign-engine loop.Config for a primary loop. cfg() seeds a
// usable native cfg; the foreign engine ignores Client, but System is required by
// the foreign actor's wiring validation.
func foreignPrimaryCfg() loop.Definition {
	return engineCfg(&stubLLM{chunks: []content.Chunk{textChunk("unused")}}, loop.EngineForeignClaude, "sys")
}

// foreignSubCfg builds the FRESH foreign-engine cfg a RunSubagent call passes for its
// sub-loop (a foreign loop needs only Engine + System).
func foreignSubCfg() loop.Definition {
	return engineCfg(&stubLLM{}, loop.EngineForeignClaude, "sys")
}

func codexForeignPrimaryCfg() loop.Definition {
	return engineCfg(&stubLLM{}, loop.EngineForeignCodex, "sys")
}

func codexForeignSubCfg() loop.Definition {
	return engineCfg(&stubLLM{}, loop.EngineForeignCodex, "sys")
}

// TestForeignPrimaryE2E runs the REAL foreign actor as the session PRIMARY: New mints a
// unique foreign sid (so the (sid,cwd) liveness lock never collides), stamps it onto the
// primary LoopStarted, and a Submit drives one foreign turn to its TurnDone terminal —
// observed on an Enduring subscription scoped to the primary loop.
func TestForeignPrimaryE2E(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rec := &recordingEventAppender{}
	agent := &fakeForeignAgent{transcript: missingTranscript(t), script: foreignScript("result text")}
	spec := foreignloop.Spec{Agent: agent, Cwd: t.TempDir()}

	s, err := New(ctx, foreignPrimaryCfg(),
		WithForeignBuilder(foreignloop.BuildWith(spec), foreignloop.BuildRestoredWith(spec)),
		WithEventAppender(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// The primary LoopStarted carries the minted (non-empty) foreign sid.
	ls, ok := firstLoopStarted(rec)
	if !ok {
		t.Fatal("no LoopStarted captured on the durable tap")
	}
	if ls.ForeignSID == "" {
		t.Fatal("primary LoopStarted.ForeignSID is empty, want the minted foreign sid")
	}

	// Subscribe (Enduring, primary-scoped) BEFORE Submit — the hub has no replay.
	primary := s.PrimaryLoopID()
	sub, err := s.SubscribeEvents(event.EventFilter{
		Enduring: event.LoopScope{Loops: map[uuid.UUID]struct{}{primary: {}}},
	})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	submitID, err := s.Submit(ctx, textBlocks("go"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	evs := drainForeignTurn(t, sub)
	want := []string{"TurnStarted", "StepDone", "TurnDone"}
	if got := foreignKinds(evs); !equalStrs(got, want) {
		t.Fatalf("primary enduring sequence = %v, want %v", got, want)
	}

	ts, ok := evs[0].(event.TurnStarted)
	if !ok {
		t.Fatalf("evs[0] = %T, want TurnStarted", evs[0])
	}
	if ts.Cause.CommandID != submitID {
		t.Errorf("TurnStarted.Cause.CommandID = %v, want submit id %v", ts.Cause.CommandID, submitID)
	}
	td, ok := evs[2].(event.TurnDone)
	if !ok {
		t.Fatalf("evs[2] = %T, want TurnDone", evs[2])
	}
	if got := aiText(td.Message); got != "result text" {
		t.Errorf("TurnDone.Message text = %q, want %q", got, "result text")
	}

	// The first turn started a NEW foreign session bound to the minted sid.
	ft := agent.foreignTurn()
	if !ft.StartNew {
		t.Error("ForeignTurn.StartNew = false, want true on the first turn")
	}
	if ft.ForeignSID != ls.ForeignSID {
		t.Errorf("ForeignTurn.ForeignSID = %q, want the minted sid %q", ft.ForeignSID, ls.ForeignSID)
	}
}

func TestCodexForeignPrimaryLateBoundPublishesBoundAndTurnDone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const boundSID = "codex-thread-primary-1"
	rec := &recordingEventAppender{}
	agent := &fakeForeignAgent{transcript: missingTranscript(t), script: foreignLateBoundScript(boundSID, "codex result")}
	spec := foreignloop.Spec{Agent: agent, Cwd: t.TempDir(), SIDMode: foreignloop.SIDLateBound}

	s, err := New(ctx, codexForeignPrimaryCfg(),
		WithForeignBuilder(foreignloop.BuildWith(spec), foreignloop.BuildRestoredWith(spec)),
		WithEventAppender(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	ls, ok := firstLoopStarted(rec)
	if !ok {
		t.Fatal("no LoopStarted captured on the durable tap")
	}
	if ls.ForeignSID != "" {
		t.Fatalf("primary LoopStarted.ForeignSID = %q, want empty for late-bound Codex", ls.ForeignSID)
	}

	primary := s.PrimaryLoopID()
	sub, err := s.SubscribeEvents(event.EventFilter{
		Enduring: event.LoopScope{Loops: map[uuid.UUID]struct{}{primary: {}}},
	})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	submitID, err := s.Submit(ctx, textBlocks("go"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	evs := drainForeignTurn(t, sub)
	want := []string{"TurnStarted", "ForeignSessionBound", "StepDone", "TurnDone"}
	if got := foreignKinds(evs); !equalStrs(got, want) {
		t.Fatalf("primary enduring sequence = %v, want %v", got, want)
	}
	ts, ok := evs[0].(event.TurnStarted)
	if !ok {
		t.Fatalf("evs[0] = %T, want TurnStarted", evs[0])
	}
	if ts.Cause.CommandID != submitID {
		t.Errorf("TurnStarted.Cause.CommandID = %v, want submit id %v", ts.Cause.CommandID, submitID)
	}
	bound, ok := evs[1].(event.ForeignSessionBound)
	if !ok {
		t.Fatalf("evs[1] = %T, want ForeignSessionBound", evs[1])
	}
	if bound.ForeignSID != boundSID {
		t.Errorf("ForeignSessionBound.ForeignSID = %q, want %q", bound.ForeignSID, boundSID)
	}
	td, ok := evs[3].(event.TurnDone)
	if !ok {
		t.Fatalf("evs[3] = %T, want TurnDone", evs[3])
	}
	if got := aiText(td.Message); got != "codex result" {
		t.Errorf("TurnDone.Message text = %q, want %q", got, "codex result")
	}

	ft := agent.foreignTurn()
	if !ft.StartNew {
		t.Error("ForeignTurn.StartNew = false, want true on the first late-bound Codex turn")
	}
	if ft.ForeignSID != "" {
		t.Errorf("ForeignTurn.ForeignSID = %q, want empty before Codex ForeignInit binds", ft.ForeignSID)
	}
}

// TestForeignSubagentE2E runs a foreign loop as a SUBAGENT under a NATIVE primary:
// RunSubagent creates the foreign sub-loop, drives one turn, and returns the foreign
// TurnDone.Message text. It also proves lineage — the sub-loop's LoopStarted is caused
// by the parent (primary) loop, carries the parent tool-use id, and is bound to a
// non-empty foreign sid.
func TestForeignSubagentE2E(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rec := &recordingEventAppender{}
	agent := &fakeForeignAgent{transcript: missingTranscript(t), script: foreignScript("subagent says hi")}
	spec := foreignloop.Spec{Agent: agent, Cwd: t.TempDir()}

	// Native primary (Engine zero) with a working native cfg + stub LLM.
	s, err := New(ctx, cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}),
		WithForeignBuilder(foreignloop.BuildWith(spec), foreignloop.BuildRestoredWith(spec)),
		WithEventAppender(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	final, err := s.RunSubagent(ctx, loop.Provenance{LoopID: s.PrimaryLoopID()},
		foreignSubCfg(), textBlocks("hi"), "tool-use-1")
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if final != "subagent says hi" {
		t.Errorf("RunSubagent final text = %q, want %q", final, "subagent says hi")
	}

	// Lineage/provenance on the sub-loop's LoopStarted.
	subLS, ok := foreignSubLoopStarted(rec, s.PrimaryLoopID())
	if !ok {
		t.Fatal("no LoopStarted captured for the foreign sub-loop")
	}
	if subLS.Cause.LoopID != s.PrimaryLoopID() {
		t.Errorf("sub-loop LoopStarted.Cause.LoopID = %v, want parent (primary) %v", subLS.Cause.LoopID, s.PrimaryLoopID())
	}
	if subLS.ParentToolUseID != "tool-use-1" {
		t.Errorf("sub-loop LoopStarted.ParentToolUseID = %q, want %q", subLS.ParentToolUseID, "tool-use-1")
	}
	if subLS.ForeignSID == "" {
		t.Error("sub-loop LoopStarted.ForeignSID is empty, want the minted foreign sid")
	}
}

func TestCodexForeignSubagentLateBoundReturnsFinalText(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const boundSID = "codex-thread-subagent-1"
	rec := &recordingEventAppender{}
	agent := &fakeForeignAgent{transcript: missingTranscript(t), script: foreignLateBoundScript(boundSID, "codex subagent final")}
	spec := foreignloop.Spec{Agent: agent, Cwd: t.TempDir(), SIDMode: foreignloop.SIDLateBound}

	s, err := New(ctx, cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}),
		WithForeignBuilder(foreignloop.BuildWith(spec), foreignloop.BuildRestoredWith(spec)),
		WithEventAppender(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	final, err := s.RunSubagent(ctx, loop.Provenance{LoopID: s.PrimaryLoopID()},
		codexForeignSubCfg(), textBlocks("hi"), "tool-use-codex")
	if err != nil {
		t.Fatalf("RunSubagent: %v", err)
	}
	if final != "codex subagent final" {
		t.Errorf("RunSubagent final text = %q, want %q", final, "codex subagent final")
	}

	subLS, ok := foreignSubLoopStarted(rec, s.PrimaryLoopID())
	if !ok {
		t.Fatal("no LoopStarted captured for the Codex foreign sub-loop")
	}
	if subLS.ForeignSID != "" {
		t.Errorf("sub-loop LoopStarted.ForeignSID = %q, want empty for late-bound Codex", subLS.ForeignSID)
	}
	if subLS.ParentToolUseID != "tool-use-codex" {
		t.Errorf("sub-loop LoopStarted.ParentToolUseID = %q, want %q", subLS.ParentToolUseID, "tool-use-codex")
	}

	ft := agent.foreignTurn()
	if !ft.StartNew {
		t.Error("ForeignTurn.StartNew = false, want true on the first late-bound Codex subagent turn")
	}
	if ft.ForeignSID != "" {
		t.Errorf("ForeignTurn.ForeignSID = %q, want empty before Codex ForeignInit binds", ft.ForeignSID)
	}

	var gotBound bool
	for _, ev := range rec.snapshot() {
		fb, ok := ev.(event.ForeignSessionBound)
		if !ok {
			continue
		}
		if fb.EventHeader().Coordinates.LoopID == subLS.Coordinates.LoopID && fb.ForeignSID == boundSID {
			gotBound = true
			break
		}
	}
	if !gotBound {
		t.Fatalf("no ForeignSessionBound{%q} captured for the Codex sub-loop", boundSID)
	}
}

// TestForeignSubagentQuotaCap proves a foreign subagent counts as one leaf loop against
// the unchanged spawn-quota machinery: with Quota=1, the first foreign RunSubagent
// succeeds and the second is refused with a typed *SessionError{SessionLoopQuotaExceeded}.
func TestForeignSubagentQuotaCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	agent := &fakeForeignAgent{transcript: missingTranscript(t), script: foreignScript("ok")}
	spec := foreignloop.Spec{Agent: agent, Cwd: t.TempDir()}

	s, err := New(ctx, cfg(&stubLLM{chunks: []content.Chunk{textChunk("primary")}}),
		WithForeignBuilder(foreignloop.BuildWith(spec), foreignloop.BuildRestoredWith(spec)),
		WithLimits(Limits{Quota: 1}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	parent := loop.Provenance{LoopID: s.PrimaryLoopID()}

	// First foreign subagent fits the quota of one leaf loop.
	if _, err := s.RunSubagent(ctx, parent, foreignSubCfg(), textBlocks("first"), "tu-1"); err != nil {
		t.Fatalf("first foreign RunSubagent: %v", err)
	}

	// The second spawn exceeds the quota — refused before any second foreign loop starts.
	_, err = s.RunSubagent(ctx, parent, foreignSubCfg(), textBlocks("second"), "tu-2")
	var se *SessionError
	if !errors.As(err, &se) || se.Kind != SessionLoopQuotaExceeded {
		t.Fatalf("second foreign RunSubagent err = %v, want *SessionError{SessionLoopQuotaExceeded}", err)
	}
}

// equalStrs reports whether two string slices are element-wise equal.
func equalStrs(a, b []string) bool {
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
