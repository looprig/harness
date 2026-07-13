package sessionstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
)

func TestCatalogFoldsLoopUsageExactlyOnce(t *testing.T) {
	t.Parallel()
	sessionID := fixedUUID(0x30)
	loopHigh, loopLow := fixedUUID(0x22), fixedUUID(0x11)
	runtimeA := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider-a", Model: "model-a"}, Limits: inference.ContextLimits{WindowTokens: 100}, Effort: inference.EffortLow}
	runtimeB := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider-b", Model: "model-b"}, Limits: inference.ContextLimits{WindowTokens: 200}, Effort: inference.EffortHigh}
	usageA := content.Usage{InputTokens: 10, OutputTokens: 2}
	usageB := content.Usage{InputTokens: 20, OutputTokens: 4, CacheReadTokens: 5}
	step := func(loopID [16]byte, usage content.Usage) event.StepDone {
		return event.StepDone{
			Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}},
			Messages: content.AgenticMessages{&content.AIMessage{
				Message: content.Message{Role: content.RoleAssistant},
				Usage:   &usage,
			}},
		}
	}
	tests := []struct {
		name      string
		events    []event.Event
		sequences []uint64
		want      []LoopUsageMeta
		wantErrAs *content.UsageOverflowError
	}{
		{
			name: "step usage is accumulated per loop and TurnDone is not added again",
			events: []event.Event{
				event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeA},
				event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopLow}}, Runtime: runtimeA},
				event.LoopModeChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopLow}}, Mode: "build", Runtime: runtimeB},
				step(loopHigh, usageA),
				event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Usage: usageA},
				step(loopHigh, usageB),
				event.LoopInferenceChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeB},
			},
			sequences: []uint64{1, 2, 3, 4, 5, 6, 7},
			want: []LoopUsageMeta{
				{LoopID: loopLow, Runtime: runtimeB},
				{LoopID: loopHigh, Runtime: runtimeB, CumulativeUsage: content.Usage{InputTokens: 30, OutputTokens: 6, CacheReadTokens: 5}},
			},
		},
		{
			name: "replayed StepDone sequence is not accumulated twice",
			events: []event.Event{
				event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeA},
				step(loopHigh, usageA),
				step(loopHigh, usageA),
			},
			sequences: []uint64{1, 2, 2},
			want:      []LoopUsageMeta{{LoopID: loopHigh, Runtime: runtimeA, CumulativeUsage: usageA}},
		},
		{
			name: "overflow is typed and does not corrupt the prior total",
			events: []event.Event{
				event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeA},
				step(loopHigh, content.Usage{InputTokens: content.TokenCount(^uint64(0))}),
				step(loopHigh, content.Usage{InputTokens: 1}),
			},
			sequences: []uint64{1, 2, 3},
			want:      []LoopUsageMeta{{LoopID: loopHigh, Runtime: runtimeA, CumulativeUsage: content.Usage{InputTokens: content.TokenCount(^uint64(0))}}},
			wantErrAs: &content.UsageOverflowError{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var meta SessionMeta
			var gotErr error
			for i, ev := range tt.events {
				var changed bool
				meta, changed, gotErr = applyEvent(meta, ev, tt.sequences[i], fixedClock(time.Time{}))
				if gotErr != nil {
					break
				}
				if !changed {
					t.Fatalf("applyEvent(%T) changed = false", ev)
				}
			}
			if tt.wantErrAs != nil {
				var overflow *content.UsageOverflowError
				if !errors.As(gotErr, &overflow) {
					t.Fatalf("applyEvent error = %T %v, want *UsageOverflowError", gotErr, gotErr)
				}
			} else if gotErr != nil {
				t.Fatalf("applyEvent error = %v", gotErr)
			}
			if !reflect.DeepEqual(meta.Loops, tt.want) {
				t.Errorf("SessionMeta.Loops = %#v, want %#v", meta.Loops, tt.want)
			}
		})
	}
}

func TestCatalogRepairRestoresLoopUsageMetadata(t *testing.T) {
	t.Parallel()
	sessionID, loopID := fixedUUID(0x40), fixedUUID(0x41)
	runtime := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}, Effort: inference.EffortMedium}
	usage := content.Usage{InputTokens: 7, OutputTokens: 3}
	tests := []struct {
		name string
	}{
		{name: "repair and codec preserve loop runtime and cumulative usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, err := Open(memstore.New())
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			opener := &fakeOpener{events: []event.Event{
				event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}},
				event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}, Runtime: runtime},
				event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}, Messages: content.AgenticMessages{&content.AIMessage{Message: content.Message{Role: content.RoleAssistant}, Usage: &usage}}},
				event.TurnDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}, Usage: usage},
			}}
			catalog := store.OpenCatalog(WithCatalogReplayer(opener))
			meta, err := catalog.RepairCatalog(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("RepairCatalog() error = %v", err)
			}
			want := []LoopUsageMeta{{LoopID: loopID, Runtime: runtime, CumulativeUsage: usage}}
			if !reflect.DeepEqual(meta.Loops, want) {
				t.Fatalf("repaired Loops = %#v, want %#v", meta.Loops, want)
			}
			encoded, err := encodeSessionMeta(meta)
			if err != nil {
				t.Fatalf("encodeSessionMeta() error = %v", err)
			}
			decoded, err := decodeSessionMeta(encoded)
			if err != nil {
				t.Fatalf("decodeSessionMeta() error = %v", err)
			}
			if !reflect.DeepEqual(decoded.Loops, want) {
				t.Errorf("codec Loops = %#v, want %#v", decoded.Loops, want)
			}
		})
	}
}
