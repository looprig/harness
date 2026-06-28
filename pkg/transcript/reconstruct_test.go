package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/event"
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
	default:
		return ev
	}
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
