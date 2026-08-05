package loopruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	model "github.com/looprig/inference/model"
)

// seededUser builds the committed UserMessage form the loop appends for a turn.
func seededUser(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// seededAI builds the committed AIMessage form a finalized step group carries.
func seededAI(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// TestNewRestored covers the loop seed path the Restore constructor (Task 8.3)
// drives: a loop built with pre-folded committed msgs + turnIndex must come up IDLE
// (it accepts a submit immediately rather than queuing), must seed loopState.msgs
// with the supplied history (proven via the next turn's request base), and must
// number the next turn from the supplied turnIndex (proven via the next
// TurnStarted's TurnIndex).
func TestNewRestored(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		initialMsgs content.AgenticMessages
		initialTurn event.TurnIndex
		wantNextIdx event.TurnIndex // next TurnStarted's TurnIndex
		// wantBaseLen is the number of seeded messages the next turn's request base
		// should carry BEFORE that turn's own initial user message.
		wantBaseLen int
	}{
		{
			name:        "empty history comes up idle, next turn is index 1",
			initialMsgs: content.AgenticMessages{},
			initialTurn: 0,
			wantNextIdx: 1,
			wantBaseLen: 0,
		},
		{
			name: "single committed turn, next turn numbers from the restored index",
			initialMsgs: content.AgenticMessages{
				seededUser("hello"),
				seededAI("hi there"),
			},
			initialTurn: 1,
			wantNextIdx: 2,
			wantBaseLen: 2,
		},
		{
			name: "two committed turns, next turn is index 3",
			initialMsgs: content.AgenticMessages{
				seededUser("first"),
				seededAI("answer one"),
				seededUser("second"),
				seededAI("answer two"),
			},
			initialTurn: 2,
			wantNextIdx: 3,
			wantBaseLen: 4,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			sessionID := mustID(t)
			loopID := mustID(t)
			client := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
			rec := &recordingPublisher{}

			l, err := newRestoredWithConfig(ctx, sessionID, loopID, rec,
				runtimeConfig{Client: client, Model: testModel(), DrainTimeout: 200 * time.Millisecond},
				RestoredState{Msgs: tt.initialMsgs, TurnIndex: tt.initialTurn})
			if err != nil {
				t.Fatalf("NewRestored: %v", err)
			}

			// Idle: a submit starts a turn immediately (it is not queued). startTurn
			// blocks until the loop publishes TurnStarted, so reaching it proves idle.
			startTurn(t, l, rec, []content.Block{&content.TextBlock{Text: "next"}})
			drainToTerminal(t, rec)

			// The next turn numbers from the restored turnIndex.
			var gotIdx event.TurnIndex
			var found bool
			for _, e := range rec.events() {
				if ts, ok := e.(event.TurnStarted); ok {
					gotIdx = ts.TurnIndex
					found = true
					break
				}
			}
			if !found {
				t.Fatal("no TurnStarted published after restore")
			}
			if gotIdx != tt.wantNextIdx {
				t.Errorf("next TurnStarted.TurnIndex = %d, want %d", gotIdx, tt.wantNextIdx)
			}

			// The next turn's request base is the seeded committed history; its first
			// request carries the seeded messages followed by this turn's user message.
			req := client.lastReq()
			if len(req.Messages) != tt.wantBaseLen+1 {
				t.Fatalf("next request had %d messages, want %d (%d seeded + 1 new user)",
					len(req.Messages), tt.wantBaseLen+1, tt.wantBaseLen)
			}
			if tt.wantBaseLen > 0 {
				gotBase := content.AgenticMessages(req.Messages[:tt.wantBaseLen])
				if !reflect.DeepEqual(gotBase, tt.initialMsgs) {
					t.Errorf("seeded request base =\n  %#v\nwant\n  %#v", gotBase, tt.initialMsgs)
				}
			}
		})
	}
}

// TestNewRestored_Validation proves NewRestored runs the SAME construction
// validation as New (missing client / invalid model / nil publisher), so a
// restore with a malformed config fails closed before any actor starts.
func TestNewRestored_Validation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessionID := mustID(t)
	loopID := mustID(t)

	tests := []struct {
		name    string
		cfg     runtimeConfig
		events  eventPublisher
		wantErr ConfigErrorKind
	}{
		{
			name:    "missing client",
			cfg:     runtimeConfig{Model: testModel()},
			events:  &recordingPublisher{},
			wantErr: ConfigMissingClient,
		},
		{
			name:    "nil publisher",
			cfg:     runtimeConfig{Client: &fakeLLM{}, Model: testModel()},
			events:  nil,
			wantErr: ConfigMissingPublisher,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := newRestoredWithConfig(ctx, sessionID, loopID, tt.events, tt.cfg, RestoredState{})
			var ce *ConfigError
			if !errors.As(err, &ce) || ce.Kind != tt.wantErr {
				t.Fatalf("NewRestored err = %v, want *ConfigError{%v}", err, tt.wantErr)
			}
		})
	}
}

// TestLoopSnapshot proves the actor-served Snapshot returns the committed msgs +
// turnIndex without racing the actor (the sole mutator of loopState). A loop seeded
// via NewRestored returns the seeded state; after a completed turn it reflects the
// grown history.
func TestLoopSnapshot(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sessionID := mustID(t)
	loopID := mustID(t)
	seeded := content.AgenticMessages{seededUser("hello"), seededAI("hi")}
	client := &recordingLLM{chunks: []content.Chunk{textChunk("ok")}}
	rec := &recordingPublisher{}

	l, err := newRestoredWithConfig(ctx, sessionID, loopID, rec,
		runtimeConfig{Client: client, Model: testModel(), DrainTimeout: 200 * time.Millisecond},
		RestoredState{Msgs: seeded, TurnIndex: 1})
	if err != nil {
		t.Fatalf("NewRestored: %v", err)
	}

	msgs, idx, err := l.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if idx != 1 {
		t.Errorf("snapshot turnIndex = %d, want 1", idx)
	}
	if !reflect.DeepEqual(msgs, seeded) {
		t.Errorf("snapshot msgs =\n  %#v\nwant\n  %#v", msgs, seeded)
	}
}

// TestNewRestoredSeedsTransportOverride proves NewRestoredWithRuntime (via NewRestored)
// grafts a seeded ModelRuntime's APIFormat/BaseURL onto the restored loop's ACTUAL
// running model, not just the lightweight reported view: a non-empty seeded
// APIFormat/BaseURL overrides the base mode's transport, and a seed that leaves both
// zero (exactly what every pre-Task-3.1 durable record folds to) must leave the base
// model's transport untouched. The seam is the same one TestNewRestoredSeedsModeAndInference
// uses: the recordingLLM's last request carries the actor's real effective model.
func TestNewRestoredSeedsTransportOverride(t *testing.T) {
	t.Parallel()
	base := testModel() // Provider: lmstudio, APIFormat: openai, BaseURL: http://localhost:1234
	tests := []struct {
		name          string
		runtime       event.ModelRuntime
		wantAPIFormat model.APIFormat
		wantBaseURL   string
	}{
		{
			name: "seeded APIFormat/BaseURL override the base transport",
			runtime: event.ModelRuntime{
				Key:       base.Key(),
				APIFormat: model.APIFormatAnthropic,
				BaseURL:   "https://example.com",
			},
			wantAPIFormat: model.APIFormatAnthropic,
			wantBaseURL:   "https://example.com",
		},
		{
			name: "zero seeded APIFormat/BaseURL leave the base transport untouched (regression)",
			runtime: event.ModelRuntime{
				Key: base.Key(),
			},
			wantAPIFormat: base.APIFormat,
			wantBaseURL:   base.BaseURL,
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
			seed := RestoredState{HasRuntime: true, Runtime: tt.runtime}
			l, err := NewRestored(ctx, mustID(t), mustID(t), Provenance{}, rec, bound, seed)
			if err != nil {
				t.Fatalf("NewRestored: %v", err)
			}
			runOneTurn(t, l, rec, "turn1")
			got := llm.lastReq().Model
			if got.APIFormat != tt.wantAPIFormat || got.BaseURL != tt.wantBaseURL {
				t.Fatalf("restored turn model APIFormat/BaseURL = %q/%q, want %q/%q",
					got.APIFormat, got.BaseURL, tt.wantAPIFormat, tt.wantBaseURL)
			}
		})
	}
}
