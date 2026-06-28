package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/identity"
	"github.com/ciram-co/looprig/pkg/tool"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// noPrompts is a SystemPromptResolver that never resolves a prompt. Task 6 wires
// real resolution; Task 2 only needs the parameter to be accepted.
type noPrompts struct{}

func (noPrompts) SystemPrompt(uuid.UUID) (string, bool) { return "", false }

// sliceSource is a slice-backed RecordSource fake: it yields its records in order
// and returns io.EOF once exhausted.
type sliceSource struct {
	records []Record
	i       int
}

func (s *sliceSource) Next(context.Context) (Record, error) {
	if s.i >= len(s.records) {
		return nil, io.EOF
	}
	rec := s.records[s.i]
	s.i++
	return rec, nil
}

// errSource is a RecordSource whose Next always fails with a non-EOF error, used
// to exercise the read-failure path of Reconstruct.
type errSource struct{ err error }

func (s *errSource) Next(context.Context) (Record, error) { return nil, s.err }

// errRead is the sentinel cause an errSource surfaces; the test asserts it is
// recoverable from the returned *ReconstructError via errors.Unwrap.
var errRead = errors.New("source read failed")

// newSliceSource stamps each event with the primary loop id and an increasing
// CreatedAt (base + N seconds), wraps it in an EventRecord, and returns a source.
func newSliceSource(loopID uuid.UUID, base time.Time, evs ...event.Event) *sliceSource {
	recs := make([]Record, len(evs))
	for i, ev := range evs {
		at := base.Add(time.Duration(i) * time.Second)
		recs[i] = EventRecord{Event: stamp(ev, loopID, at)}
	}
	return &sliceSource{records: recs}
}

// stamp returns a copy of ev with its Header LoopID and CreatedAt set. Events are
// value types whose embedded Header is not settable through the Event interface, so
// stamping switches over the concrete types this test exercises.
func stamp(ev event.Event, loopID uuid.UUID, at time.Time) event.Event {
	switch e := ev.(type) {
	case event.SessionStarted:
		e.Header.LoopID, e.Header.CreatedAt = loopID, at
		return e
	case event.LoopStarted:
		e.Header.LoopID, e.Header.CreatedAt = loopID, at
		return e
	case event.TurnStarted:
		e.Header.LoopID, e.Header.CreatedAt = loopID, at
		return e
	case event.StepDone:
		e.Header.LoopID, e.Header.CreatedAt = loopID, at
		return e
	case event.TurnDone:
		e.Header.LoopID, e.Header.CreatedAt = loopID, at
		return e
	case event.PermissionRequested:
		e.Header.LoopID, e.Header.CreatedAt = loopID, at
		return e
	case event.UserInputRequested:
		e.Header.LoopID, e.Header.CreatedAt = loopID, at
		return e
	default:
		return ev
	}
}

// item is one fixture entry: exactly one of ev/cmd is set. newMixedSource stamps
// each with the loop id and an increasing CreatedAt and wraps it as a Record, so a
// table row can interleave enduring events and user commands in journal order.
type item struct {
	ev  event.Event
	cmd command.Command
}

func evItem(e event.Event) item      { return item{ev: e} }
func cmdItem(c command.Command) item { return item{cmd: c} }

// newMixedSource stamps each item with loopID and base+N*sec and returns a source
// yielding the records in order (io.EOF past the end).
func newMixedSource(loopID uuid.UUID, base time.Time, items ...item) *sliceSource {
	recs := make([]Record, len(items))
	for i, it := range items {
		at := base.Add(time.Duration(i) * time.Second)
		switch {
		case it.ev != nil:
			recs[i] = EventRecord{Event: stamp(it.ev, loopID, at)}
		case it.cmd != nil:
			recs[i] = CommandRecord{Command: stampCommand(it.cmd, at)}
		}
	}
	return &sliceSource{records: recs}
}

// stampCommand returns a copy of c with its Header.CreatedAt set (preserving every
// other field, including Agency). Commands are value types whose embedded Header is
// not settable through the Command interface, so this switches over the concrete
// gate-resolving commands the tests exercise.
func stampCommand(c command.Command, at time.Time) command.Command {
	switch cc := c.(type) {
	case command.ApproveToolCall:
		cc.Header.CreatedAt = at
		return cc
	case command.DenyToolCall:
		cc.Header.CreatedAt = at
		return cc
	case command.ProvideUserInput:
		cc.Header.CreatedAt = at
		return cc
	default:
		return c
	}
}

// loopNode is a Task-4 fixture entry: exactly one of ev/cmd, the loop id the event
// belongs to, and (for a child LoopStarted) the parent loop id to stamp onto
// Header.Cause. newLoopSource stamps each with its loop id and base+N*sec, so a
// table row can interleave events across a parent and a child loop in REAL journal
// order — the child's whole lifecycle before the parent's StepDone (Decision 6).
type loopNode struct {
	ev          event.Event
	cmd         command.Command
	loopID      uuid.UUID
	causeLoopID uuid.UUID // parent loop id for a child LoopStarted; zero otherwise
}

func evNode(loopID uuid.UUID, e event.Event) loopNode { return loopNode{ev: e, loopID: loopID} }
func cmdNode(c command.Command) loopNode              { return loopNode{cmd: c} }
func childStart(loopID, parentLoopID uuid.UUID, e event.LoopStarted) loopNode {
	return loopNode{ev: e, loopID: loopID, causeLoopID: parentLoopID}
}

// newLoopSource stamps each node with its loop id and an increasing CreatedAt and
// returns a source yielding the records in order (io.EOF past the end). A child
// LoopStarted additionally gets its Header.Cause.LoopID set to the parent loop.
func newLoopSource(base time.Time, nodes ...loopNode) *sliceSource {
	recs := make([]Record, len(nodes))
	for i, n := range nodes {
		at := base.Add(time.Duration(i) * time.Second)
		switch {
		case n.cmd != nil:
			recs[i] = CommandRecord{Command: stampCommand(n.cmd, at)}
		case n.ev != nil:
			ev := stamp(n.ev, n.loopID, at)
			if ls, ok := ev.(event.LoopStarted); ok && n.causeLoopID != (uuid.UUID{}) {
				ls.Header.Cause.LoopID = n.causeLoopID
				ev = ls
			}
			recs[i] = EventRecord{Event: ev}
		}
	}
	return &sliceSource{records: recs}
}

// userMsg builds a single-text-block user message for a fixture turn.
func userMsg(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// aiText builds an AIMessage whose only block is the given text (no tool uses).
func aiText(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// aiToolUse builds an AIMessage whose blocks are the given tool-use blocks.
func aiToolUse(blocks ...content.Block) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: blocks,
	}}
}

// toolResult builds a single-text-block tool result paired to useID.
func toolResult(useID, text string) *content.ToolResultMessage {
	return &content.ToolResultMessage{
		Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: text}}},
		ToolUseID: useID,
	}
}

// onlyStep returns the single step of the single turn of the root loop, failing if
// the model is not exactly one loop / one turn / one step.
func onlyStep(t *testing.T, s *Session) *Step {
	t.Helper()
	if s == nil || s.Root == nil {
		t.Fatal("nil session or root loop")
	}
	if len(s.Root.Turns) != 1 {
		t.Fatalf("len(Turns) = %d, want 1", len(s.Root.Turns))
	}
	turn := s.Root.Turns[0]
	if len(turn.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(turn.Steps))
	}
	return turn.Steps[0]
}

// firstText returns the text of the first *content.TextBlock in blocks, or "".
func firstText(blocks []content.Block) string {
	for _, b := range blocks {
		if tb, ok := b.(*content.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return u
}

func TestReconstruct(t *testing.T) {
	loopID := mustUUID(t)
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		src   RecordSource
		check func(t *testing.T, s *Session, warnings []Warning, err error)
	}{
		{
			name: "happy path: turn with one step and one tool call",
			src: newSliceSource(loopID, base,
				event.SessionStarted{Config: event.ConfigFingerprint{
					ModelID:   "claude-opus-4-8",
					AgentKind: "operator",
				}},
				event.LoopStarted{ParentToolUseID: ""},
				event.TurnStarted{
					TurnIndex: 1,
					Message: &content.UserMessage{Message: content.Message{
						Role:   content.RoleUser,
						Blocks: []content.Block{&content.TextBlock{Text: "hi"}},
					}},
				},
				event.StepDone{Messages: content.AgenticMessages{
					&content.AIMessage{Message: content.Message{
						Role: content.RoleAssistant,
						Blocks: []content.Block{
							&content.TextBlock{Text: "hello"},
							&content.ToolUseBlock{ID: "tu1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
						},
					}},
					&content.ToolResultMessage{
						Message: content.Message{
							Role:   content.RoleTool,
							Blocks: []content.Block{&content.TextBlock{Text: "ok"}},
						},
						ToolUseID: "tu1",
					},
				}},
				event.TurnDone{TurnIndex: 1},
			),
			check: func(t *testing.T, s *Session, warnings []Warning, err error) {
				if err != nil {
					t.Fatalf("Reconstruct() error = %v", err)
				}
				if s == nil {
					t.Fatal("Reconstruct() returned nil Session")
				}
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %+v", warnings)
				}
				if s.Config.ModelID != "claude-opus-4-8" || s.Config.AgentKind != "operator" {
					t.Errorf("Config = %+v, want model=claude-opus-4-8 kind=operator", s.Config)
				}
				if s.StartedAt != base {
					t.Errorf("StartedAt = %v, want %v", s.StartedAt, base)
				}
				if want := base.Add(4 * time.Second); s.EndedAt != want {
					t.Errorf("EndedAt = %v, want %v (last record)", s.EndedAt, want)
				}
				if s.Root == nil {
					t.Fatal("Root loop is nil")
				}
				if s.Root.ParentToolUseID != "" {
					t.Errorf("Root.ParentToolUseID = %q, want \"\"", s.Root.ParentToolUseID)
				}
				if len(s.Root.Turns) != 1 {
					t.Fatalf("len(Turns) = %d, want 1", len(s.Root.Turns))
				}
				turn := s.Root.Turns[0]
				if turn.Outcome != OutcomeDone {
					t.Errorf("Outcome = %d, want OutcomeDone", turn.Outcome)
				}
				if turn.Index != 1 {
					t.Errorf("turn.Index = %d, want 1", turn.Index)
				}
				if turn.User == nil || firstText(turn.User.Blocks) != "hi" {
					t.Errorf("User text = %q, want \"hi\"", msgText(turn.User))
				}
				if len(turn.Steps) != 1 {
					t.Fatalf("len(Steps) = %d, want 1", len(turn.Steps))
				}
				step := turn.Steps[0]
				if step.AI == nil || firstText(step.AI.Blocks) != "hello" {
					t.Errorf("AI text = %q, want \"hello\"", msgText(step.AI))
				}
				if len(step.Tools) != 1 {
					t.Fatalf("len(Tools) = %d, want 1", len(step.Tools))
				}
				tc := step.Tools[0]
				if tc.Name != "Bash" {
					t.Errorf("tool Name = %q, want Bash", tc.Name)
				}
				if tc.ToolUseID != "tu1" {
					t.Errorf("ToolUseID = %q, want tu1", tc.ToolUseID)
				}
				if firstText(tc.Result) != "ok" {
					t.Errorf("Result text = %q, want \"ok\"", firstText(tc.Result))
				}
				if tc.IsError {
					t.Error("IsError = true, want false")
				}
			},
		},
		{
			name: "read error: a non-EOF source failure is a typed ReconstructError",
			src:  &errSource{err: errRead},
			check: func(t *testing.T, s *Session, warnings []Warning, err error) {
				if s != nil || warnings != nil {
					t.Fatalf("on read error want nil session/warnings, got s=%v warnings=%v", s, warnings)
				}
				var re *ReconstructError
				if !errors.As(err, &re) {
					t.Fatalf("error = %v (%T), want *ReconstructError", err, err)
				}
				if re.Stage != stageRead {
					t.Errorf("Stage = %q, want %q", re.Stage, stageRead)
				}
				if got := errors.Unwrap(re); !errors.Is(got, errRead) {
					t.Errorf("Unwrap = %v, want %v", got, errRead)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, warnings, err := Reconstruct(context.Background(), tt.src, noPrompts{})
			tt.check(t, s, warnings, err)
		})
	}
}

// msgText extracts the first text block from a possibly-nil Message, for readable
// failure messages. It is role-agnostic — used for both user and AI messages.
func msgText(m *Message) string {
	if m == nil {
		return "<nil>"
	}
	return firstText(m.Blocks)
}

func TestReconstructGates(t *testing.T) {
	loopID := mustUUID(t)
	e1, e2 := mustUUID(t), mustUUID(t)
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	// cmdTime is the CreatedAt newMixedSource stamps onto the item at position i.
	cmdTime := func(i int) time.Time { return base.Add(time.Duration(i) * time.Second) }

	approve := func(exec uuid.UUID, scope tool.ApprovalScope) command.ApproveToolCall {
		return command.ApproveToolCall{
			GateRoute: command.GateRoute{ToolExecutionID: exec},
			Scope:     scope,
			Header:    command.Header{Agency: identity.AgencyUser},
		}
	}

	tests := []struct {
		name  string
		src   RecordSource
		check func(t *testing.T, s *Session, warnings []Warning)
	}{
		{
			name: "permission approved at session scope binds to its tool call",
			src: newMixedSource(loopID, base,
				evItem(event.LoopStarted{ParentToolUseID: ""}),
				evItem(event.TurnStarted{TurnIndex: 1, Message: userMsg("run tests")}),
				evItem(event.PermissionRequested{ToolExecutionID: e1, Request: tool.BashRequest{Command: "go test ./..."}}),
				cmdItem(approve(e1, tool.ScopeSession)),
				evItem(event.StepDone{Messages: content.AgenticMessages{
					aiToolUse(&content.ToolUseBlock{ID: "tu1", Name: "Bash", Input: json.RawMessage(`{"command":"go test ./..."}`)}),
					toolResult("tu1", "ok"),
				}}),
				evItem(event.TurnDone{TurnIndex: 1}),
			),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %+v", warnings)
				}
				step := onlyStep(t, s)
				if len(step.Gates) != 1 {
					t.Fatalf("len(Gates) = %d, want 1", len(step.Gates))
				}
				g := step.Gates[0]
				if g.Kind != GateKindPermission {
					t.Errorf("Kind = %d, want GateKindPermission", g.Kind)
				}
				if g.Decision != DecisionApproved {
					t.Errorf("Decision = %d, want DecisionApproved", g.Decision)
				}
				if g.Scope != tool.ScopeSession {
					t.Errorf("Scope = %d, want ScopeSession", g.Scope)
				}
				if g.ToolName != "Bash" {
					t.Errorf("ToolName = %q, want Bash", g.ToolName)
				}
				if g.Description != "go test ./..." {
					t.Errorf("Description = %q, want %q", g.Description, "go test ./...")
				}
				if g.ToolUseID != "tu1" {
					t.Errorf("ToolUseID = %q, want tu1", g.ToolUseID)
				}
				if want := cmdTime(3); g.DecidedAt != want {
					t.Errorf("DecidedAt = %v, want %v (command time)", g.DecidedAt, want)
				}
				if len(step.Tools) != 1 {
					t.Fatalf("len(Tools) = %d, want 1", len(step.Tools))
				}
				if step.Tools[0].Gate != g {
					t.Errorf("Tools[0].Gate (%p) is not the same pointer as Gates[0] (%p)", step.Tools[0].Gate, g)
				}
			},
		},
		{
			name: "permission denied",
			src: newMixedSource(loopID, base,
				evItem(event.LoopStarted{}),
				evItem(event.TurnStarted{TurnIndex: 1, Message: userMsg("run")}),
				evItem(event.PermissionRequested{ToolExecutionID: e1, Request: tool.BashRequest{Command: "rm -rf /"}}),
				cmdItem(command.DenyToolCall{GateRoute: command.GateRoute{ToolExecutionID: e1}, Header: command.Header{Agency: identity.AgencyUser}}),
				evItem(event.StepDone{Messages: content.AgenticMessages{
					aiToolUse(&content.ToolUseBlock{ID: "tu1", Name: "Bash", Input: json.RawMessage(`{"command":"rm -rf /"}`)}),
					toolResult("tu1", "denied"),
				}}),
				evItem(event.TurnDone{TurnIndex: 1}),
			),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %+v", warnings)
				}
				step := onlyStep(t, s)
				if len(step.Gates) != 1 {
					t.Fatalf("len(Gates) = %d, want 1", len(step.Gates))
				}
				g := step.Gates[0]
				if g.Decision != DecisionDenied {
					t.Errorf("Decision = %d, want DecisionDenied", g.Decision)
				}
				if g.ToolUseID != "tu1" || step.Tools[0].Gate != g {
					t.Errorf("denied gate not bound to tu1: ToolUseID=%q sameptr=%v", g.ToolUseID, step.Tools[0].Gate == g)
				}
			},
		},
		{
			name: "askUser answered lands on Step.Gates",
			src: newMixedSource(loopID, base,
				evItem(event.LoopStarted{}),
				evItem(event.TurnStarted{TurnIndex: 1, Message: userMsg("ask me")}),
				evItem(event.UserInputRequested{ToolExecutionID: e2, Question: "which?", Choices: []string{"a", "b"}}),
				cmdItem(command.ProvideUserInput{GateRoute: command.GateRoute{ToolExecutionID: e2}, Answer: "a", Header: command.Header{Agency: identity.AgencyUser}}),
				evItem(event.StepDone{Messages: content.AgenticMessages{
					aiToolUse(&content.ToolUseBlock{ID: "tu2", Name: "AskUser", Input: json.RawMessage(`{"question":"which?"}`)}),
					toolResult("tu2", "a"),
				}}),
				evItem(event.TurnDone{TurnIndex: 1}),
			),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %+v", warnings)
				}
				step := onlyStep(t, s)
				if len(step.Gates) != 1 {
					t.Fatalf("len(Gates) = %d, want 1", len(step.Gates))
				}
				g := step.Gates[0]
				if g.Kind != GateKindAskUser {
					t.Errorf("Kind = %d, want GateKindAskUser", g.Kind)
				}
				if g.Decision != DecisionAnswered {
					t.Errorf("Decision = %d, want DecisionAnswered", g.Decision)
				}
				if g.Question != "which?" {
					t.Errorf("Question = %q, want which?", g.Question)
				}
				if len(g.Choices) != 2 || g.Choices[0] != "a" || g.Choices[1] != "b" {
					t.Errorf("Choices = %v, want [a b]", g.Choices)
				}
				if g.Answer != "a" {
					t.Errorf("Answer = %q, want a", g.Answer)
				}
				// The askUser gate carries no ToolName; the gateToolName -> "AskUser"
				// fallback must bind it to the AskUser tool call (tu2).
				if g.ToolUseID != "tu2" {
					t.Errorf("ToolUseID = %q, want tu2 (askUser fallback bind)", g.ToolUseID)
				}
				if len(step.Tools) != 1 || step.Tools[0].Gate != g {
					t.Error("Tools[0].Gate is not the same pointer as Gates[0]")
				}
			},
		},
		{
			name: "nil permission request stays unbound with empty ToolName",
			src: newMixedSource(loopID, base,
				evItem(event.LoopStarted{}),
				evItem(event.TurnStarted{TurnIndex: 1, Message: userMsg("run")}),
				evItem(event.PermissionRequested{ToolExecutionID: e1, Request: nil}),
				cmdItem(approve(e1, tool.ScopeOnce)),
				evItem(event.StepDone{Messages: content.AgenticMessages{
					aiToolUse(&content.ToolUseBlock{ID: "tu1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}),
					toolResult("tu1", "ok"),
				}}),
				evItem(event.TurnDone{TurnIndex: 1}),
			),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %+v", warnings)
				}
				step := onlyStep(t, s)
				if len(step.Gates) != 1 {
					t.Fatalf("len(Gates) = %d, want 1", len(step.Gates))
				}
				g := step.Gates[0]
				if g.ToolName != "" {
					t.Errorf("ToolName = %q, want \"\" (nil Request)", g.ToolName)
				}
				if g.Decision != DecisionApproved {
					t.Errorf("Decision = %d, want DecisionApproved", g.Decision)
				}
				// Empty ToolName -> firstUnboundNamed("") returns nil -> gate stays unbound.
				if g.ToolUseID != "" {
					t.Errorf("ToolUseID = %q, want \"\" (unbound)", g.ToolUseID)
				}
				if len(step.Tools) != 1 || step.Tools[0].Gate != nil {
					t.Error("Tools[0].Gate != nil, want nil (no name to bind by)")
				}
			},
		},
		{
			name: "gate whose ToolName matches no tool stays unbound",
			src: newMixedSource(loopID, base,
				evItem(event.LoopStarted{}),
				evItem(event.TurnStarted{TurnIndex: 1, Message: userMsg("run")}),
				evItem(event.PermissionRequested{ToolExecutionID: e1, Request: tool.BashRequest{Command: "ls"}}),
				cmdItem(approve(e1, tool.ScopeOnce)),
				evItem(event.StepDone{Messages: content.AgenticMessages{
					aiToolUse(&content.ToolUseBlock{ID: "tu1", Name: "Glob", Input: json.RawMessage(`{"pattern":"*.go"}`)}),
					toolResult("tu1", "found"),
				}}),
				evItem(event.TurnDone{TurnIndex: 1}),
			),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %+v", warnings)
				}
				step := onlyStep(t, s)
				if len(step.Gates) != 1 {
					t.Fatalf("len(Gates) = %d, want 1", len(step.Gates))
				}
				g := step.Gates[0]
				if g.Decision != DecisionApproved {
					t.Errorf("Decision = %d, want DecisionApproved", g.Decision)
				}
				if g.ToolName != "Bash" {
					t.Errorf("ToolName = %q, want Bash", g.ToolName)
				}
				// No Bash tool in the step (only Glob) -> the gate stays unbound.
				if g.ToolUseID != "" {
					t.Errorf("ToolUseID = %q, want \"\" (no matching tool)", g.ToolUseID)
				}
				if len(step.Tools) != 1 || step.Tools[0].Gate != nil {
					t.Error("Tools[0].Gate != nil, want nil (name mismatch)")
				}
			},
		},
		{
			name: "unresolved gate stays pending with no warning",
			src: newMixedSource(loopID, base,
				evItem(event.LoopStarted{}),
				evItem(event.TurnStarted{TurnIndex: 1, Message: userMsg("run")}),
				evItem(event.PermissionRequested{ToolExecutionID: e1, Request: tool.BashRequest{Command: "ls"}}),
				evItem(event.StepDone{Messages: content.AgenticMessages{
					aiToolUse(&content.ToolUseBlock{ID: "tu1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}),
					toolResult("tu1", "ok"),
				}}),
				evItem(event.TurnDone{TurnIndex: 1}),
			),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 0 {
					t.Fatalf("a pending gate must not warn, got: %+v", warnings)
				}
				step := onlyStep(t, s)
				if len(step.Gates) != 1 {
					t.Fatalf("len(Gates) = %d, want 1", len(step.Gates))
				}
				if step.Gates[0].Decision != DecisionPending {
					t.Errorf("Decision = %d, want DecisionPending", step.Gates[0].Decision)
				}
			},
		},
		{
			name: "orphan command warns without panic or gate",
			src: newMixedSource(loopID, base,
				evItem(event.LoopStarted{}),
				evItem(event.TurnStarted{TurnIndex: 1, Message: userMsg("run")}),
				cmdItem(approve(e1, tool.ScopeOnce)), // targets a gate that never opened
				evItem(event.StepDone{Messages: content.AgenticMessages{
					aiToolUse(&content.ToolUseBlock{ID: "tu1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}),
					toolResult("tu1", "ok"),
				}}),
				evItem(event.TurnDone{TurnIndex: 1}),
			),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 1 {
					t.Fatalf("len(warnings) = %d, want 1", len(warnings))
				}
				step := onlyStep(t, s)
				if len(step.Gates) != 0 {
					t.Errorf("len(Gates) = %d, want 0 (orphan command makes no gate)", len(step.Gates))
				}
				if step.Tools[0].Gate != nil {
					t.Error("Tools[0].Gate != nil, want nil (no gate bound)")
				}
			},
		},
		{
			name: "two same-named gated calls bind positionally",
			src: newMixedSource(loopID, base,
				evItem(event.LoopStarted{}),
				evItem(event.TurnStarted{TurnIndex: 1, Message: userMsg("run two")}),
				evItem(event.PermissionRequested{ToolExecutionID: e1, Request: tool.BashRequest{Command: "echo 1"}}),
				cmdItem(approve(e1, tool.ScopeOnce)),
				evItem(event.PermissionRequested{ToolExecutionID: e2, Request: tool.BashRequest{Command: "echo 2"}}),
				cmdItem(approve(e2, tool.ScopeOnce)),
				evItem(event.StepDone{Messages: content.AgenticMessages{
					aiToolUse(
						&content.ToolUseBlock{ID: "tu1", Name: "Bash", Input: json.RawMessage(`{"command":"echo 1"}`)},
						&content.ToolUseBlock{ID: "tu2", Name: "Bash", Input: json.RawMessage(`{"command":"echo 2"}`)},
					),
					toolResult("tu1", "1"),
					toolResult("tu2", "2"),
				}}),
				evItem(event.TurnDone{TurnIndex: 1}),
			),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %+v", warnings)
				}
				step := onlyStep(t, s)
				if len(step.Gates) != 2 {
					t.Fatalf("len(Gates) = %d, want 2", len(step.Gates))
				}
				if len(step.Tools) != 2 {
					t.Fatalf("len(Tools) = %d, want 2", len(step.Tools))
				}
				// gate(e1) arrived first → Gates[0] → binds the first Bash (tu1);
				// gate(e2) → Gates[1] → the next unbound Bash (tu2).
				if step.Gates[0].ToolUseID != "tu1" {
					t.Errorf("Gates[0].ToolUseID = %q, want tu1", step.Gates[0].ToolUseID)
				}
				if step.Gates[1].ToolUseID != "tu2" {
					t.Errorf("Gates[1].ToolUseID = %q, want tu2", step.Gates[1].ToolUseID)
				}
				if step.Tools[0].Gate != step.Gates[0] {
					t.Error("Tools[0].Gate is not Gates[0]")
				}
				if step.Tools[1].Gate != step.Gates[1] {
					t.Error("Tools[1].Gate is not Gates[1]")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, warnings, err := Reconstruct(context.Background(), tt.src, noPrompts{})
			if err != nil {
				t.Fatalf("Reconstruct() error = %v", err)
			}
			tt.check(t, s, warnings)
		})
	}
}

// onlyChildStep returns the single step of the single turn of a child loop, failing
// if it is not exactly one turn / one step.
func onlyChildStep(t *testing.T, child *Loop) *Step {
	t.Helper()
	if child == nil {
		t.Fatal("nil child loop")
	}
	if len(child.Turns) != 1 {
		t.Fatalf("child len(Turns) = %d, want 1", len(child.Turns))
	}
	if len(child.Turns[0].Steps) != 1 {
		t.Fatalf("child len(Steps) = %d, want 1", len(child.Turns[0].Steps))
	}
	return child.Turns[0].Steps[0]
}

func TestReconstructSubagents(t *testing.T) {
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	approveOnce := func(exec uuid.UUID) command.ApproveToolCall {
		return command.ApproveToolCall{
			GateRoute: command.GateRoute{ToolExecutionID: exec},
			Scope:     tool.ScopeOnce,
			Header:    command.Header{Agency: identity.AgencyUser},
		}
	}

	tests := []struct {
		name  string
		src   RecordSource
		check func(t *testing.T, s *Session, warnings []Warning)
	}{
		{
			name: "child loop nests under its spawning Subagent tool call",
			src: func() RecordSource {
				primary, child := mustUUID(t), mustUUID(t)
				return newLoopSource(base,
					evNode(primary, event.LoopStarted{ParentToolUseID: ""}),
					evNode(primary, event.TurnStarted{TurnIndex: 1, Message: userMsg("review this")}),
					childStart(child, primary, event.LoopStarted{
						ParentToolUseID: "sub1",
						Header:          event.Header{AgentName: identity.AgentName("reviewer")},
					}),
					evNode(child, event.TurnStarted{TurnIndex: 1, Message: userMsg("on it")}),
					evNode(child, event.StepDone{Messages: content.AgenticMessages{aiText("looks good")}}),
					evNode(child, event.TurnDone{TurnIndex: 1}),
					evNode(primary, event.StepDone{Messages: content.AgenticMessages{
						aiToolUse(&content.ToolUseBlock{ID: "sub1", Name: "Subagent", Input: json.RawMessage(`{"agent":"reviewer"}`)}),
						toolResult("sub1", "done"),
					}}),
					evNode(primary, event.TurnDone{TurnIndex: 1}),
				)
			}(),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %+v", warnings)
				}
				step := onlyStep(t, s)
				if len(step.Tools) != 1 {
					t.Fatalf("len(Tools) = %d, want 1", len(step.Tools))
				}
				tc := step.Tools[0]
				if tc.ToolUseID != "sub1" || tc.Name != "Subagent" {
					t.Fatalf("tool = {%q,%q}, want {sub1,Subagent}", tc.ToolUseID, tc.Name)
				}
				if tc.Child == nil {
					t.Fatal("Subagent tool call has nil Child")
				}
				if tc.Child.AgentName != "reviewer" {
					t.Errorf("Child.AgentName = %q, want reviewer", tc.Child.AgentName)
				}
				if tc.Child == s.Root {
					t.Error("child loop must not be Root")
				}
				if s.Root.ParentToolUseID != "" {
					t.Errorf("Root.ParentToolUseID = %q, want \"\" (primary only)", s.Root.ParentToolUseID)
				}
				cs := onlyChildStep(t, tc.Child)
				if cs.AI == nil || firstText(cs.AI.Blocks) != "looks good" {
					t.Errorf("child step AI = %q, want \"looks good\"", msgText(cs.AI))
				}
			},
		},
		{
			name: "two concurrent children attach to their own tool calls",
			src: func() RecordSource {
				primary, c1, c2 := mustUUID(t), mustUUID(t), mustUUID(t)
				return newLoopSource(base,
					evNode(primary, event.LoopStarted{ParentToolUseID: ""}),
					evNode(primary, event.TurnStarted{TurnIndex: 1, Message: userMsg("spawn two")}),
					childStart(c1, primary, event.LoopStarted{ParentToolUseID: "sub1", Header: event.Header{AgentName: identity.AgentName("reviewer")}}),
					evNode(c1, event.TurnStarted{TurnIndex: 1, Message: userMsg("one")}),
					evNode(c1, event.StepDone{Messages: content.AgenticMessages{aiText("child one")}}),
					evNode(c1, event.TurnDone{TurnIndex: 1}),
					childStart(c2, primary, event.LoopStarted{ParentToolUseID: "sub2", Header: event.Header{AgentName: identity.AgentName("tester")}}),
					evNode(c2, event.TurnStarted{TurnIndex: 1, Message: userMsg("two")}),
					evNode(c2, event.StepDone{Messages: content.AgenticMessages{aiText("child two")}}),
					evNode(c2, event.TurnDone{TurnIndex: 1}),
					evNode(primary, event.StepDone{Messages: content.AgenticMessages{
						aiToolUse(
							&content.ToolUseBlock{ID: "sub1", Name: "Subagent"},
							&content.ToolUseBlock{ID: "sub2", Name: "Subagent"},
						),
						toolResult("sub1", "a"),
						toolResult("sub2", "b"),
					}}),
					evNode(primary, event.TurnDone{TurnIndex: 1}),
				)
			}(),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %+v", warnings)
				}
				step := onlyStep(t, s)
				if len(step.Tools) != 2 {
					t.Fatalf("len(Tools) = %d, want 2", len(step.Tools))
				}
				one, two := step.Tools[0], step.Tools[1]
				if one.Child == nil || one.Child.AgentName != "reviewer" {
					t.Errorf("Tools[0].Child = %+v, want reviewer", one.Child)
				}
				if two.Child == nil || two.Child.AgentName != "tester" {
					t.Errorf("Tools[1].Child = %+v, want tester", two.Child)
				}
				if one.Child == two.Child {
					t.Error("the two tool calls cross-attached to the same child")
				}
				if got := firstText(onlyChildStep(t, one.Child).AI.Blocks); got != "child one" {
					t.Errorf("Tools[0].Child step = %q, want \"child one\"", got)
				}
				if got := firstText(onlyChildStep(t, two.Child).AI.Blocks); got != "child two" {
					t.Errorf("Tools[1].Child step = %q, want \"child two\"", got)
				}
			},
		},
		{
			name: "orphan child whose parent tool-use never appears warns once",
			src: func() RecordSource {
				primary, child := mustUUID(t), mustUUID(t)
				return newLoopSource(base,
					evNode(primary, event.LoopStarted{ParentToolUseID: ""}),
					evNode(primary, event.TurnStarted{TurnIndex: 1, Message: userMsg("go")}),
					childStart(child, primary, event.LoopStarted{ParentToolUseID: "nope", Header: event.Header{AgentName: identity.AgentName("ghost")}}),
					evNode(child, event.TurnStarted{TurnIndex: 1, Message: userMsg("orphaned")}),
					evNode(child, event.StepDone{Messages: content.AgenticMessages{aiText("nobody owns me")}}),
					evNode(child, event.TurnDone{TurnIndex: 1}),
					evNode(primary, event.StepDone{Messages: content.AgenticMessages{
						aiToolUse(&content.ToolUseBlock{ID: "sub1", Name: "Subagent"}),
						toolResult("sub1", "done"),
					}}),
					evNode(primary, event.TurnDone{TurnIndex: 1}),
				)
			}(),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 1 {
					t.Fatalf("len(warnings) = %d, want 1 (orphan child)", len(warnings))
				}
				step := onlyStep(t, s)
				if len(step.Tools) != 1 {
					t.Fatalf("len(Tools) = %d, want 1", len(step.Tools))
				}
				if step.Tools[0].Child != nil {
					t.Error("Subagent tool wrongly attached the orphan child")
				}
				// The orphan is unreachable from Root: it never became a ToolCall.Child.
				if s.Root.ParentToolUseID != "" {
					t.Errorf("Root.ParentToolUseID = %q, want \"\"", s.Root.ParentToolUseID)
				}
			},
		},
		{
			name: "per-loop gate isolation: child gate binds child tool, parent gate binds parent tool",
			src: func() RecordSource {
				primary, child := mustUUID(t), mustUUID(t)
				ec, ep := mustUUID(t), mustUUID(t)
				return newLoopSource(base,
					evNode(primary, event.LoopStarted{ParentToolUseID: ""}),
					evNode(primary, event.TurnStarted{TurnIndex: 1, Message: userMsg("review + run")}),
					childStart(child, primary, event.LoopStarted{ParentToolUseID: "sub1", Header: event.Header{AgentName: identity.AgentName("reviewer")}}),
					evNode(child, event.TurnStarted{TurnIndex: 1, Message: userMsg("on it")}),
					// Child gate opens, then the parent's own gate opens, BOTH before the
					// child StepDone — a global buffer would dump both onto the child step.
					evNode(child, event.PermissionRequested{ToolExecutionID: ec, Request: tool.BashRequest{Command: "go vet ./..."}}),
					cmdNode(approveOnce(ec)),
					evNode(primary, event.PermissionRequested{ToolExecutionID: ep, Request: tool.BashRequest{Command: "ls"}}),
					cmdNode(approveOnce(ep)),
					evNode(child, event.StepDone{Messages: content.AgenticMessages{
						aiToolUse(&content.ToolUseBlock{ID: "ctu1", Name: "Bash", Input: json.RawMessage(`{"command":"go vet ./..."}`)}),
						toolResult("ctu1", "ok"),
					}}),
					evNode(child, event.TurnDone{TurnIndex: 1}),
					evNode(primary, event.StepDone{Messages: content.AgenticMessages{
						aiToolUse(
							&content.ToolUseBlock{ID: "sub1", Name: "Subagent"},
							&content.ToolUseBlock{ID: "ptu1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
						),
						toolResult("sub1", "reviewed"),
						toolResult("ptu1", "ok"),
					}}),
					evNode(primary, event.TurnDone{TurnIndex: 1}),
				)
			}(),
			check: func(t *testing.T, s *Session, warnings []Warning) {
				if len(warnings) != 0 {
					t.Fatalf("unexpected warnings: %+v", warnings)
				}
				parentStep := onlyStep(t, s)
				if len(parentStep.Tools) != 2 {
					t.Fatalf("parent len(Tools) = %d, want 2", len(parentStep.Tools))
				}
				sub, parentBash := parentStep.Tools[0], parentStep.Tools[1]
				if sub.Name != "Subagent" || sub.Child == nil {
					t.Fatalf("Tools[0] = {%q, child=%v}, want Subagent with child", sub.Name, sub.Child)
				}
				if sub.Gate != nil {
					t.Error("Subagent tool wrongly carries a gate")
				}
				// Parent gate (ep) binds to the PARENT's Bash, on the PARENT step.
				if len(parentStep.Gates) != 1 {
					t.Fatalf("parent len(Gates) = %d, want 1", len(parentStep.Gates))
				}
				if parentStep.Gates[0].ToolUseID != "ptu1" {
					t.Errorf("parent gate bound to %q, want ptu1", parentStep.Gates[0].ToolUseID)
				}
				if parentBash.Gate != parentStep.Gates[0] {
					t.Error("parent Bash tool is not bound to the parent gate")
				}
				// Child gate (ec) binds to the CHILD's Bash, on the CHILD step — never leaks
				// onto the parent step.
				childStep := onlyChildStep(t, sub.Child)
				if len(childStep.Gates) != 1 {
					t.Fatalf("child len(Gates) = %d, want 1", len(childStep.Gates))
				}
				if childStep.Gates[0].ToolUseID != "ctu1" {
					t.Errorf("child gate bound to %q, want ctu1", childStep.Gates[0].ToolUseID)
				}
				if childStep.Gates[0].Decision != DecisionApproved {
					t.Errorf("child gate Decision = %d, want DecisionApproved", childStep.Gates[0].Decision)
				}
				if childStep.Tools[0].Gate != childStep.Gates[0] {
					t.Error("child Bash tool is not bound to the child gate")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, warnings, err := Reconstruct(context.Background(), tt.src, noPrompts{})
			if err != nil {
				t.Fatalf("Reconstruct() error = %v", err)
			}
			tt.check(t, s, warnings)
		})
	}
}

// TestReconstructOrphanWarningOrder pins the deterministic order of end-of-stream
// orphan warnings. finalize ranges a (randomized) map, so the order must come from an
// explicit sort by StartedAt — here "orphanA" is journaled before "orphanB", so its
// loop has the earlier StartedAt and must always warn first. Reconstructing the same
// stream many times must yield the identical order every run (a single run could pass
// by luck under map randomization).
func TestReconstructOrphanWarningOrder(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

	// newSrc builds a fresh stream that orphans two children (their parent tool-use ids
	// never appear as a tool-use): orphanA's LoopStarted precedes orphanB's.
	newSrc := func() RecordSource {
		primary, c1, c2 := mustUUID(t), mustUUID(t), mustUUID(t)
		return newLoopSource(base,
			evNode(primary, event.LoopStarted{ParentToolUseID: ""}),
			evNode(primary, event.TurnStarted{TurnIndex: 1, Message: userMsg("go")}),
			childStart(c1, primary, event.LoopStarted{ParentToolUseID: "orphanA", Header: event.Header{AgentName: identity.AgentName("first")}}),
			evNode(c1, event.TurnStarted{TurnIndex: 1, Message: userMsg("a")}),
			evNode(c1, event.StepDone{Messages: content.AgenticMessages{aiText("a")}}),
			evNode(c1, event.TurnDone{TurnIndex: 1}),
			childStart(c2, primary, event.LoopStarted{ParentToolUseID: "orphanB", Header: event.Header{AgentName: identity.AgentName("second")}}),
			evNode(c2, event.TurnStarted{TurnIndex: 1, Message: userMsg("b")}),
			evNode(c2, event.StepDone{Messages: content.AgenticMessages{aiText("b")}}),
			evNode(c2, event.TurnDone{TurnIndex: 1}),
			evNode(primary, event.StepDone{Messages: content.AgenticMessages{
				aiToolUse(&content.ToolUseBlock{ID: "sub1", Name: "Subagent"}),
				toolResult("sub1", "done"),
			}}),
			evNode(primary, event.TurnDone{TurnIndex: 1}),
		)
	}

	const runs = 50
	for i := 0; i < runs; i++ {
		_, warnings, err := Reconstruct(context.Background(), newSrc(), noPrompts{})
		if err != nil {
			t.Fatalf("run %d: Reconstruct() error = %v", i, err)
		}
		if len(warnings) != 2 {
			t.Fatalf("run %d: len(warnings) = %d, want 2", i, len(warnings))
		}
		if !strings.Contains(warnings[0].Text, "orphanA") {
			t.Fatalf("run %d: warnings[0] = %q, want it to mention orphanA (earlier StartedAt first)", i, warnings[0].Text)
		}
		if !strings.Contains(warnings[1].Text, "orphanB") {
			t.Fatalf("run %d: warnings[1] = %q, want it to mention orphanB", i, warnings[1].Text)
		}
		if warnings[0].At.After(warnings[1].At) {
			t.Fatalf("run %d: warnings not ordered by time: %v then %v", i, warnings[0].At, warnings[1].At)
		}
	}
}
