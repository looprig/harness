package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
)

func drainEmit(events *[]Event) func(Event) {
	return func(ev Event) { *events = append(*events, ev) }
}

func TestRunTurn(t *testing.T) {
	t.Parallel()
	cfg := Config{Model: llm.ModelSpec{Model: "m"}}
	input := []content.Block{&content.TextBlock{Text: "hi"}}

	t.Run("success appends user+assistant and returns TurnDone", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: []content.Chunk{textChunk("hel"), textChunk("lo")}}
		var emitted []Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, drainEmit(&emitted))

		done, ok := terminal.(TurnDone)
		if !ok {
			t.Fatalf("terminal = %T, want TurnDone", terminal)
		}
		if len(msgs) != 2 {
			t.Fatalf("history len = %d, want 2 (user, assistant)", len(msgs))
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
	})

	t.Run("stream error rolls back user message, TurnFailed carries typed cause", func(t *testing.T) {
		t.Parallel()
		boom := &llm.ValidationError{Field: "x", Reason: "boom"}
		client := &fakeLLM{streamErr: boom}
		var emitted []Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, drainEmit(&emitted))

		failed, ok := terminal.(TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ve *llm.ValidationError
		if !errors.As(failed.Err, &ve) {
			t.Fatalf("TurnFailed.Err = %T, want *llm.ValidationError via errors.As", failed.Err)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})

	t.Run("empty response rolls back and returns EmptyResponseError", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: nil}
		var emitted []Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, drainEmit(&emitted))

		failed, ok := terminal.(TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ere *EmptyResponseError
		if !errors.As(failed.Err, &ere) {
			t.Fatalf("TurnFailed.Err = %T, want *EmptyResponseError", failed.Err)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})

	t.Run("cancelled context rolls back and returns TurnInterrupted", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := &fakeLLM{streamErr: context.Canceled}
		var emitted []Event
		msgs, terminal := runTurn(ctx, input, 1, nil, cfg, client, drainEmit(&emitted))

		if _, ok := terminal.(TurnInterrupted); !ok {
			t.Fatalf("terminal = %T, want TurnInterrupted", terminal)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})

	t.Run("mid-stream Next error rolls back with typed cause", func(t *testing.T) {
		t.Parallel()
		boom := &llm.ValidationError{Field: "y", Reason: "midstream"}
		client := &fakeLLM{chunks: []content.Chunk{textChunk("partial")}, nextErr: boom}
		var emitted []Event
		msgs, terminal := runTurn(context.Background(), input, 1, nil, cfg, client, drainEmit(&emitted))

		failed, ok := terminal.(TurnFailed)
		if !ok {
			t.Fatalf("terminal = %T, want TurnFailed", terminal)
		}
		var ve *llm.ValidationError
		if !errors.As(failed.Err, &ve) {
			t.Fatalf("TurnFailed.Err = %T, want *llm.ValidationError", failed.Err)
		}
		if len(msgs) != 0 {
			t.Errorf("history len = %d, want 0 (user rolled back)", len(msgs))
		}
	})

	t.Run("emits TurnStarted then TokenDelta per chunk", func(t *testing.T) {
		t.Parallel()
		client := &fakeLLM{chunks: []content.Chunk{textChunk("a"), textChunk("b")}}
		var emitted []Event
		runTurn(context.Background(), input, 1, nil, cfg, client, drainEmit(&emitted))
		if len(emitted) < 1 {
			t.Fatal("no events emitted")
		}
		if _, ok := emitted[0].(TurnStarted); !ok {
			t.Errorf("first event = %T, want TurnStarted", emitted[0])
		}
		var deltas int
		for _, e := range emitted {
			if _, ok := e.(TokenDelta); ok {
				deltas++
			}
		}
		if deltas != 2 {
			t.Errorf("TokenDelta count = %d, want 2", deltas)
		}
	})
}
