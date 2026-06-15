package loop

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// TestProjectForSink_RedactsAndExtractsCallID is a white-box test of the loop's
// sink-projection helper: for every event it must (1) drop secrets via
// SinkProjection when the event is Redactable, and (2) surface the CallID for the
// envelope from CallID-bearing events. Non-Redactable events pass through; events
// without a CallID yield the zero CallID.
func TestProjectForSink_RedactsAndExtractsCallID(t *testing.T) {
	t.Parallel()
	callID, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}

	const (
		secretCmd    = "SECRET_rm_-rf_slash"
		secretQ      = "SECRET_question_text"
		secretChoice = "SECRET_choice_text"
		secretResult = "SECRET_result_preview"
		secretArgs   = "SECRET_tool_arg_token"
	)

	tests := []struct {
		name       string
		in         event.Event
		wantCallID uuid.UUID
		// absentSecret must NOT appear in the projected event's encoding.
		absentSecret string
		// presentKeep must appear (non-secret survives).
		presentKeep string
		// wantType is the concrete type the projection must produce.
		check func(t *testing.T, got event.Event)
	}{
		{
			name:         "PermissionRequested redacted, CallID extracted",
			in:           event.PermissionRequested{CallID: callID, Request: tool.BashRequest{Command: secretCmd}},
			wantCallID:   callID,
			absentSecret: secretCmd,
			presentKeep:  "Bash",
			check: func(t *testing.T, got event.Event) {
				pr, ok := got.(event.PermissionRequested)
				if !ok {
					t.Fatalf("got %T, want PermissionRequested", got)
				}
				if pr.Request.Description() != "" {
					t.Errorf("Description() = %q, want empty (redacted)", pr.Request.Description())
				}
				if pr.Request.ToolName() != "Bash" {
					t.Errorf("ToolName() = %q, want Bash", pr.Request.ToolName())
				}
			},
		},
		{
			name:         "UserInputRequested redacted to sink shape, CallID extracted",
			in:           event.UserInputRequested{CallID: callID, Question: secretQ, Choices: []string{secretChoice, "b"}},
			wantCallID:   callID,
			absentSecret: secretQ,
			presentKeep:  "count=2",
			check: func(t *testing.T, got event.Event) {
				s, ok := got.(event.UserInputRequestedSink)
				if !ok {
					t.Fatalf("got %T, want UserInputRequestedSink", got)
				}
				if s.ChoiceCount != 2 {
					t.Errorf("ChoiceCount = %d, want 2", s.ChoiceCount)
				}
			},
		},
		{
			name:        "ToolCallStarted passes through, CallID extracted",
			in:          event.ToolCallStarted{CallID: callID, ToolName: "Bash", Summary: "run ls"},
			wantCallID:  callID,
			presentKeep: "run ls",
			check: func(t *testing.T, got event.Event) {
				if _, ok := got.(event.ToolCallStarted); !ok {
					t.Fatalf("got %T, want ToolCallStarted (unchanged)", got)
				}
			},
		},
		{
			name:         "ToolCallCompleted drops ResultPreview, CallID extracted",
			in:           event.ToolCallCompleted{CallID: callID, IsError: true, ResultPreview: secretResult},
			wantCallID:   callID,
			absentSecret: secretResult,
			presentKeep:  "err=true",
			check: func(t *testing.T, got event.Event) {
				c, ok := got.(event.ToolCallCompleted)
				if !ok {
					t.Fatalf("got %T, want ToolCallCompleted", got)
				}
				if c.ResultPreview != "" {
					t.Errorf("ResultPreview = %q, want empty", c.ResultPreview)
				}
				if !c.IsError {
					t.Error("IsError lost in projection")
				}
			},
		},
		{
			name:         "TokenDelta ToolUseChunk drops InputJSON, no CallID",
			in:           event.TokenDelta{TurnIndex: 1, Chunk: &content.ToolUseChunk{Name: "Bash", InputJSON: `{"cmd":"` + secretArgs + `"}`}},
			wantCallID:   uuid.UUID{},
			absentSecret: secretArgs,
			presentKeep:  "name=Bash",
		},
		{
			name: "TurnDone redacts ToolUseBlock.Input, no CallID",
			in: event.TurnDone{TurnIndex: 1, Message: &content.AIMessage{Message: content.Message{
				Blocks: []content.Block{&content.ToolUseBlock{Name: "Bash", Input: json.RawMessage(`{"cmd":"` + secretArgs + `"}`)}},
			}}},
			wantCallID:   uuid.UUID{},
			absentSecret: secretArgs,
			presentKeep:  "name=Bash",
		},
		{
			name:        "TurnStarted is not Redactable, passes through, no CallID",
			in:          event.TurnStarted{TurnIndex: 5},
			wantCallID:  uuid.UUID{},
			presentKeep: "",
			check: func(t *testing.T, got event.Event) {
				if _, ok := got.(event.TurnStarted); !ok {
					t.Fatalf("got %T, want TurnStarted unchanged", got)
				}
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, gotCallID := projectForSink(tt.in)
			if gotCallID != tt.wantCallID {
				t.Errorf("CallID = %v, want %v", gotCallID, tt.wantCallID)
			}
			enc := encodeSink(t, got)
			if tt.absentSecret != "" && strings.Contains(enc, tt.absentSecret) {
				t.Errorf("SECURITY: projected event %q leaks secret %q", enc, tt.absentSecret)
			}
			if tt.presentKeep != "" && !strings.Contains(enc, tt.presentKeep) {
				t.Errorf("projected event %q dropped required %q", enc, tt.presentKeep)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

// encodeSink renders a projected event so a test can assert a secret is absent.
func encodeSink(t *testing.T, ev event.Event) string {
	t.Helper()
	switch e := ev.(type) {
	case event.PermissionRequested:
		return "perm tool=" + e.Request.ToolName() + " desc=" + e.Request.Description()
	case event.UserInputRequestedSink:
		return "uis count=" + strconv.Itoa(e.ChoiceCount)
	case event.UserInputRequested:
		return "uir q=" + e.Question + " choices=" + strings.Join(e.Choices, ",")
	case event.ToolCallStarted:
		return "started tool=" + e.ToolName + " summary=" + e.Summary
	case event.ToolCallCompleted:
		return "completed err=" + boolStr(e.IsError) + " preview=" + e.ResultPreview
	case event.TokenDelta:
		if tu, ok := e.Chunk.(*content.ToolUseChunk); ok {
			return "td name=" + tu.Name + " input=" + tu.InputJSON
		}
		return "td other"
	case event.TurnDone:
		var sb strings.Builder
		sb.WriteString("turndone ")
		if e.Message != nil {
			for _, b := range e.Message.Blocks {
				if tu, ok := b.(*content.ToolUseBlock); ok {
					sb.WriteString("name=" + tu.Name + " input=" + string(tu.Input) + " ")
				}
			}
		}
		return sb.String()
	case event.TurnStarted:
		return "turnstarted"
	default:
		return "unknown"
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestSinkPathRedactsToolArgsEndToEnd drives a real turn whose stream contains a
// ToolUseChunk carrying a secret in InputJSON, and proves the two-audience
// contract end-to-end through the live actor: the per-turn STREAM receives the
// full ToolUseChunk (InputJSON present), while a recording EventSink receives the
// projected delta with InputJSON DROPPED — the secret never reaches the sink.
func TestSinkPathRedactsToolArgsEndToEnd(t *testing.T) {
	t.Parallel()
	const secret = "SECRET_inline_token_xyz"
	toolChunk := &content.ToolUseChunk{Index: 0, ID: "call_1", Name: "Bash", InputJSON: `{"cmd":"` + secret + `"}`}

	sink := &captureSink{}
	l, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("hi"), toolChunk}}, sink)
	ev, _ := startTurn(t, l, context.Background(), nil)

	// Collect the full stream (it includes the un-projected ToolUseChunk).
	var streamSawSecret, streamSawToolUse bool
	for e := range ev {
		if td, ok := e.(event.TokenDelta); ok {
			if tu, ok := td.Chunk.(*content.ToolUseChunk); ok {
				streamSawToolUse = true
				if strings.Contains(tu.InputJSON, secret) {
					streamSawSecret = true
				}
			}
		}
		switch e.(type) {
		case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
			goto drained
		}
	}
drained:
	if !streamSawToolUse {
		t.Fatal("stream never saw the ToolUseChunk TokenDelta")
	}
	if !streamSawSecret {
		t.Fatal("stream must receive the FULL ToolUseChunk including InputJSON secret")
	}

	// Shut down so all sink deliveries have flushed, then inspect the sink.
	shutdown(t, l)

	var sinkSawToolUse bool
	for _, env := range sink.events() {
		td, ok := env.Event.(event.TokenDelta)
		if !ok {
			continue
		}
		tu, ok := td.Chunk.(*content.ToolUseChunk)
		if !ok {
			continue
		}
		sinkSawToolUse = true
		if strings.Contains(tu.InputJSON, secret) {
			t.Errorf("SECURITY: sink received ToolUseChunk.InputJSON %q containing the secret", tu.InputJSON)
		}
		if tu.InputJSON != "" {
			t.Errorf("sink ToolUseChunk.InputJSON = %q, want empty (dropped)", tu.InputJSON)
		}
		if tu.Name != "Bash" {
			t.Errorf("sink ToolUseChunk.Name = %q, want Bash (kept)", tu.Name)
		}
	}
	if !sinkSawToolUse {
		t.Fatal("sink never received the projected ToolUseChunk TokenDelta")
	}

	// Belt-and-suspenders: the secret must not appear anywhere in any sink envelope.
	for _, env := range sink.events() {
		if td, ok := env.Event.(event.TokenDelta); ok {
			if strings.Contains(encodeSink(t, td), secret) {
				t.Errorf("SECURITY: secret leaked into a sink TokenDelta: %q", encodeSink(t, td))
			}
		}
	}
}

// shutdown sends a Shutdown and waits for Done, guaranteeing every queued sink
// delivery has flushed before the test inspects the recording sink.
func shutdown(t *testing.T, l *Loop) {
	t.Helper()
	ack := make(chan error, 1)
	l.Commands <- command.Shutdown{Ack: ack}
	if err := <-ack; err != nil {
		t.Fatalf("Shutdown ack = %v", err)
	}
	<-l.Done
}
