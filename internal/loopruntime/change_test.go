package loopruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
)

// swapTestModel returns a second VALID model with a distinct name so a mode/inference
// swap is observable in the recorded request.
func swapTestModel(name string) inference.Model {
	m := testModel()
	m.Name = name
	return m
}

// modeDefinition builds a bound definition with a base model + three modes (plan/build/
// swap) so the change tests can observe effort, system, and model changes per mode. The
// recording client captures each turn's request.
func modeDefinition(t *testing.T, client inference.Client) loop.BoundDefinition {
	t.Helper()
	d, err := loop.Define(
		loop.WithName("agent"),
		loop.WithInference(client, testModel()),
		loop.WithSystem("base"),
		loop.WithModes(
			loop.Mode{Name: "plan", Effort: inference.EffortLow, Instructions: "plan-i"},
			loop.Mode{Name: "build", Effort: inference.EffortHigh, Instructions: "build-i"},
			loop.Mode{Name: "swap", Model: swapTestModel("swap-model")},
		),
		loop.WithInitialMode("plan"),
	)
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	bound, err := d.Bind(context.Background(), tool.Bindings{SessionID: mustID(t), LoopID: mustID(t)})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return bound
}

// newBoundLoop starts a loop from a bound definition (so the actor holds the modes it
// validates against), wired to a recording publisher and a client the caller controls.
func newBoundLoop(t *testing.T, client inference.Client, bound loop.BoundDefinition) (*Loop, *recordingPublisher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := &recordingPublisher{}
	l, err := New(ctx, mustID(t), mustID(t), Provenance{}, rec, bound)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l, rec
}

// runOneTurn submits a user turn and blocks until its TurnDone is observed, so the
// request the client recorded for that turn is complete and the loop is back at a turn
// boundary before the next command.
func runOneTurn(t *testing.T, l *Loop, rec *recordingPublisher, text string) {
	t.Helper()
	before := countTurnsDone(rec.events())
	id := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: []content.Block{&content.TextBlock{Text: text}}}
	blockUntilEvents(t, rec, func(evs []event.Event) bool { return countTurnsDone(evs) > before })
}

func countTurnsDone(evs []event.Event) int {
	n := 0
	for _, e := range evs {
		if _, ok := e.(event.TurnDone); ok {
			n++
		}
	}
	return n
}

func sendSetMode(t *testing.T, l *Loop, mode string) command.LoopChangeResult {
	t.Helper()
	ack := make(chan command.LoopChangeResult, 1)
	if !sendCmd(t, l, command.SetLoopMode{Header: command.Header{CommandID: mustID(t)}, Mode: mode, Ack: ack}) {
		t.Fatal("SetLoopMode send did not land (loop exited)")
	}
	select {
	case res := <-ack:
		return res
	case <-l.Done:
		t.Fatal("loop exited before SetLoopMode ack")
	case <-time.After(2 * time.Second):
		t.Fatal("SetLoopMode ack timeout")
	}
	return command.LoopChangeResult{}
}

func sendChange(t *testing.T, l *Loop, c command.ChangeLoopInference) command.LoopChangeResult {
	t.Helper()
	c.Header = command.Header{CommandID: mustID(t)}
	ack := make(chan command.LoopChangeResult, 1)
	c.Ack = ack
	if !sendCmd(t, l, c) {
		t.Fatal("ChangeLoopInference send did not land (loop exited)")
	}
	select {
	case res := <-ack:
		return res
	case <-l.Done:
		t.Fatal("loop exited before ChangeLoopInference ack")
	case <-time.After(2 * time.Second):
		t.Fatal("ChangeLoopInference ack timeout")
	}
	return command.LoopChangeResult{}
}

func hasModeChanged(evs []event.Event, prev, next string) bool {
	for _, e := range evs {
		if mc, ok := e.(event.LoopModeChanged); ok && mc.PreviousMode == prev && mc.Mode == next {
			return true
		}
	}
	return false
}

func countModeChanged(evs []event.Event) int {
	n := 0
	for _, e := range evs {
		if _, ok := e.(event.LoopModeChanged); ok {
			n++
		}
	}
	return n
}

func countInferenceChanged(evs []event.Event) int {
	n := 0
	for _, e := range evs {
		if _, ok := e.(event.LoopInferenceChanged); ok {
			n++
		}
	}
	return n
}

// TestSetModeAppliesAtNextTurnBoundary proves a SetLoopMode: (1) takes effect only on the
// NEXT turn (turn 1 runs under the initial mode, turn 2 under the new mode), (2) emits
// LoopModeChanged with the previous+next names, and (3) preserves the loop id + committed
// history + turn numbering across the change.
func TestSetModeAppliesAtNextTurnBoundary(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := modeDefinition(t, llm)
	l, rec := newBoundLoop(t, llm, bound)

	runOneTurn(t, l, rec, "turn1")
	if got := llm.lastReq(); got.Model.Sampling.Effort != inference.EffortLow || got.System != "base\n\nplan-i" {
		t.Fatalf("turn1 request effort/system = %q/%q, want low/%q", got.Model.Sampling.Effort, got.System, "base\n\nplan-i")
	}

	res := sendSetMode(t, l, "build")
	if res.Err != nil {
		t.Fatalf("SetLoopMode(build) err = %v", res.Err)
	}
	if res.Mode != "build" {
		t.Fatalf("result mode = %q, want build", res.Mode)
	}
	blockUntilEvents(t, rec, func(evs []event.Event) bool { return hasModeChanged(evs, "plan", "build") })

	runOneTurn(t, l, rec, "turn2")
	if got := llm.lastReq(); got.Model.Sampling.Effort != inference.EffortHigh || got.System != "base\n\nbuild-i" {
		t.Fatalf("turn2 request effort/system = %q/%q, want high/%q", got.Model.Sampling.Effort, got.System, "base\n\nbuild-i")
	}

	// Identity + history preserved: turnIndex advanced to 2, both user turns committed.
	msgs, idx, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if idx != 2 {
		t.Fatalf("turnIndex = %d, want 2 (history preserved across mode change)", idx)
	}
	if len(msgs) == 0 {
		t.Fatal("committed history empty after mode change")
	}
}

// TestSetModeSwapModel proves a mode carrying a distinct model swaps the request model.
func TestSetModeSwapModel(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := modeDefinition(t, llm)
	l, rec := newBoundLoop(t, llm, bound)

	runOneTurn(t, l, rec, "turn1")
	if got := llm.lastReq().Model.Name; got != "m" {
		t.Fatalf("turn1 model = %q, want m", got)
	}
	if res := sendSetMode(t, l, "swap"); res.Err != nil {
		t.Fatalf("SetLoopMode(swap) err = %v", res.Err)
	}
	runOneTurn(t, l, rec, "turn2")
	if got := llm.lastReq().Model.Name; got != "swap-model" {
		t.Fatalf("turn2 model = %q, want swap-model", got)
	}
}

// TestSetModeResolvesByExactName proves the change path resolves the target mode by EXACT
// name (no ""→initial remap): SetMode("") reaches the reachable BASE mode (base system/
// model/effort) with a matching "" label and event, named modes select themselves, and an
// unknown name is a typed ChangeInvalidMode. The committed label, the emitted event, and the
// applied request config are mutually consistent for every case.
func TestSetModeResolvesByExactName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mode       string
		wantErr    bool
		wantModel  string
		wantEffort inference.Effort
		wantSystem string
	}{
		{name: "base mode via empty name", mode: "", wantModel: "m", wantEffort: inference.EffortNone, wantSystem: "base"},
		{name: "plan mode", mode: "plan", wantModel: "m", wantEffort: inference.EffortLow, wantSystem: "base\n\nplan-i"},
		{name: "build mode", mode: "build", wantModel: "m", wantEffort: inference.EffortHigh, wantSystem: "base\n\nbuild-i"},
		{name: "swap mode distinct model", mode: "swap", wantModel: "swap-model", wantEffort: inference.EffortNone, wantSystem: "base"},
		{name: "unknown mode rejected", mode: "nope", wantErr: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
			bound := modeDefinition(t, llm)
			l, rec := newBoundLoop(t, llm, bound)

			res := sendSetMode(t, l, tt.mode)
			if tt.wantErr {
				var ce *loop.ChangeError
				if !errors.As(res.Err, &ce) || ce.Kind != loop.ChangeInvalidMode {
					t.Fatalf("SetMode(%q) err = %v, want ChangeInvalidMode", tt.mode, res.Err)
				}
				return
			}
			if res.Err != nil {
				t.Fatalf("SetMode(%q) err = %v", tt.mode, res.Err)
			}
			if res.Mode != tt.mode {
				t.Fatalf("committed mode label = %q, want %q", res.Mode, tt.mode)
			}
			// The emitted event's label matches the committed selection.
			blockUntilEvents(t, rec, func(evs []event.Event) bool {
				for _, e := range evs {
					if mc, ok := e.(event.LoopModeChanged); ok && mc.Mode == tt.mode {
						return true
					}
				}
				return false
			})
			// The applied config the next turn runs under matches the selected mode.
			runOneTurn(t, l, rec, "turn")
			got := llm.lastReq()
			if got.System != tt.wantSystem || got.Model.Name != tt.wantModel || got.Model.Sampling.Effort != tt.wantEffort {
				t.Fatalf("SetMode(%q) request system/model/effort = %q/%q/%q, want %q/%q/%q",
					tt.mode, got.System, got.Model.Name, got.Model.Sampling.Effort, tt.wantSystem, tt.wantModel, tt.wantEffort)
			}
		})
	}
}

// TestChangeAppliedMidTurnDefersToNextTurn proves a SUCCESSFUL inference change that lands
// WHILE a turn is actively running does not disturb the in-flight turn: the running turn
// completes normally, and EVERY request it makes — including a step built AFTER the change
// landed — uses the config captured at turn start. The NEXT turn uses the new config. The
// turn is parked deterministically in a blocking tool (no sleeps).
func TestChangeAppliedMidTurnDefersToNextTurn(t *testing.T) {
	t.Parallel()
	bt := newBlockingTool()
	ts := agenticToolSet([]tool.InvokableTool{bt}, 25, 100)
	client := &scriptedLLM{scripts: [][]content.Chunk{
		{toolUseChunk(0, "id-1", "Block", `{}`)}, // turn 1, step 0: tool call (parks the turn mid-flight)
		{textChunk("done")},                      // turn 1, step 1: text -> TurnDone (built AFTER the change)
		{textChunk("done2")},                     // turn 2, step 0: text -> TurnDone
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rec := &recordingPublisher{}
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, rec,
		runtimeConfig{Client: client, Model: testModel(), Tools: ts, DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}

	// Start turn 1; it parks inside the blocking tool after issuing step 0's request.
	startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "go"}})
	<-bt.started

	// Land a SUCCESSFUL inference change while turn 1 is parked (the ack proves it applied).
	if res := sendChange(t, l, command.ChangeLoopInference{Model: swapTestModel("routed"), SetModel: true, Effort: inference.EffortHigh, SetEffort: true}); res.Err != nil {
		t.Fatalf("mid-turn Change err = %v", res.Err)
	}

	// Release the tool so turn 1 continues to step 1 and completes normally.
	close(bt.release)
	blockUntilEvents(t, rec, func(evs []event.Event) bool { return countTurnsDone(evs) >= 1 })

	// Both of turn 1's requests keep the OLD model/effort captured at turn start — including
	// step 1's request, which was built after the change landed.
	reqs := client.requests()
	if len(reqs) < 2 {
		t.Fatalf("turn 1 made %d LLM calls, want >= 2", len(reqs))
	}
	for i := 0; i < 2; i++ {
		if reqs[i].Model.Name != "m" || reqs[i].Model.Sampling.Effort != inference.EffortNone {
			t.Fatalf("turn1 request[%d] model/effort = %q/%q, want unchanged m/none (in-flight turn keeps captured config)",
				i, reqs[i].Model.Name, reqs[i].Model.Sampling.Effort)
		}
	}

	// The NEXT turn runs under the NEW config.
	runOneTurn(t, l, rec, "turn2")
	last := client.requests()
	got := last[len(last)-1]
	if got.Model.Name != "routed" || got.Model.Sampling.Effort != inference.EffortHigh {
		t.Fatalf("turn2 request model/effort = %q/%q, want routed/high", got.Model.Name, got.Model.Sampling.Effort)
	}
}

// TestChangeMixedBatchAtomicRejection proves a batch mixing one valid and one invalid field
// is rejected atomically: a typed error AND nothing applied (the next turn keeps BOTH the
// old model and the old effort), and no LoopInferenceChanged is emitted.
func TestChangeMixedBatchAtomicRejection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  command.ChangeLoopInference
		kind loop.ChangeErrorKind
	}{
		{
			name: "valid model + invalid effort",
			cmd:  command.ChangeLoopInference{Model: swapTestModel("routed"), SetModel: true, Effort: inference.Effort("turbo"), SetEffort: true},
			kind: loop.ChangeInvalidEffort,
		},
		{
			name: "invalid model + valid effort",
			cmd:  command.ChangeLoopInference{Model: inference.Model{Name: ""}, SetModel: true, Effort: inference.EffortHigh, SetEffort: true},
			kind: loop.ChangeInvalidModel,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
			bound := modeDefinition(t, llm) // initial mode "plan": model m, effort low
			l, rec := newBoundLoop(t, llm, bound)

			res := sendChange(t, l, tt.cmd)
			var ce *loop.ChangeError
			if !errors.As(res.Err, &ce) || ce.Kind != tt.kind {
				t.Fatalf("mixed batch err = %v, want %s", res.Err, tt.kind)
			}
			// Nothing applied: the next turn keeps the OLD model AND the OLD effort.
			runOneTurn(t, l, rec, "turn")
			got := llm.lastReq()
			if got.Model.Name != "m" || got.Model.Sampling.Effort != inference.EffortLow {
				t.Fatalf("after rejected mixed batch model/effort = %q/%q, want unchanged m/low", got.Model.Name, got.Model.Sampling.Effort)
			}
			if countInferenceChanged(rec.events()) != 0 {
				t.Fatalf("LoopInferenceChanged emitted for a rejected mixed batch")
			}
		})
	}
}

// TestSetModeInvalidRejected proves an unknown mode name is refused with a typed
// ChangeInvalidMode, emits no LoopModeChanged, and does not change the effective mode.
func TestSetModeInvalidRejected(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := modeDefinition(t, llm)
	l, rec := newBoundLoop(t, llm, bound)

	res := sendSetMode(t, l, "nope")
	var ce *loop.ChangeError
	if !errors.As(res.Err, &ce) || ce.Kind != loop.ChangeInvalidMode {
		t.Fatalf("SetLoopMode(nope) err = %v, want ChangeInvalidMode", res.Err)
	}
	// No event emitted, and the next turn still runs under the initial (plan) mode.
	runOneTurn(t, l, rec, "turn1")
	if countModeChanged(rec.events()) != 0 {
		t.Fatalf("LoopModeChanged emitted for an invalid mode")
	}
	if got := llm.lastReq(); got.System != "base\n\nplan-i" {
		t.Fatalf("after invalid mode, request system = %q, want unchanged plan", got.System)
	}
}

// TestChangeInferenceAppliesAtNextTurnBoundary proves a Change alters ONLY model+effort at
// the next turn boundary, emits LoopInferenceChanged, and leaves the mode's system intact.
func TestChangeInferenceAppliesAtNextTurnBoundary(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := modeDefinition(t, llm)
	l, rec := newBoundLoop(t, llm, bound)

	runOneTurn(t, l, rec, "turn1")

	res := sendChange(t, l, command.ChangeLoopInference{Model: swapTestModel("routed"), SetModel: true, Effort: inference.EffortHigh, SetEffort: true})
	if res.Err != nil {
		t.Fatalf("Change err = %v", res.Err)
	}
	if res.Model.Name != "routed" || res.Effort != inference.EffortHigh {
		t.Fatalf("result = %q/%q, want routed/high", res.Model.Name, res.Effort)
	}
	blockUntilEvents(t, rec, func(evs []event.Event) bool { return countInferenceChanged(evs) == 1 })

	runOneTurn(t, l, rec, "turn2")
	got := llm.lastReq()
	if got.Model.Name != "routed" || got.Model.Sampling.Effort != inference.EffortHigh {
		t.Fatalf("turn2 model/effort = %q/%q, want routed/high", got.Model.Name, got.Model.Sampling.Effort)
	}
	// Mode (system prompt) unchanged by a direct inference change.
	if got.System != "base\n\nplan-i" {
		t.Fatalf("turn2 system = %q, want unchanged plan (inference change must not touch mode)", got.System)
	}
}

// TestChangeInferenceEffortOnlyKeepsModel proves an effort-only change keeps the current
// model and only overrides effort.
func TestChangeInferenceEffortOnlyKeepsModel(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	bound := modeDefinition(t, llm)
	l, rec := newBoundLoop(t, llm, bound)

	if res := sendChange(t, l, command.ChangeLoopInference{Effort: inference.EffortMax, SetEffort: true}); res.Err != nil {
		t.Fatalf("Change(effort) err = %v", res.Err)
	}
	runOneTurn(t, l, rec, "turn1")
	got := llm.lastReq()
	if got.Model.Name != "m" || got.Model.Sampling.Effort != inference.EffortMax {
		t.Fatalf("model/effort = %q/%q, want m/max", got.Model.Name, got.Model.Sampling.Effort)
	}
}

// TestChangeInferenceValidation proves the batch is validated atomically: an invalid model
// or effort is refused with a typed error, emits no event, and applies nothing.
func TestChangeInferenceValidation(t *testing.T) {
	t.Parallel()
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}

	tests := []struct {
		name string
		cmd  command.ChangeLoopInference
		kind loop.ChangeErrorKind
	}{
		{
			name: "empty model name is invalid",
			cmd:  command.ChangeLoopInference{Model: inference.Model{Name: ""}, SetModel: true},
			kind: loop.ChangeInvalidModel,
		},
		{
			name: "unknown effort is invalid",
			cmd:  command.ChangeLoopInference{Effort: inference.Effort("turbo"), SetEffort: true},
			kind: loop.ChangeInvalidEffort,
		},
		{
			name: "no changes selected",
			cmd:  command.ChangeLoopInference{},
			kind: loop.ChangeNoChanges,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bound := modeDefinition(t, llm)
			l, rec := newBoundLoop(t, llm, bound)
			res := sendChange(t, l, tt.cmd)
			var ce *loop.ChangeError
			if !errors.As(res.Err, &ce) || ce.Kind != tt.kind {
				t.Fatalf("err = %v, want %s", res.Err, tt.kind)
			}
			runOneTurn(t, l, rec, "turn1")
			if countInferenceChanged(rec.events()) != 0 {
				t.Fatalf("LoopInferenceChanged emitted for an invalid change")
			}
		})
	}
}

// faultingPublisher is a recording publisher that ALSO satisfies faultProbe, so the actor
// probes it after emitting a change event. FaultErr returns a fixed fault, simulating a
// required-durable-append failure (which the hub raises inline via ReportFault).
type faultingPublisher struct {
	*recordingPublisher
	fault error
}

func (f *faultingPublisher) FaultErr() error { return f.fault }

// TestChangeNotAppliedOnDurableFault proves the fail-secure post-emit fault check: when the
// change event's durable append faulted the session, the actor replies
// ChangeDurableAppendFailed and does NOT apply the change (the next turn keeps the old
// effort).
func TestChangeNotAppliedOnDurableFault(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fp := &faultingPublisher{recordingPublisher: &recordingPublisher{}, fault: errors.New("durable append failed")}
	llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	l, err := newWithConfig(ctx, mustID(t), mustID(t), Provenance{}, fp, runtimeConfig{Client: llm, Model: testModel(), DrainTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("newWithConfig: %v", err)
	}
	res := sendChange(t, l, command.ChangeLoopInference{Effort: inference.EffortHigh, SetEffort: true})
	var ce *loop.ChangeError
	if !errors.As(res.Err, &ce) || ce.Kind != loop.ChangeDurableAppendFailed {
		t.Fatalf("err = %v, want ChangeDurableAppendFailed", res.Err)
	}
	// The change was NOT applied: the next turn runs under the ORIGINAL (unset) effort.
	runOneTurn(t, l, fp.recordingPublisher, "turn1")
	if got := llm.lastReq().Model.Sampling.Effort; got != inference.EffortNone {
		t.Fatalf("effort after faulted change = %q, want unchanged (none)", got)
	}
}

// TestNewRestoredSeedsModeAndInference proves a restored loop comes up under the
// restore-folded mode (and any direct inference override on top of it): its FIRST turn
// runs under the seeded effective config, not the definition's initial mode.
func TestNewRestoredSeedsModeAndInference(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		seed       RestoredState
		wantSystem string
		wantModel  string
		wantEffort inference.Effort
	}{
		{
			name:       "restored base mode via empty name resolves base, not the initial mode",
			seed:       RestoredState{HasMode: true, Mode: ""},
			wantSystem: "base",
			wantModel:  "m",
			wantEffort: inference.EffortNone,
		},
		{
			name:       "restored mode only",
			seed:       RestoredState{HasMode: true, Mode: "build"},
			wantSystem: "base\n\nbuild-i",
			wantModel:  "m",
			wantEffort: inference.EffortHigh,
		},
		{
			name:       "restored mode plus inference override",
			seed:       RestoredState{HasMode: true, Mode: "plan", HasInference: true, Model: swapTestModel("routed"), Effort: inference.EffortMax},
			wantSystem: "base\n\nplan-i",
			wantModel:  "routed",
			wantEffort: inference.EffortMax,
		},
		{
			name:       "no fold resumes at initial mode",
			seed:       RestoredState{},
			wantSystem: "base\n\nplan-i",
			wantModel:  "m",
			wantEffort: inference.EffortLow,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			llm := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
			bound := modeDefinition(t, llm)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			rec := &recordingPublisher{}
			seed := tt.seed
			if seed.HasInference {
				// The fold bakes effort into the model's Sampling.Effort, mirroring the actor.
				seed.Model.Sampling.Effort = seed.Effort
			}
			l, err := NewRestored(ctx, mustID(t), mustID(t), Provenance{}, rec, bound, seed)
			if err != nil {
				t.Fatalf("NewRestored: %v", err)
			}
			runOneTurn(t, l, rec, "turn1")
			got := llm.lastReq()
			if got.System != tt.wantSystem || got.Model.Name != tt.wantModel || got.Model.Sampling.Effort != tt.wantEffort {
				t.Fatalf("restored turn system/model/effort = %q/%q/%q, want %q/%q/%q",
					got.System, got.Model.Name, got.Model.Sampling.Effort, tt.wantSystem, tt.wantModel, tt.wantEffort)
			}
		})
	}
}

// TestChangeRejectedWhileShuttingDown proves a change is refused with ChangeLoopShuttingDown
// once the loop is shutting down. A provider that ignores ctx keeps a turn "running" so the
// actor stays in loopShuttingDown (waiting to drain) and still serves the command select.
func TestChangeRejectedWhileShuttingDown(t *testing.T) {
	t.Parallel()
	llm := &fakeLLM{blockUntilCancel: true, ignoreCtx: true}
	// A base-model definition with the plan/build modes so SetMode has a target.
	bound := modeDefinition(t, llm)
	l, rec := newBoundLoop(t, llm, bound)

	// Start a turn that hangs in the provider.
	id := mustID(t)
	l.Commands <- command.UserInput{Header: command.Header{CommandID: id}, Blocks: []content.Block{&content.TextBlock{Text: "hang"}}}
	blockUntilEvents(t, rec, func(evs []event.Event) bool {
		for _, e := range evs {
			if _, ok := e.(event.TurnStarted); ok {
				return true
			}
		}
		return false
	})

	// Shutdown: the actor flips to loopShuttingDown and cancels the (ctx-ignoring) turn but
	// keeps selecting on commands while it waits for the turn to drain.
	shutAck := make(chan error, 1)
	l.Commands <- command.Shutdown{Header: command.Header{CommandID: mustID(t)}, Ack: shutAck}

	res := sendSetMode(t, l, "build")
	var ce *loop.ChangeError
	if !errors.As(res.Err, &ce) || ce.Kind != loop.ChangeLoopShuttingDown {
		t.Fatalf("SetLoopMode while shutting down err = %v, want ChangeLoopShuttingDown", res.Err)
	}
	cres := sendChange(t, l, command.ChangeLoopInference{Effort: inference.EffortHigh, SetEffort: true})
	if !errors.As(cres.Err, &ce) || ce.Kind != loop.ChangeLoopShuttingDown {
		t.Fatalf("Change while shutting down err = %v, want ChangeLoopShuttingDown", cres.Err)
	}
}
