package loopruntime

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
)

func ownershipMessages() content.AgenticMessages {
	return content.AgenticMessages{
		&content.UserMessage{Message: content.Message{
			Role: content.RoleUser,
			Blocks: []content.Block{
				&content.ImageBlock{MediaType: "image/png", Source: content.ImageSource{Data: []byte{1, 2, 3}}},
			},
		}},
		&content.AIMessage{
			Message: content.Message{
				Role: content.RoleAssistant,
				Blocks: []content.Block{
					&content.ToolUseBlock{ID: "call-1", Name: "Echo", Input: json.RawMessage(`{"value":"original"}`)},
					&content.ToolResultBlock{
						ToolUseID: "nested-1",
						Content: []content.Block{
							&content.AudioBlock{MediaType: "audio/wav", Data: []byte{4, 5, 6}},
							&content.DocumentBlock{MediaType: "application/octet-stream", Name: "doc", Data: []byte{7, 8, 9}},
						},
					},
				},
			},
			Usage: &content.Usage{InputTokens: 11, OutputTokens: 7, ReasoningTokens: 3},
		},
		&content.ToolResultMessage{
			Message: content.Message{
				Role:   content.RoleTool,
				Blocks: []content.Block{&content.TextBlock{Text: "original result"}},
			},
			ToolUseID: "call-1",
		},
	}
}

func mutateOwnershipGraph(msgs content.AgenticMessages) {
	user := msgs[0].(*content.UserMessage)
	user.Blocks[0].(*content.ImageBlock).Source.Data[0] = 91
	ai := msgs[1].(*content.AIMessage)
	ai.Usage.InputTokens = 99
	ai.Blocks[0].(*content.ToolUseBlock).Input[0] = '['
	nested := ai.Blocks[1].(*content.ToolResultBlock)
	nested.Content[0].(*content.AudioBlock).Data[0] = 92
	nested.Content[1].(*content.DocumentBlock).Data[0] = 93
	msgs[2].(*content.ToolResultMessage).Blocks[0].(*content.TextBlock).Text = "mutated result"
}

func ownershipStepMessages() content.AgenticMessages {
	messages := ownershipMessages()
	return content.AgenticMessages{messages[1], messages[2]}
}

func mutateOwnershipStepGraph(msgs content.AgenticMessages) {
	ai := msgs[0].(*content.AIMessage)
	ai.Usage.InputTokens = 99
	ai.Blocks[0].(*content.ToolUseBlock).Input[0] = '['
	nested := ai.Blocks[1].(*content.ToolResultBlock)
	nested.Content[0].(*content.AudioBlock).Data[0] = 92
	nested.Content[1].(*content.DocumentBlock).Data[0] = 93
	msgs[1].(*content.ToolResultMessage).Blocks[0].(*content.TextBlock).Text = "mutated result"
}

func TestCommitStepOwnsStepDoneAndHistoryGraphs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(event.StepDone, content.AgenticMessages)
		target string
	}{
		{
			name: "mutating StepDone leaves committed history unchanged",
			mutate: func(done event.StepDone, _ content.AgenticMessages) {
				mutateOwnershipStepGraph(done.Messages)
			},
			target: "event",
		},
		{
			name: "mutating committed history leaves StepDone unchanged",
			mutate: func(_ event.StepDone, committed content.AgenticMessages) {
				mutateOwnershipStepGraph(committed)
			},
			target: "history",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			original := ownershipStepMessages()
			want := ownershipStepMessages()
			var got turnCommit
			cfg := turnConfig{commit: func(_ context.Context, commit turnCommit) error {
				got = commit
				return nil
			}}
			if err := commitStep(context.Background(), cfg, stepState{msgs: original}); err != nil {
				t.Fatalf("commitStep() error = %v", err)
			}
			done, ok := got.Event.(event.StepDone)
			if !ok {
				t.Fatalf("commit event = %T, want event.StepDone", got.Event)
			}

			tt.mutate(done, got.Messages)

			if !reflect.DeepEqual(original, want) {
				t.Errorf("source step changed through public boundary\n got: %#v\nwant: %#v", original, want)
			}
			if tt.target != "event" && !reflect.DeepEqual(done.Messages, want) {
				t.Errorf("StepDone changed through another owner\n got: %#v\nwant: %#v", done.Messages, want)
			}
			if tt.target != "history" && !reflect.DeepEqual(got.Messages, want) {
				t.Errorf("committed history changed through another owner\n got: %#v\nwant: %#v", got.Messages, want)
			}
		})
	}
}

func TestRunTurnOwnsTurnDoneStepDoneAndHistoryGraphs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*content.AIMessage)
		target string
	}{
		{
			name: "mutating TurnDone message leaves StepDone and history unchanged",
			mutate: func(message *content.AIMessage) {
				message.Usage.InputTokens = 99
				message.Blocks[0].(*content.TextBlock).Text = "mutated"
			},
			target: "turn",
		},
		{
			name: "mutating StepDone message leaves TurnDone and history unchanged",
			mutate: func(message *content.AIMessage) {
				message.Usage.OutputTokens = 99
				message.Blocks[0].(*content.TextBlock).Text = "mutated"
			},
			target: "step",
		},
		{
			name: "mutating committed history leaves terminal events unchanged",
			mutate: func(message *content.AIMessage) {
				message.Usage.ReasoningTokens = 1
				message.Blocks[0].(*content.TextBlock).Text = "mutated"
			},
			target: "history",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			usage := content.Usage{InputTokens: 11, OutputTokens: 7, ReasoningTokens: 3}
			client := &scriptedLLM{
				scripts: [][]content.Chunk{{textChunk("original")}},
				results: []*inference.StreamResult{{Usage: &usage}},
			}
			cfg, state, recorder := newTurnFixture(
				[]content.Block{&content.TextBlock{Text: "go"}}, nil, ToolSet{}, client, noGateReg(),
			)
			terminal := runTurn(context.Background(), cfg, state)
			done, ok := terminal.(event.TurnDone)
			if !ok {
				t.Fatalf("runTurn() terminal = %T, want event.TurnDone", terminal)
			}
			steps := stepDones(recorder.events())
			if len(steps) != 1 {
				t.Fatalf("StepDone count = %d, want 1", len(steps))
			}
			stepAI := steps[0].Messages[0].(*content.AIMessage)
			committedAI := recorder.committedMsgs()[0].(*content.AIMessage)

			switch tt.target {
			case "turn":
				tt.mutate(done.Message)
			case "step":
				tt.mutate(stepAI)
			case "history":
				tt.mutate(committedAI)
			default:
				t.Fatalf("unknown target %q", tt.target)
			}

			wantUsage := content.Usage{InputTokens: 11, OutputTokens: 7, ReasoningTokens: 3}
			for name, message := range map[string]*content.AIMessage{
				"TurnDone": done.Message,
				"StepDone": stepAI,
				"history":  committedAI,
			} {
				if name == map[string]string{"turn": "TurnDone", "step": "StepDone", "history": "history"}[tt.target] {
					continue
				}
				if message.Blocks[0].(*content.TextBlock).Text != "original" {
					t.Errorf("%s text changed through %s mutation", name, tt.target)
				}
				if message.Usage == nil || !reflect.DeepEqual(*message.Usage, wantUsage) {
					t.Errorf("%s usage = %+v, want %+v after %s mutation", name, message.Usage, wantUsage, tt.target)
				}
			}
		})
	}
}

func TestSnapshotOwnsRestoredAndReturnedGraphs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(content.AgenticMessages, content.AgenticMessages)
	}{
		{
			name:   "mutating restored seed after construction leaves actor history unchanged",
			mutate: func(seed, _ content.AgenticMessages) { mutateOwnershipGraph(seed) },
		},
		{
			name:   "mutating returned snapshot leaves actor history unchanged",
			mutate: func(_, snapshot content.AgenticMessages) { mutateOwnershipGraph(snapshot) },
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			seed := ownershipMessages()
			want := ownershipMessages()
			loop, err := newRestoredWithConfig(
				ctx,
				mustID(t),
				mustID(t),
				&recordingPublisher{},
				runtimeConfig{Client: &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}, Model: testModel(), DrainTimeout: 200 * time.Millisecond},
				RestoredState{Msgs: seed},
			)
			if err != nil {
				t.Fatalf("newRestoredWithConfig() error = %v", err)
			}
			first, _, err := loop.Snapshot(ctx)
			if err != nil {
				t.Fatalf("first Snapshot() error = %v", err)
			}

			tt.mutate(seed, first)

			second, _, err := loop.Snapshot(ctx)
			if err != nil {
				t.Fatalf("second Snapshot() error = %v", err)
			}
			if !reflect.DeepEqual(second, want) {
				t.Errorf("actor history changed through public boundary\n got: %#v\nwant: %#v", second, want)
			}
		})
	}
}

func TestTurnStartedOwnsCommandAndHistoryGraphs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func([]content.Block, *content.UserMessage)
	}{
		{
			name: "mutating submitted command after completion leaves history unchanged",
			mutate: func(blocks []content.Block, _ *content.UserMessage) {
				blocks[0].(*content.ImageBlock).Source.Data[0] = 99
			},
		},
		{
			name: "mutating TurnStarted message after completion leaves history unchanged",
			mutate: func(_ []content.Block, started *content.UserMessage) {
				started.Blocks[0].(*content.ImageBlock).Source.Data[0] = 99
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			loop, recorder, _ := newLoop(t, &fakeLLM{chunks: []content.Chunk{textChunk("done")}})
			blocks := []content.Block{&content.ImageBlock{Source: content.ImageSource{Data: []byte{1, 2, 3}}}}
			inputID := mustID(t)
			loop.Commands <- command.UserInput{Header: command.Header{CommandID: inputID}, Blocks: blocks}
			reply := awaitReply(t, recorder, inputID)
			startedEvent, ok := reply.(event.TurnStarted)
			if !ok {
				t.Fatalf("submit result = %T, want event.TurnStarted", reply)
			}
			if _, ok := drainToTerminal(t, recorder).(event.TurnDone); !ok {
				t.Fatal("terminal is not event.TurnDone")
			}

			tt.mutate(blocks, startedEvent.Message)

			snapshot, _, err := loop.Snapshot(context.Background())
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			user := snapshot[0].(*content.UserMessage)
			if got := user.Blocks[0].(*content.ImageBlock).Source.Data[0]; got != 1 {
				t.Errorf("history image byte = %d, want 1", got)
			}
		})
	}
}

func TestTurnFoldedIntoOwnsEventAndHistoryGraphs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*content.UserMessage, *content.UserMessage)
		target string
	}{
		{
			name: "mutating TurnFoldedInto message leaves committed history unchanged",
			mutate: func(folded, _ *content.UserMessage) {
				folded.Blocks[0].(*content.ImageBlock).Source.Data[0] = 99
			},
			target: "event",
		},
		{
			name: "mutating committed fold leaves TurnFoldedInto unchanged",
			mutate: func(_, committed *content.UserMessage) {
				committed.Blocks[0].(*content.ImageBlock).Source.Data[0] = 99
			},
			target: "history",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := &content.UserMessage{Message: content.Message{
				Role:   content.RoleUser,
				Blocks: []content.Block{&content.ImageBlock{Source: content.ImageSource{Data: []byte{1, 2, 3}}}},
			}}
			recorder := &turnRecorder{drainBatches: [][]queuedInput{{{msg: source}}}}
			cfg := turnConfig{commit: recorder.commit, drainPending: recorder.drainPending}
			state := turnState{}
			if err := foldPending(context.Background(), cfg, &state); err != nil {
				t.Fatalf("foldPending() error = %v", err)
			}
			if len(recorder.commits) != 1 {
				t.Fatalf("commit count = %d, want 1", len(recorder.commits))
			}
			folded, ok := recorder.commits[0].Event.(event.TurnFoldedInto)
			if !ok {
				t.Fatalf("commit event = %T, want event.TurnFoldedInto", recorder.commits[0].Event)
			}
			committed := recorder.commits[0].Messages[0].(*content.UserMessage)

			tt.mutate(folded.Message, committed)

			if got := source.Blocks[0].(*content.ImageBlock).Source.Data[0]; got != 1 {
				t.Errorf("staged source image byte = %d, want 1", got)
			}
			if tt.target != "event" {
				if got := folded.Message.Blocks[0].(*content.ImageBlock).Source.Data[0]; got != 1 {
					t.Errorf("TurnFoldedInto image byte = %d, want 1", got)
				}
			}
			if tt.target != "history" {
				if got := committed.Blocks[0].(*content.ImageBlock).Source.Data[0]; got != 1 {
					t.Errorf("committed image byte = %d, want 1", got)
				}
			}
		})
	}
}

func TestRequestMessagesOwnsInputGraphs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		base        content.AgenticMessages
		staged      content.AgenticMessages
		runtimeTail *content.UserMessage
		mutateAt    int
		wantBase    content.AgenticMessages
		wantStaged  content.AgenticMessages
		wantTail    *content.UserMessage
	}{
		{
			name:     "committed base is isolated from inference request",
			base:     ownershipMessages(),
			mutateAt: 0,
			wantBase: ownershipMessages(),
		},
		{
			name:       "staged history is isolated from inference request",
			staged:     ownershipMessages(),
			mutateAt:   1,
			wantStaged: ownershipMessages(),
		},
		{
			name:        "runtime tail is isolated from inference request",
			runtimeTail: &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.ImageBlock{Source: content.ImageSource{Data: []byte{1, 2, 3}}}}}},
			mutateAt:    0,
			wantTail:    &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.ImageBlock{Source: content.ImageSource{Data: []byte{1, 2, 3}}}}}},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			request := requestMessages(tt.base, tt.staged, tt.runtimeTail)
			switch message := request[tt.mutateAt].(type) {
			case *content.UserMessage:
				message.Blocks[0].(*content.ImageBlock).Source.Data[0] = 99
			case *content.AIMessage:
				message.Usage.InputTokens = 99
			default:
				t.Fatalf("request message = %T, want mutable user or AI", message)
			}
			if !reflect.DeepEqual(tt.base, tt.wantBase) {
				t.Errorf("base changed through inference request")
			}
			if !reflect.DeepEqual(tt.staged, tt.wantStaged) {
				t.Errorf("staged changed through inference request")
			}
			if !reflect.DeepEqual(tt.runtimeTail, tt.wantTail) {
				t.Errorf("runtime tail changed through inference request")
			}
		})
	}
}
