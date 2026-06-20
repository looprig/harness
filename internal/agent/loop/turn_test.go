package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// turnRecorder stands in for the actor during runTurn unit tests. It records the
// ordered per-turn stream the way the actor would observe it: cfg.emit events and
// the StepDone delivered through cfg.commit BOTH funnel into `stream` in call
// order, and each commit's Messages are appended to `committed` (the loopState.msgs
// the actor owns). commit is synchronous and never cancelled, mirroring a healthy
// actor; the cancellation path is exercised separately (commitErr / actor tests).
type turnRecorder struct {
	mu        sync.Mutex
	stream    []event.Event           // emit() events + commit StepDone, in order
	committed content.AgenticMessages // groups appended by commit, in order
	commits   []turnCommit
	commitErr error // when non-nil, commit returns it without committing

	// drainBatches is the queue of queuedInput batches drainPending returns, one per
	// drain call (a nil/empty default means "no pending input" — the common case for
	// the multi-step tool tests that do not exercise folding). drainCalls counts the
	// drainPending invocations, and drainErr (when set) makes drainPending return it.
	drainBatches [][]queuedInput
	drainCalls   int
	drainErr     error
}

func (r *turnRecorder) emit(ev event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stream = append(r.stream, ev)
}

func (r *turnRecorder) commit(ctx context.Context, tc turnCommit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.commitErr != nil {
		return r.commitErr
	}
	r.commits = append(r.commits, tc)
	r.committed = append(r.committed, tc.Messages...)
	// The actor emits the StepDone at the commit point, onto the same per-turn
	// stream, AFTER appending. Record that ordering here.
	r.stream = append(r.stream, tc.Event)
	return nil
}

// drainPending mirrors the actor's tool-continuation drain: it returns the next
// queued batch (nil/empty when there is no pending input) and records the call. The
// fixture's default has no batches, so the multi-step tool tests fold nothing.
func (r *turnRecorder) drainPending(ctx context.Context) ([]queuedInput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drainErr != nil {
		return nil, r.drainErr
	}
	i := r.drainCalls
	r.drainCalls++
	if i < len(r.drainBatches) {
		return r.drainBatches[i], nil
	}
	return nil, nil
}

func (r *turnRecorder) events() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.stream...)
}

func (r *turnRecorder) committedMsgs() content.AgenticMessages {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(content.AgenticMessages(nil), r.committed...)
}

// drainEmit appends every emitted event to events; a simple recording emit used
// by step-level tests that do not need the full turnRecorder.
func drainEmit(events *[]event.Event) func(event.Event) {
	return func(ev event.Event) { *events = append(*events, ev) }
}

// stepDones returns the StepDone events from an emitted-event slice, in order.
func stepDones(emitted []event.Event) []event.StepDone {
	var out []event.StepDone
	for _, e := range emitted {
		if sd, ok := e.(event.StepDone); ok {
			out = append(out, sd)
		}
	}
	return out
}

// testIdentity mints a fresh (session, loop, turn) identity for a runTurn call so
// emitted StepDone Headers carry non-zero, consistent ids. It panics on the
// near-impossible crypto/rand failure (acceptable in a test helper).
func testIdentity() turnIdentity {
	must := func() uuid.UUID {
		id, err := uuid.New()
		if err != nil {
			panic(err)
		}
		return id
	}
	return turnIdentity{sessionID: must(), loopID: must(), turnID: must()}
}

// newTurnFixture builds a turnConfig wired to a fresh turnRecorder plus a turnState
// seeded with the input as the initial UserMessage. base is the pre-turn committed
// history clone (the actor commits the initial UserMessage separately; in these
// unit tests the committed view is base + recorder.committed). The committed
// loop history a strict provider would see is committedHistory(base, rec).
func newTurnFixture(input []content.Block, base content.AgenticMessages, ts ToolSet, client llm.LLM, gateReg chan<- gateRegistration) (turnConfig, turnState, *turnRecorder) {
	rec := &turnRecorder{}
	id := testIdentity()
	user := &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: input}}
	st := newTurnState(id.sessionID, id.loopID, id.turnID, 1, uuid.UUID{}, user)
	cfg := turnConfig{
		base:         cloneMessages(base),
		model:        llm.ModelSpec{Model: "m"},
		tools:        ts,
		client:       client,
		gateReg:      gateReg,
		idGen:        uuid.New,
		commit:       rec.commit,
		drainPending: rec.drainPending,
		emit:         rec.emit,
	}
	return cfg, st, rec
}

// committedHistory reconstructs the loop history visible after a turn: the initial
// UserMessage (which the actor commits up front) plus every committed step group.
// In these unit tests base already EXCLUDES the initial UserMessage, so the
// committed view is base + initial user + recorder groups. The fixture seeds the
// user message into turnState; the actor would have appended it to loopState.msgs
// at TurnStarted. We model that by prefixing it here.
func committedHistory(base content.AgenticMessages, user *content.UserMessage, rec *turnRecorder) content.AgenticMessages {
	out := append(content.AgenticMessages(nil), base...)
	out = append(out, user)
	out = append(out, rec.committedMsgs()...)
	return out
}

// ---------------------------------------------------------------------------
// Multi-stream fake LLM: returns a DIFFERENT scripted stream per Stream() call
// (one per agentic iteration), records every request it received, and can be told
// to cancel ctx between/within iterations.
// ---------------------------------------------------------------------------

// scriptedLLM streams scripts[i] on its i-th Stream() call. If more Stream()
// calls arrive than there are scripts, the last script is repeated (so an
// "always calls tools" model is a single tool-call script repeated forever).
// onStreamN, if set for an index, runs at the START of that Stream() call (used
// to cancel ctx mid-loop). It records every request for toolDefs/base assertions.
type scriptedLLM struct {
	mu        sync.Mutex
	scripts   [][]content.Chunk
	reqs      []llm.Request
	calls     int
	onStreamN map[int]func()
}

func (s *scriptedLLM) Invoke(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return nil, errors.New("scriptedLLM.Invoke not used")
}

func (s *scriptedLLM) Stream(ctx context.Context, req llm.Request) (*llm.StreamReader[content.Chunk], error) {
	s.mu.Lock()
	n := s.calls
	s.calls++
	s.reqs = append(s.reqs, req)
	hook := s.onStreamN[n]
	var script []content.Chunk
	if n < len(s.scripts) {
		script = s.scripts[n]
	} else if len(s.scripts) > 0 {
		script = s.scripts[len(s.scripts)-1]
	}
	s.mu.Unlock()

	if hook != nil {
		hook()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	i := 0
	next := func() (content.Chunk, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if i < len(script) {
			c := script[i]
			i++
			return c, nil
		}
		return nil, io.EOF
	}
	return llm.NewStreamReader(next, nil), nil
}

func (s *scriptedLLM) requests() []llm.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Request, len(s.reqs))
	copy(out, s.reqs)
	return out
}

// toolUseChunk builds a single-fragment tool-call delta.
func toolUseChunk(index int, id, name, inputJSON string) content.Chunk {
	return &content.ToolUseChunk{Index: index, ID: id, Name: name, InputJSON: inputJSON}
}

// echoTool is a registered fake tool for the agentic-loop tests: it echoes a
// fixed output and records how many times it ran. It implements only the base
// interface (autoApproveGate handles permission).
type echoTool struct {
	name   string
	output string
	mu     sync.Mutex
	runs   int
}

func (e *echoTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: e.name, Desc: "echoes", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (e *echoTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	e.mu.Lock()
	e.runs++
	e.mu.Unlock()
	return tool.TextResult(e.output), nil
}

func (e *echoTool) runCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs
}

// askUserTool is a registered fake tool that calls RequestUserInput inside its run
// (the way the real AskUser tool does), so a runTurn-level test exercises the
// UserInputRequested gate end-to-end. The runner injects emit/callID/gateReg into
// the tool's ctx, so RequestUserInput finds everything it needs there.
type askUserTool struct {
	name     string
	question string
	choices  []string
}

func (a *askUserTool) Info(ctx context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: a.name, Desc: "asks", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (a *askUserTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	ans, err := RequestUserInput(ctx, a.question, a.choices)
	if err != nil {
		return nil, err
	}
	return tool.TextResult(ans), nil
}

// countToolUseInHistory counts tool_use blocks (in AIMessages) and ToolResultMessages.
// A well-formed committed history has equal counts; a discarded in-flight step
// leaves no unpaired tool_use.
func countToolUseInHistory(msgs content.AgenticMessages) (toolUse, toolMsg int) {
	for _, m := range msgs {
		switch v := m.(type) {
		case *content.AIMessage:
			for _, b := range v.Blocks {
				if _, ok := b.(*content.ToolUseBlock); ok {
					toolUse++
				}
			}
		case *content.ToolResultMessage:
			toolMsg++
		}
	}
	return
}

func agenticToolSet(reg []tool.InvokableTool, maxIters, maxCalls int) ToolSet {
	return resolveToolSetCaps(ToolSet{
		Permission:           autoApproveGate{},
		Registry:             reg,
		MaxToolIterations:    maxIters,
		MaxToolCallsPerTurn:  maxCalls,
		MaxParallelToolCalls: 4,
	})
}

// noGateReg returns a gateReg channel that is never read (no EffectAsk in
// auto-approve scenarios).
func noGateReg() chan gateRegistration { return make(chan gateRegistration) }

// initialUser pulls the initial UserMessage out of a turnState (always msgs[0]).
func initialUser(ts turnState) *content.UserMessage { return ts.msgs[0].(*content.UserMessage) }

// TestToolResultMessage verifies the staged ToolResultMessage carries the result's
// IsError flag (instead of dropping it) along with the flattened text and tool_use
// id, so the message-level error signal survives into committed history.
func TestToolResultMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		in          result
		wantText    string
		wantIsError bool
	}{
		{
			name:        "error result carries IsError true",
			in:          result{ToolUseID: "tu-err", Content: []content.Block{&content.TextBlock{Text: "tool error: boom"}}, IsError: true},
			wantText:    "tool error: boom",
			wantIsError: true,
		},
		{
			name:        "success result carries IsError false",
			in:          result{ToolUseID: "tu-ok", Content: []content.Block{&content.TextBlock{Text: "ran"}}, IsError: false},
			wantText:    "ran",
			wantIsError: false,
		},
		{
			name:        "empty content still pairs by id, IsError preserved",
			in:          result{ToolUseID: "tu-empty", Content: nil, IsError: true},
			wantText:    flattenToText(nil),
			wantIsError: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := toolResultMessage(tt.in)
			if got.ToolUseID != tt.in.ToolUseID {
				t.Errorf("ToolUseID = %q, want %q", got.ToolUseID, tt.in.ToolUseID)
			}
			if got.IsError != tt.wantIsError {
				t.Errorf("IsError = %v, want %v (dropped?)", got.IsError, tt.wantIsError)
			}
			if text := flattenToText(got.Blocks); text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
		})
	}
}

func TestRunTurnAgentic(t *testing.T) {
	t.Parallel()
	input := []content.Block{&content.TextBlock{Text: "hi"}}

	t.Run("one tool round-trip then text-only completes with TurnDone", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "tool ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 25, 100)
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Echo", `{"x":1}`)}, // iter1: one tool call
			{textChunk("all done")},                      // iter2: text-only → TurnDone
		}}
		cfg, st, rec := newTurnFixture(input, nil, ts, client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)

		done, ok := terminal.(event.TurnDone)
		if !ok {
			t.Fatalf("terminal = %T, want TurnDone", terminal)
		}
		if echo.runCount() != 1 {
			t.Errorf("echo ran %d times, want 1", echo.runCount())
		}

		// A multi-step (tool-using) turn commits one step per completed step, each
		// emitting a StepDone. Step 0 is the tool step (group = AIMessage + its
		// ToolResultMessage); step 1 is the final text step (group = just the AIMessage).
		sds := stepDones(rec.events())
		if len(sds) != 2 {
			t.Fatalf("StepDone count = %d, want 2 (one per completed step)", len(sds))
		}
		id := turnIdentity{sessionID: st.sessionID, loopID: st.loopID, turnID: st.id}
		seenStepIDs := map[uuid.UUID]struct{}{}
		for i, sd := range sds {
			if sd.SessionID != id.sessionID || sd.LoopID != id.loopID || sd.TurnID != id.turnID {
				t.Errorf("StepDone[%d] Header ids = (s=%v l=%v t=%v), want (s=%v l=%v t=%v)",
					i, sd.SessionID, sd.LoopID, sd.TurnID, id.sessionID, id.loopID, id.turnID)
			}
			if sd.StepID.IsZero() {
				t.Errorf("StepDone[%d] StepID is zero", i)
			}
			if _, dup := seenStepIDs[sd.StepID]; dup {
				t.Errorf("StepDone[%d] StepID %v duplicated", i, sd.StepID)
			}
			seenStepIDs[sd.StepID] = struct{}{}
		}
		if len(sds[0].Messages) != 2 {
			t.Fatalf("StepDone[0].Messages len = %d, want 2 (AIMessage + ToolResultMessage)", len(sds[0].Messages))
		}
		if _, ok := sds[0].Messages[0].(*content.AIMessage); !ok {
			t.Errorf("StepDone[0].Messages[0] = %T, want *AIMessage", sds[0].Messages[0])
		}
		trm, ok := sds[0].Messages[1].(*content.ToolResultMessage)
		if !ok {
			t.Fatalf("StepDone[0].Messages[1] = %T, want *ToolResultMessage", sds[0].Messages[1])
		}
		if trm.ToolUseID != "id-1" {
			t.Errorf("StepDone[0] tool result ToolUseID = %q, want %q", trm.ToolUseID, "id-1")
		}
		if len(sds[1].Messages) != 1 {
			t.Fatalf("StepDone[1].Messages len = %d, want 1 (final AIMessage only)", len(sds[1].Messages))
		}
		if sds[1].Messages[0] != done.Message {
			t.Errorf("StepDone[1].Messages[0] must be the final assistant message (== TurnDone.Message)")
		}

		// committed history: user, assistant(tool_use), tool message, assistant(text).
		msgs := committedHistory(cfg.base, initialUser(st), rec)
		if len(msgs) != 4 {
			t.Fatalf("committed history len = %d, want 4 (user, assistant tool_use, tool, assistant text)", len(msgs))
		}
		if _, ok := msgs[0].(*content.UserMessage); !ok {
			t.Errorf("msgs[0] = %T, want *UserMessage", msgs[0])
		}
		ai1, ok := msgs[1].(*content.AIMessage)
		if !ok {
			t.Fatalf("msgs[1] = %T, want *AIMessage", msgs[1])
		}
		if _, ok := ai1.Blocks[len(ai1.Blocks)-1].(*content.ToolUseBlock); !ok {
			t.Errorf("msgs[1] last block = %T, want *ToolUseBlock", ai1.Blocks[len(ai1.Blocks)-1])
		}
		tm, ok := msgs[2].(*content.ToolResultMessage)
		if !ok {
			t.Fatalf("msgs[2] = %T, want *ToolResultMessage", msgs[2])
		}
		if tm.ToolUseID != "id-1" {
			t.Errorf("tool message ToolUseID = %q, want %q", tm.ToolUseID, "id-1")
		}
		if got := flattenToText(tm.Blocks); got != "tool ran" {
			t.Errorf("tool message text = %q, want %q", got, "tool ran")
		}
		if _, ok := msgs[3].(*content.AIMessage); !ok {
			t.Errorf("msgs[3] = %T, want *AIMessage", msgs[3])
		}
		if done.Message != msgs[3] {
			t.Errorf("TurnDone.Message must be the final assistant message")
		}
		tu, tmCount := countToolUseInHistory(msgs)
		if tu != tmCount {
			t.Errorf("unpaired tool_use: %d tool_use vs %d tool messages", tu, tmCount)
		}

		// The continuation request must be built from base + staged turn msgs — never
		// duplicating the committed groups. Request 2 (the tool continuation) carries:
		// user, assistant(tool_use), tool message = 3 messages (base is empty here).
		reqs := client.requests()
		if len(reqs) != 2 {
			t.Fatalf("recorded %d requests, want 2", len(reqs))
		}
		if len(reqs[1].Messages) != 3 {
			t.Errorf("continuation request had %d messages, want 3 (user, assistant tool_use, tool)", len(reqs[1].Messages))
		}
	})

	t.Run("tool-step ToolCallStarted/Completed carry the step's StepID (== that step's StepDone)", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "tool ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 25, 100)
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Echo", `{"x":1}`)}, // iter1: one tool call → step 0
			{textChunk("all done")},                      // iter2: text-only → TurnDone (step 1)
		}}
		cfg, st, rec := newTurnFixture(input, nil, ts, client, noGateReg())
		if _, ok := runTurn(context.Background(), cfg, st).(event.TurnDone); !ok {
			t.Fatalf("terminal not TurnDone")
		}

		// Step 0 is the tool step; its StepDone carries the step's StepID. The
		// ToolCallStarted/Completed for that step's batch must carry the SAME StepID
		// (non-zero), satisfying the ToolExecutionID-requires-StepID invariant.
		sds := stepDones(rec.events())
		if len(sds) != 2 {
			t.Fatalf("StepDone count = %d, want 2", len(sds))
		}
		toolStepID := sds[0].StepID
		if toolStepID.IsZero() {
			t.Fatal("tool step's StepDone StepID is zero")
		}

		var started, completed int
		for _, ev := range rec.events() {
			switch e := ev.(type) {
			case event.ToolCallStarted:
				started++
				if e.StepID != toolStepID {
					t.Errorf("ToolCallStarted StepID = %v, want %v (the tool step's StepID)", e.StepID, toolStepID)
				}
				if e.StepID.IsZero() {
					t.Error("ToolCallStarted StepID is zero")
				}
			case event.ToolCallCompleted:
				completed++
				if e.StepID != toolStepID {
					t.Errorf("ToolCallCompleted StepID = %v, want %v (the tool step's StepID)", e.StepID, toolStepID)
				}
			}
		}
		if started != 1 || completed != 1 {
			t.Errorf("events: %d started / %d completed, want 1/1", started, completed)
		}
	})

	t.Run("gate PermissionRequested carries the tool step's StepID", func(t *testing.T) {
		t.Parallel()
		tl := &fakeRunTool{name: "T", output: "ok"}
		pt := promptTool{fakeRunTool: tl}
		tl.promptFn = func(string) (tool.PermissionRequest, error) {
			return tool.UnknownRequest{Tool: "T", Summary: "do"}, nil
		}
		gate := &fakePermissionGate{checkFn: func(string, string) Effect { return EffectAsk }}
		ts := resolveToolSetCaps(ToolSet{
			Permission:           gate,
			Registry:             []tool.InvokableTool{pt},
			MaxToolIterations:    25,
			MaxToolCallsPerTurn:  100,
			MaxParallelToolCalls: 4,
		})
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "T", `{}`)}, // iter1: one gated tool call → step 0
			{textChunk("done")},                  // iter2: text-only → TurnDone
		}}

		gateReg := make(chan gateRegistration)
		// Fake actor: install the gate (close ack), then approve the call.
		go func() {
			reg := <-gateReg
			close(reg.ack)
			reg.reply <- command.ApproveToolCall{GateRoute: command.GateRoute{ToolExecutionID: reg.callID}, Scope: tool.ScopeOnce}
		}()

		cfg, st, rec := newTurnFixture(input, nil, ts, client, gateReg)
		if _, ok := runTurn(context.Background(), cfg, st).(event.TurnDone); !ok {
			t.Fatalf("terminal not TurnDone")
		}

		sds := stepDones(rec.events())
		if len(sds) != 2 {
			t.Fatalf("StepDone count = %d, want 2", len(sds))
		}
		toolStepID := sds[0].StepID
		if toolStepID.IsZero() {
			t.Fatal("tool step's StepDone StepID is zero")
		}

		var nPerm int
		for _, ev := range rec.events() {
			if pr, ok := ev.(event.PermissionRequested); ok {
				nPerm++
				if pr.StepID != toolStepID {
					t.Errorf("PermissionRequested StepID = %v, want %v (the tool step's StepID)", pr.StepID, toolStepID)
				}
			}
		}
		if nPerm != 1 {
			t.Errorf("PermissionRequested count = %d, want 1", nPerm)
		}
	})

	t.Run("gate UserInputRequested carries the tool step's StepID", func(t *testing.T) {
		t.Parallel()
		// askTool calls RequestUserInput inside its run; the runner injects emit +
		// callID + gateReg into the tool's ctx, so this exercises the real AskUser path
		// end-to-end (the same path RequestUserInput tests cover at the unit level).
		ask := &askUserTool{name: "Ask", question: "favorite color?", choices: []string{"red", "blue"}}
		ts := resolveToolSetCaps(ToolSet{
			Permission:           autoApproveGate{},
			Registry:             []tool.InvokableTool{ask},
			MaxToolIterations:    25,
			MaxToolCallsPerTurn:  100,
			MaxParallelToolCalls: 4,
		})
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Ask", `{}`)}, // iter1: one user-input tool call → step 0
			{textChunk("done")},                    // iter2: text-only → TurnDone
		}}

		gateReg := make(chan gateRegistration)
		// Fake actor: install the user-input gate (close ack), then provide the answer.
		go func() {
			reg := <-gateReg
			close(reg.ack)
			reg.reply <- command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: reg.callID}, Answer: "blue"}
		}()

		cfg, st, rec := newTurnFixture(input, nil, ts, client, gateReg)
		if _, ok := runTurn(context.Background(), cfg, st).(event.TurnDone); !ok {
			t.Fatalf("terminal not TurnDone")
		}

		sds := stepDones(rec.events())
		if len(sds) != 2 {
			t.Fatalf("StepDone count = %d, want 2", len(sds))
		}
		toolStepID := sds[0].StepID
		if toolStepID.IsZero() {
			t.Fatal("tool step's StepDone StepID is zero")
		}

		var nReq int
		for _, ev := range rec.events() {
			uir, ok := ev.(event.UserInputRequested)
			if !ok {
				continue
			}
			nReq++
			if uir.StepID != toolStepID {
				t.Errorf("UserInputRequested StepID = %v, want %v (the tool step's StepID)", uir.StepID, toolStepID)
			}
			if uir.StepID.IsZero() {
				t.Error("UserInputRequested StepID is zero")
			}
		}
		if nReq != 1 {
			t.Errorf("UserInputRequested count = %d, want 1", nReq)
		}
	})

	t.Run("model always calls tools → TurnFailed{ToolLimitError}; committed steps survive, in-flight discarded", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 3, 100)
		// A single tool-call script repeated forever → never a text-only iteration.
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-x", "Echo", `{}`)},
		}}
		// Pre-existing history so base != 0.
		pre := content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "earlier"}}}}}
		cfg, st, rec := newTurnFixture(input, pre, ts, client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)

		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var tle *event.ToolLimitError
		if !errors.As(failed.Err, &tle) {
			t.Fatalf("TurnFailed.Err = %T, want *ToolLimitError via errors.As", failed.Err)
		}
		// Step-granularity rollback (Phase 8): the cap fires on the UNCOMPLETED 4th
		// iteration (maxIters=3). Iterations 1-3 are COMPLETED tool steps that each
		// committed (StepDone + group) and STAY in committed history; only the
		// in-flight 4th step is discarded. Committed history therefore holds:
		// earlier(pre), user, then 3 × (assistant tool_use + tool message) = 1+1+6.
		if got := len(stepDones(rec.events())); got != 3 {
			t.Errorf("StepDone count = %d, want 3 (the 3 completed tool steps before the cap)", got)
		}
		if len(rec.commits) != 3 {
			t.Errorf("commit count = %d, want 3 (3 completed steps committed)", len(rec.commits))
		}
		msgs := committedHistory(cfg.base, initialUser(st), rec)
		if len(msgs) != 1+1+6 {
			t.Fatalf("committed history len = %d, want 8 (pre, user, 3×(tool_use+tool))", len(msgs))
		}
		if msgs[0] != pre[0] {
			t.Errorf("committed history must preserve pre-turn history exactly")
		}
		// Each committed tool_use is paired with its tool result — the in-flight 4th
		// tool_use was never committed, so no unpaired tool_use survives.
		tu, tmCount := countToolUseInHistory(msgs)
		if tu != tmCount {
			t.Errorf("unpaired tool_use after step-granularity rollback: %d tool_use vs %d tool messages", tu, tmCount)
		}
		if tu != 3 {
			t.Errorf("committed tool_use count = %d, want 3 (the completed steps)", tu)
		}
	})

	t.Run("malformed tool args → {} in stored assistant message + tool-result error; turn continues", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 25, 100)
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-bad", "Echo", `{not valid json`)}, // malformed args
			{textChunk("recovered")},                               // model reacts → TurnDone
		}}
		cfg, st, rec := newTurnFixture(input, nil, ts, client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)

		if _, ok := terminal.(event.TurnDone); !ok {
			t.Fatalf("terminal = %T, want TurnDone (turn recovers from malformed args)", terminal)
		}
		if echo.runCount() != 0 {
			t.Errorf("echo ran %d times, want 0 (invalid args)", echo.runCount())
		}
		msgs := committedHistory(cfg.base, initialUser(st), rec)
		ai1, ok := msgs[1].(*content.AIMessage)
		if !ok {
			t.Fatalf("msgs[1] = %T, want *AIMessage", msgs[1])
		}
		var tub *content.ToolUseBlock
		for _, b := range ai1.Blocks {
			if x, ok := b.(*content.ToolUseBlock); ok {
				tub = x
			}
		}
		if tub == nil {
			t.Fatal("no ToolUseBlock in stored assistant message")
		}
		if string(tub.Input) != "{}" {
			t.Errorf("stored tool_use Input = %q, want %q (sanitized)", string(tub.Input), "{}")
		}
		if !json.Valid(tub.Input) {
			t.Errorf("stored tool_use Input is not valid JSON: %q", string(tub.Input))
		}
		tm, ok := msgs[2].(*content.ToolResultMessage)
		if !ok {
			t.Fatalf("msgs[2] = %T, want *ToolResultMessage", msgs[2])
		}
		if got := flattenToText(tm.Blocks); got == "" {
			t.Errorf("tool message must carry a non-empty error result, got %q", got)
		}
		var nStarted, nCompletedErr int
		for _, ev := range rec.events() {
			switch e := ev.(type) {
			case event.ToolCallStarted:
				nStarted++
			case event.ToolCallCompleted:
				if e.IsError {
					nCompletedErr++
				}
			}
		}
		if nStarted != 1 || nCompletedErr != 1 {
			t.Errorf("events: %d started / %d completed-err, want 1/1", nStarted, nCompletedErr)
		}
	})

	t.Run("ToolUseChunk fragments fold by Index (multi-fragment, multi-index, negative/huge Index no panic)", func(t *testing.T) {
		t.Parallel()
		echoA := &echoTool{name: "A", output: "ra"}
		echoB := &echoTool{name: "B", output: "rb"}
		ts := agenticToolSet([]tool.InvokableTool{echoA, echoB}, 25, 100)
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{
				toolUseChunk(1, "id-b", "B", `{"k"`),
				toolUseChunk(0, "id-a", "A", `{"k"`),
				toolUseChunk(1, "", "", `:2}`),
				toolUseChunk(0, "", "", `:1}`),
				toolUseChunk(-5, "id-neg", "A", `{}`),
				toolUseChunk(1<<30, "id-huge", "B", `{}`),
			},
			{textChunk("done")},
		}}
		cfg, st, rec := newTurnFixture(input, nil, ts, client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)
		if _, ok := terminal.(event.TurnDone); !ok {
			t.Fatalf("terminal = %T, want TurnDone", terminal)
		}
		msgs := committedHistory(cfg.base, initialUser(st), rec)
		ai1 := msgs[1].(*content.AIMessage)
		var blocks []*content.ToolUseBlock
		for _, b := range ai1.Blocks {
			if x, ok := b.(*content.ToolUseBlock); ok {
				blocks = append(blocks, x)
			}
		}
		if len(blocks) != 4 {
			t.Fatalf("folded %d tool_use blocks, want 4 (indices -5,0,1,2^30)", len(blocks))
		}
		wantIDs := []string{"id-neg", "id-a", "id-b", "id-huge"}
		for i, b := range blocks {
			if b.ID != wantIDs[i] {
				t.Errorf("block[%d].ID = %q, want %q (ascending Index order)", i, b.ID, wantIDs[i])
			}
		}
		for _, b := range blocks {
			if b.ID == "id-a" && string(b.Input) != `{"k":1}` {
				t.Errorf("index 0 folded Input = %q, want %q", string(b.Input), `{"k":1}`)
			}
			if b.ID == "id-b" && string(b.Input) != `{"k":2}` {
				t.Errorf("index 1 folded Input = %q, want %q", string(b.Input), `{"k":2}`)
			}
		}
	})

	t.Run("interrupt mid-loop → completed step survives, in-flight discarded, TurnInterrupted", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 25, 100)
		ctx, cancel := context.WithCancel(context.Background())
		// iter1 streams a tool call and runs it (step 0 completes + commits); iter2's
		// Stream() cancels ctx before producing any chunk → interrupt between steps.
		client := &scriptedLLM{
			scripts: [][]content.Chunk{
				{toolUseChunk(0, "id-1", "Echo", `{}`)},
				{textChunk("never reached")},
			},
			onStreamN: map[int]func(){1: cancel},
		}
		pre := content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "earlier"}}}}}
		cfg, st, rec := newTurnFixture(input, pre, ts, client, noGateReg())
		terminal := runTurn(ctx, cfg, st)

		if _, ok := terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want TurnInterrupted", terminal)
		}
		// Step 0 (the tool step) COMPLETED + committed before iter2's stream was
		// interrupted, so exactly one StepDone was emitted and committed; the
		// interrupted step 1 emits/commits none and is discarded.
		if got := len(stepDones(rec.events())); got != 1 {
			t.Errorf("StepDone count = %d, want 1 (only the completed step 0)", got)
		}
		if len(rec.commits) != 1 {
			t.Errorf("commit count = %d, want 1 (only the completed step 0)", len(rec.commits))
		}
		// Committed history: earlier(pre), user, assistant(tool_use), tool message.
		msgs := committedHistory(cfg.base, initialUser(st), rec)
		if len(msgs) != 4 {
			t.Fatalf("committed history len = %d, want 4 (pre, user, tool_use, tool)", len(msgs))
		}
		tu, tmCount := countToolUseInHistory(msgs)
		if tu != tmCount || tu != 1 {
			t.Errorf("committed pairs: %d tool_use vs %d tool messages, want 1/1", tu, tmCount)
		}
	})

	t.Run("toolDefs maps registry → req.Tools (the client receives the tool defs)", func(t *testing.T) {
		t.Parallel()
		echoA := &echoTool{name: "A", output: "ra"}
		echoB := &echoTool{name: "B", output: "rb"}
		ts := agenticToolSet([]tool.InvokableTool{echoA, echoB}, 25, 100)
		client := &scriptedLLM{scripts: [][]content.Chunk{{textChunk("hi")}}}
		cfg, st, _ := newTurnFixture(input, nil, ts, client, noGateReg())
		runTurn(context.Background(), cfg, st)

		reqs := client.requests()
		if len(reqs) == 0 {
			t.Fatal("no request recorded")
		}
		defs := reqs[0].Tools
		if len(defs) != 2 {
			t.Fatalf("req.Tools len = %d, want 2", len(defs))
		}
		byName := map[string]llm.Tool{}
		for _, d := range defs {
			byName[d.Name] = d
		}
		if _, ok := byName["A"]; !ok {
			t.Errorf("tool A missing from req.Tools")
		}
		if got := string(byName["A"].Schema); got != `{"type":"object"}` {
			t.Errorf("tool A schema = %q, want %q", got, `{"type":"object"}`)
		}
		if byName["A"].Description != "echoes" {
			t.Errorf("tool A description = %q, want %q", byName["A"].Description, "echoes")
		}
	})

	t.Run("request base = turnConfig.base + staged turn msgs (committed pre-history is used, not duplicated)", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 25, 100)
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Echo", `{}`)},
			{textChunk("done")},
		}}
		pre := content.AgenticMessages{
			&content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "older user"}}}},
			&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "older reply"}}}},
		}
		cfg, st, _ := newTurnFixture(input, pre, ts, client, noGateReg())
		runTurn(context.Background(), cfg, st)

		reqs := client.requests()
		if len(reqs) != 2 {
			t.Fatalf("recorded %d requests, want 2", len(reqs))
		}
		// Request 0 (first step): base (2) + initial user (1) = 3 messages.
		if len(reqs[0].Messages) != 3 {
			t.Errorf("request 0 had %d messages, want 3 (base 2 + user)", len(reqs[0].Messages))
		}
		// Request 1 (continuation): base (2) + user + assistant(tool_use) + tool = 5.
		if len(reqs[1].Messages) != 5 {
			t.Errorf("request 1 had %d messages, want 5 (base 2 + user + tool_use + tool)", len(reqs[1].Messages))
		}
		// The first two messages of every request are the committed pre-history,
		// exactly once (not duplicated).
		if reqs[0].Messages[0] != pre[0] || reqs[0].Messages[1] != pre[1] {
			t.Errorf("request 0 did not start with the base pre-history")
		}
		if reqs[1].Messages[0] != pre[0] || reqs[1].Messages[1] != pre[1] {
			t.Errorf("request 1 did not start with the base pre-history")
		}
	})

	t.Run("call-cap fires when a batch exceeds MaxToolCallsPerTurn", func(t *testing.T) {
		t.Parallel()
		echo := &echoTool{name: "Echo", output: "ran"}
		ts := agenticToolSet([]tool.InvokableTool{echo}, 25, 1)
		client := &scriptedLLM{scripts: [][]content.Chunk{
			{toolUseChunk(0, "id-1", "Echo", `{}`), toolUseChunk(1, "id-2", "Echo", `{}`)},
		}}
		cfg, st, rec := newTurnFixture(input, nil, ts, client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)
		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var tle *event.ToolLimitError
		if !errors.As(failed.Err, &tle) {
			t.Fatalf("TurnFailed.Err = %T, want *ToolLimitError", failed.Err)
		}
		// The cap fires on the FIRST (uncompleted) step, so nothing is committed.
		if len(rec.commits) != 0 {
			t.Errorf("commit count = %d, want 0 (cap fires on the first uncompleted step)", len(rec.commits))
		}
		if echo.runCount() != 0 {
			t.Errorf("echo ran %d times, want 0 (cap fires before execution)", echo.runCount())
		}
	})
}

func TestRunTurn(t *testing.T) {
	t.Parallel()
	input := []content.Block{&content.TextBlock{Text: "hi"}}
	emptyTS := func() ToolSet { return resolveToolSetCaps(ToolSet{Permission: autoApproveGate{}}) }

	t.Run("success commits one step group and returns TurnDone", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: []content.Chunk{textChunk("hel"), textChunk("lo")}}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)

		done, ok := terminal.(event.TurnDone)
		if !ok {
			t.Fatalf("terminal = %T, want TurnDone", terminal)
		}
		msgs := committedHistory(cfg.base, initialUser(st), rec)
		if len(msgs) != 2 {
			t.Fatalf("committed history len = %d, want 2 (user, assistant)", len(msgs))
		}
		if _, ok := msgs[0].(*content.UserMessage); !ok {
			t.Errorf("msgs[0] = %T, want *UserMessage", msgs[0])
		}
		last := done.Message.Blocks[len(done.Message.Blocks)-1]
		tb, ok := last.(*content.TextBlock)
		if !ok {
			t.Fatalf("last block = %T, want *content.TextBlock", last)
		}
		if tb.Text != "hello" {
			t.Errorf("assembled text = %q, want %q", tb.Text, "hello")
		}
		// A single-step, no-tool turn commits exactly one step (Messages is just the
		// AIMessage) and emits its StepDone, stamped from the turn identity.
		sds := stepDones(rec.events())
		if len(sds) != 1 {
			t.Fatalf("StepDone count = %d, want 1 (single no-tool step)", len(sds))
		}
		if len(sds[0].Messages) != 1 {
			t.Fatalf("StepDone.Messages len = %d, want 1 (AIMessage only)", len(sds[0].Messages))
		}
		if sds[0].Messages[0] != done.Message {
			t.Errorf("StepDone.Messages[0] must be the assistant message (== TurnDone.Message)")
		}
		if sds[0].SessionID != st.sessionID || sds[0].LoopID != st.loopID || sds[0].TurnID != st.id || sds[0].StepID.IsZero() {
			t.Errorf("StepDone Header ids not stamped from identity: %+v", sds[0].Header)
		}
		// Ordering: StepDone is recorded AFTER the step's TokenDeltas (the recorder
		// funnels emit + commit into one ordered stream, like the actor's per-turn
		// stream) and there is no StepDone before any TokenDelta.
		var sawDelta, sawStepDone bool
		for _, e := range rec.events() {
			switch e.(type) {
			case event.TokenDelta:
				sawDelta = true
				if sawStepDone {
					t.Error("TokenDelta emitted after StepDone for the same step")
				}
			case event.StepDone:
				sawStepDone = true
				if !sawDelta {
					t.Error("StepDone emitted before any TokenDelta")
				}
			}
		}
	})

	t.Run("stream error discards the in-flight step (no commit), TurnFailed carries typed cause", func(t *testing.T) {
		t.Parallel()
		boom := &llm.ValidationError{Field: "x", Reason: "boom"}
		client := &fakeLLM{streamErr: boom}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)

		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ve *llm.ValidationError
		if !errors.As(failed.Err, &ve) {
			t.Fatalf("TurnFailed.Err = %T, want *llm.ValidationError via errors.As", failed.Err)
		}
		// The step never finalized an AIMessage, so nothing was committed.
		if len(rec.commits) != 0 {
			t.Errorf("commit count = %d, want 0 (in-flight step discarded)", len(rec.commits))
		}
		if got := len(stepDones(rec.events())); got != 0 {
			t.Errorf("StepDone count = %d, want 0 (step never completed)", got)
		}
	})

	t.Run("empty response discards the in-flight step and returns EmptyResponseError", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: nil}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)

		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ere *event.EmptyResponseError
		if !errors.As(failed.Err, &ere) {
			t.Fatalf("TurnFailed.Err = %T, want *EmptyResponseError", failed.Err)
		}
		if len(rec.commits) != 0 {
			t.Errorf("commit count = %d, want 0 (empty response is not a completed step)", len(rec.commits))
		}
		if got := len(stepDones(rec.events())); got != 0 {
			t.Errorf("StepDone count = %d, want 0 (empty response is not a completed step)", got)
		}
	})

	t.Run("empty-string-only chunks discard the in-flight step and return EmptyResponseError", func(t *testing.T) {
		t.Parallel()
		chunks := []content.Chunk{textChunk(""), textChunk("")}
		client := &fakeLLM{chunks: chunks}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)

		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ere *event.EmptyResponseError
		if !errors.As(failed.Err, &ere) {
			t.Fatalf("TurnFailed.Err = %T, want *EmptyResponseError", failed.Err)
		}
		if len(rec.commits) != 0 {
			t.Errorf("commit count = %d, want 0 (no assistant message stored)", len(rec.commits))
		}
		var deltas int
		for _, e := range rec.events() {
			if _, ok := e.(event.TokenDelta); ok {
				deltas++
			}
		}
		if deltas != len(chunks) {
			t.Errorf("TokenDelta count = %d, want %d (one per chunk)", deltas, len(chunks))
		}
	})

	t.Run("cancelled context discards the in-flight step and returns TurnInterrupted", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := &fakeLLM{streamErr: context.Canceled}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		terminal := runTurn(ctx, cfg, st)

		if _, ok := terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want TurnInterrupted", terminal)
		}
		if len(rec.commits) != 0 {
			t.Errorf("commit count = %d, want 0 (in-flight step discarded)", len(rec.commits))
		}
	})

	t.Run("mid-stream Next error discards the in-flight step with typed cause", func(t *testing.T) {
		t.Parallel()
		boom := &llm.ValidationError{Field: "y", Reason: "midstream"}
		client := &fakeLLM{chunks: []content.Chunk{textChunk("partial")}, nextErr: boom}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		terminal := runTurn(context.Background(), cfg, st)

		failed, ok := terminal.(event.TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ve *llm.ValidationError
		if !errors.As(failed.Err, &ve) {
			t.Fatalf("TurnFailed.Err = %T, want *llm.ValidationError", failed.Err)
		}
		if len(rec.commits) != 0 {
			t.Errorf("commit count = %d, want 0 (in-flight step discarded)", len(rec.commits))
		}
	})

	t.Run("a cancelled commit handshake aborts the turn (TurnInterrupted), keeping prior commits", func(t *testing.T) {
		t.Parallel()
		// The recorder rejects the commit with a CommitError, modelling an
		// Interrupt/Shutdown that cancels the handshake. runTurn must surface
		// TurnInterrupted instead of wedging or finalizing a TurnDone.
		client := &fakeLLM{chunks: []content.Chunk{textChunk("answer")}}
		cfg, st, rec := newTurnFixture(input, nil, emptyTS(), client, noGateReg())
		rec.commitErr = &CommitError{Reason: CommitTurnCancelled, Cause: context.Canceled}
		terminal := runTurn(context.Background(), cfg, st)
		if _, ok := terminal.(event.TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want TurnInterrupted (commit cancelled)", terminal)
		}
		if len(rec.commits) != 0 {
			t.Errorf("commit count = %d, want 0 (the commit was rejected)", len(rec.commits))
		}
	})
}
