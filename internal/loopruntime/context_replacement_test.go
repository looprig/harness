package loopruntime

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
)

func TestContextReplacementIsNotAPublicLoopCapability(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "Loop exports no arbitrary context mutation method"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loopType := reflect.TypeOf((*Loop)(nil))
			for index := 0; index < loopType.NumMethod(); index++ {
				name := loopType.Method(index).Name
				if strings.Contains(name, "Replace") || strings.Contains(name, "Rewrite") || strings.Contains(name, "SetMessages") {
					t.Fatalf("public Loop method %q exposes arbitrary context mutation", name)
				}
			}
		})
	}
}

func TestPrepareActorContextReplacementUsesFullMeasurementCAS(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*loopState, *compactionPreparedSuccess)
	}{
		{
			name: "stale basis",
			mutate: func(state *loopState, _ *compactionPreparedSuccess) {
				state.context.Basis = event.ContextBasis{Revision: 90, ThroughEventID: uuid.UUID{90}}
				state.contextTracker.basis = state.context.Basis
			},
		},
		{
			name: "stale model",
			mutate: func(state *loopState, _ *compactionPreparedSuccess) {
				state.context.Model = inference.ModelKey{Provider: "test", Model: "changed"}
			},
		},
		{
			name: "stale request fingerprint",
			mutate: func(state *loopState, _ *compactionPreparedSuccess) {
				state.context.RequestFingerprint = [32]byte{91}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, attempt, success, settings := validActorReplacementFixture(t)
			tt.mutate(&state, success)
			before := cloneReplacementTestState(state)

			_, err := prepareActorContextReplacement(state, attempt, success, settings)
			var stale *StaleCompactionError
			if !errors.As(err, &stale) {
				t.Fatalf("prepareActorContextReplacement() error = %T %v, want StaleCompactionError", err, err)
			}
			if !reflect.DeepEqual(state, before) {
				t.Fatal("failed CAS mutated actor state")
			}
		})
	}
}

func TestActorContextReplacementResetsCommittedStateAndPreservesQueue(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "committed replacement owns summary and leaves uncommitted input queued"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, attempt, success, settings := validActorReplacementFixture(t)
			queuedBefore := cloneMessages(content.AgenticMessages{state.inbox[0].msg})[0].(*content.UserMessage)
			plan, err := prepareActorContextReplacement(state, attempt, success, settings)
			if err != nil {
				t.Fatalf("prepareActorContextReplacement() error = %v", err)
			}
			committed := event.CompactionCommitted{
				Header: event.Header{EventID: uuid.UUID{70}}, AttemptID: attempt.AttemptID, Basis: attempt.Basis,
				Summary: cloneUserMessage(success.Summary), PostContext: validFinalizationMeasurement(70),
			}

			plan.apply(&state, committed)

			if len(state.msgs) != 1 || !reflect.DeepEqual(state.msgs[0], committed.Summary) {
				t.Fatalf("actor messages = %#v, want only committed summary", state.msgs)
			}
			if state.context != committed.PostContext || !state.hasContext || state.contextTracker.currentBasis() != committed.PostContext.Basis {
				t.Fatalf("actor context = %+v has=%v basis=%+v, want committed PostContext", state.context, state.hasContext, state.contextTracker.currentBasis())
			}
			if len(state.inbox) != 1 || !reflect.DeepEqual(state.inbox[0].msg, queuedBefore) {
				t.Fatalf("queued input changed: %#v", state.inbox)
			}
			committed.Summary.Blocks[0].(*content.TextBlock).Text = "mutated outside actor"
			if state.msgs[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text == "mutated outside actor" {
				t.Fatal("actor replacement aliases committed event summary")
			}
		})
	}
}

func TestTurnContextReplacementResetsOnlyRequestHistory(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "base and staged messages reset while turn counters and identity survive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := validFinalizationSummary()
			cfg := turnConfig{base: content.AgenticMessages{replacementTestMessage("old base")}}
			state := turnState{
				sessionID: uuid.UUID{1}, loopID: uuid.UUID{2}, id: uuid.UUID{3}, index: 4, causationID: uuid.UUID{5},
				msgs: content.AgenticMessages{replacementTestMessage("old staged")}, usage: content.Usage{InputTokens: 6},
				toolIterations: 7, toolCalls: 8,
			}

			applyTurnContextReplacement(&cfg, &state, turnContextReplacement{Summary: summary})

			if len(cfg.base) != 0 {
				t.Fatalf("turn base = %#v, want empty", cfg.base)
			}
			request := requestMessages(cfg.base, state.msgs, nil)
			if len(request) != 1 || !reflect.DeepEqual(request[0], summary) {
				t.Fatalf("next request = %#v, want only validated summary", request)
			}
			if state.sessionID != (uuid.UUID{1}) || state.loopID != (uuid.UUID{2}) || state.id != (uuid.UUID{3}) || state.index != 4 || state.causationID != (uuid.UUID{5}) ||
				state.usage.InputTokens != 6 || state.toolIterations != 7 || state.toolCalls != 8 {
				t.Fatalf("non-context turn state changed: %+v", state)
			}
			summary.Blocks[0].(*content.TextBlock).Text = "mutated outside turn"
			if state.msgs[0].(*content.UserMessage).Blocks[0].(*content.TextBlock).Text == "mutated outside turn" {
				t.Fatal("turn replacement aliases handshake summary")
			}
		})
	}
}

func validActorReplacementFixture(t *testing.T) (loopState, compactionAttempt, *compactionPreparedSuccess, contextAdmissionSettings) {
	t.Helper()
	attempt := validFinalizationAttempt()
	measurement := validFinalizationMeasurement(4)
	measurement.Basis = attempt.Basis
	settings := contextAdmissionSettings{ReservedOutput: 10, CompactAt: 8_000, RearmBelow: 6_000}
	state := newLoopState(uuid.UUID{10}, uuid.UUID{11}, Provenance{})
	state.msgs = content.AgenticMessages{replacementTestMessage("old committed")}
	state.context = measurement
	state.hasContext = true
	if err := state.contextTracker.restore(attempt.Basis, true, measurement, true, event.ContextBasis{}, false, settings); err != nil {
		t.Fatalf("contextTracker.restore() error = %v", err)
	}
	state.inbox = []queuedInput{{inputID: uuid.UUID{12}, msg: replacementTestMessage("queued")}}
	success := &compactionPreparedSuccess{
		Model: measurement.Model, RequestFingerprint: measurement.RequestFingerprint,
		Summary: validFinalizationSummary(), PostContext: validFinalizationMeasurement(13),
	}
	return state, attempt, success, settings
}

func cloneReplacementTestState(state loopState) loopState {
	state.msgs = cloneMessages(state.msgs)
	state.inbox = append([]queuedInput(nil), state.inbox...)
	for index := range state.inbox {
		state.inbox[index].msg = cloneUserMessage(state.inbox[index].msg)
	}
	return state
}

func replacementTestMessage(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: text}}}}
}
