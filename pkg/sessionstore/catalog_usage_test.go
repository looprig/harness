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
				{LoopID: loopLow, Runtime: runtimeB, RuntimeSeq: 3, RuntimeValueSeq: 3, ContextSeq: 3},
				{LoopID: loopHigh, Runtime: runtimeB, RuntimeSeq: 7, RuntimeValueSeq: 7, CumulativeUsage: content.Usage{InputTokens: 30, OutputTokens: 6, CacheReadTokens: 5}, ContextSeq: 7},
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
			want:      []LoopUsageMeta{{LoopID: loopHigh, Runtime: runtimeA, RuntimeSeq: 1, RuntimeValueSeq: 1, CumulativeUsage: usageA, ContextSeq: 2}},
		},
		{
			name: "delayed older lifecycle cannot regress a newer runtime",
			events: []event.Event{
				event.LoopInferenceChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeB},
				event.LoopInferenceChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeA},
			},
			sequences: []uint64{10, 5},
			want:      []LoopUsageMeta{{LoopID: loopHigh, Runtime: runtimeB, RuntimeSeq: 10, RuntimeValueSeq: 10, ContextSeq: 10}},
		},
		{
			name: "legacy missing runtime cannot blank a known runtime",
			events: []event.Event{
				event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeA},
				event.LoopModeChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Mode: "legacy-build"},
			},
			sequences: []uint64{1, 2},
			want:      []LoopUsageMeta{{LoopID: loopHigh, Runtime: runtimeA, RuntimeSeq: 2, RuntimeValueSeq: 1, ContextSeq: 2}},
		},
		{
			name: "legacy lifecycle watermark accepts a newer delayed known inference",
			events: []event.Event{
				event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeA},
				event.LoopModeChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Mode: "legacy-build"},
				event.LoopInferenceChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeB},
			},
			sequences: []uint64{1, 10, 5},
			want:      []LoopUsageMeta{{LoopID: loopHigh, Runtime: runtimeB, RuntimeSeq: 10, RuntimeValueSeq: 5, ContextSeq: 10}},
		},
		{
			name: "newer legacy watermark retains the newest delayed known runtime",
			events: []event.Event{
				event.LoopModeChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Mode: "legacy-build"},
				event.LoopInferenceChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeA},
				event.LoopInferenceChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeB},
			},
			sequences: []uint64{10, 5, 8},
			want:      []LoopUsageMeta{{LoopID: loopHigh, Runtime: runtimeB, RuntimeSeq: 10, RuntimeValueSeq: 8, ContextSeq: 10}},
		},
		{
			name: "older delayed known runtime cannot replace a newer backfill",
			events: []event.Event{
				event.LoopModeChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Mode: "legacy-build"},
				event.LoopInferenceChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeB},
				event.LoopInferenceChanged{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeA},
			},
			sequences: []uint64{10, 8, 5},
			want:      []LoopUsageMeta{{LoopID: loopHigh, Runtime: runtimeB, RuntimeSeq: 10, RuntimeValueSeq: 8, ContextSeq: 10}},
		},
		{
			name: "overflow is typed and does not corrupt the prior total",
			events: []event.Event{
				event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopHigh}}, Runtime: runtimeA},
				step(loopHigh, content.Usage{InputTokens: content.TokenCount(^uint64(0))}),
				step(loopHigh, content.Usage{InputTokens: 1}),
			},
			sequences: []uint64{1, 2, 3},
			want:      []LoopUsageMeta{{LoopID: loopHigh, Runtime: runtimeA, RuntimeSeq: 1, RuntimeValueSeq: 1, CumulativeUsage: content.Usage{InputTokens: content.TokenCount(^uint64(0))}, ContextSeq: 2}},
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

func TestCatalogRepairsAmbiguousStepOrdering(t *testing.T) {
	t.Parallel()
	sessionID, loopID := fixedUUID(0x50), fixedUUID(0x51)
	runtime := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}}
	usageA := content.Usage{InputTokens: 3, OutputTokens: 1}
	usageB := content.Usage{InputTokens: 7, OutputTokens: 2}
	step := func(usage content.Usage) event.StepDone {
		return event.StepDone{
			Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}},
			Messages: content.AgenticMessages{&content.AIMessage{
				Message: content.Message{Role: content.RoleAssistant},
				Usage:   &usage,
			}},
		}
	}
	start := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}}
	loopStart := event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}, Runtime: runtime}
	first, second := step(usageA), step(usageB)
	tests := []struct {
		name       string
		deliveries []struct {
			ev  event.Event
			seq uint64
		}
		want content.Usage
	}{
		{
			name: "delayed unique step repairs from the ordered journal",
			deliveries: []struct {
				ev  event.Event
				seq uint64
			}{
				{ev: start, seq: 1},
				{ev: loopStart, seq: 2},
				{ev: second, seq: 4},
				{ev: first, seq: 3},
			},
			want: content.Usage{InputTokens: 10, OutputTokens: 3},
		},
		{
			name: "repeated older step repairs without double counting",
			deliveries: []struct {
				ev  event.Event
				seq uint64
			}{
				{ev: start, seq: 1},
				{ev: loopStart, seq: 2},
				{ev: first, seq: 3},
				{ev: second, seq: 4},
				{ev: first, seq: 3},
			},
			want: content.Usage{InputTokens: 10, OutputTokens: 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, err := Open(memstore.New())
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			catalog := store.OpenCatalog(WithCatalogReplayer(&fakeOpener{events: []event.Event{start, loopStart, first, second}}))
			for _, delivery := range tt.deliveries {
				if err := catalog.UpdateOnEvent(context.Background(), delivery.ev, delivery.seq); err != nil {
					t.Fatalf("UpdateOnEvent(%T, %d) error = %v", delivery.ev, delivery.seq, err)
				}
			}
			meta, found, err := catalog.ReadMeta(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("ReadMeta() error = %v", err)
			}
			if !found || len(meta.Loops) != 1 {
				t.Fatalf("ReadMeta() found=%v loops=%#v, want one loop", found, meta.Loops)
			}
			if meta.Loops[0].CumulativeUsage != tt.want {
				t.Errorf("CumulativeUsage = %+v, want %+v", meta.Loops[0].CumulativeUsage, tt.want)
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
			want := []LoopUsageMeta{{LoopID: loopID, Runtime: runtime, RuntimeSeq: 2, RuntimeValueSeq: 2, CumulativeUsage: usage, ContextSeq: 3}}
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

func TestCatalogRepairFoldsLegacyLifecycleWire(t *testing.T) {
	t.Parallel()
	sessionID, loopID := fixedUUID(0x60), fixedUUID(0x61)
	eventID := fixedUUID(0x62)
	prefix := `,"v":1,"session_id":"` + sessionID.String() + `","loop_id":"` + loopID.String() + `","event_id":"` + eventID.String() + `"`
	current := event.ModelRuntime{Key: inference.ModelKey{Provider: "current", Model: "current-model"}, Limits: inference.ContextLimits{WindowTokens: 100}}
	migrated := event.ModelRuntime{Key: inference.ModelKey{Provider: "legacy", Model: "legacy-model"}, Limits: inference.ContextLimits{WindowTokens: 64_000}, Effort: inference.EffortLow}
	tests := []struct {
		name       string
		seed       event.Event
		legacyWire string
		want       LoopUsageMeta
	}{
		{
			name:       "old inference wire migrates into repaired runtime metadata",
			seed:       event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}},
			legacyWire: `{"type":"LoopInferenceChanged"` + prefix + `,"model":{"Provider":"legacy","Name":"legacy-model","Caps":{"MaxContext":64000}},"effort":"low"}`,
			want:       LoopUsageMeta{LoopID: loopID, Runtime: migrated, RuntimeSeq: 3, RuntimeValueSeq: 3, ContextSeq: 3},
		},
		{
			name:       "old mode wire preserves known runtime while advancing ordering",
			seed:       event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}, Runtime: current},
			legacyWire: `{"type":"LoopModeChanged"` + prefix + `,"previous_mode":"plan","mode":"build"}`,
			want:       LoopUsageMeta{LoopID: loopID, Runtime: current, RuntimeSeq: 3, RuntimeValueSeq: 2, ContextSeq: 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			legacy, err := event.UnmarshalEvent([]byte(tt.legacyWire))
			if err != nil {
				t.Fatalf("UnmarshalEvent() error = %v", err)
			}
			store, err := Open(memstore.New())
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			opener := &fakeOpener{events: []event.Event{
				event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}},
				tt.seed,
				legacy,
			}}
			meta, err := store.OpenCatalog(WithCatalogReplayer(opener)).RepairCatalog(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("RepairCatalog() error = %v", err)
			}
			if len(meta.Loops) != 1 || !reflect.DeepEqual(meta.Loops[0], tt.want) {
				t.Errorf("repaired Loops = %#v, want %#v", meta.Loops, []LoopUsageMeta{tt.want})
			}
		})
	}
}
