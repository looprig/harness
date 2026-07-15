package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
)

func TestSessionMetaRejectsInvalidLoopProjection(t *testing.T) {
	t.Parallel()
	loopLow, loopHigh := fixedUUID(0x11), fixedUUID(0x22)
	validRuntime := event.ModelRuntime{
		Key:    inference.ModelKey{Provider: "provider", Model: "model"},
		Limits: inference.ContextLimits{WindowTokens: 100},
		Effort: inference.EffortLow,
	}
	validLoop := LoopUsageMeta{LoopID: loopLow, Runtime: validRuntime, RuntimeSeq: 2, RuntimeValueSeq: 1}
	tests := []struct {
		name string
		meta SessionMeta
	}{
		{name: "zero loop id", meta: SessionMeta{Loops: []LoopUsageMeta{{}}}},
		{name: "loops are not sorted", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopHigh}, {LoopID: loopLow}}}},
		{name: "duplicate loop id", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopLow}, {LoopID: loopLow}}}},
		{name: "runtime value sequence exceeds lifecycle sequence", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopLow, RuntimeSeq: 1, RuntimeValueSeq: 2}}}},
		{name: "legacy zero runtime has a value sequence", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopLow, RuntimeSeq: 2, RuntimeValueSeq: 1}}}},
		{name: "runtime provider is empty", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopLow, Runtime: event.ModelRuntime{Key: inference.ModelKey{Model: "model"}}, RuntimeSeq: 1, RuntimeValueSeq: 1}}}},
		{name: "runtime model is empty", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopLow, Runtime: event.ModelRuntime{Key: inference.ModelKey{Provider: "provider"}}, RuntimeSeq: 1, RuntimeValueSeq: 1}}}},
		{name: "runtime limits are invalid", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopLow, Runtime: event.ModelRuntime{Key: validRuntime.Key, Limits: inference.ContextLimits{WindowTokens: 10, MaxInputTokens: 11}}, RuntimeSeq: 1, RuntimeValueSeq: 1}}}},
		{name: "runtime effort is invalid", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopLow, Runtime: event.ModelRuntime{Key: validRuntime.Key, Effort: inference.Effort("invalid")}, RuntimeSeq: 1, RuntimeValueSeq: 1}}}},
		{name: "usage is invalid", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopLow, CumulativeUsage: content.Usage{OutputTokens: 1, ReasoningTokens: 2}}}}},
		{name: "valid projection", meta: SessionMeta{Loops: []LoopUsageMeta{validLoop}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(tt.meta)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			_, decodeErr := decodeSessionMeta(raw)
			_, encodeErr := encodeSessionMeta(tt.meta)
			wantErr := tt.name != "valid projection"
			if (decodeErr != nil) != wantErr {
				t.Errorf("decodeSessionMeta() error = %v, wantErr %v", decodeErr, wantErr)
			}
			if (encodeErr != nil) != wantErr {
				t.Errorf("encodeSessionMeta() error = %v, wantErr %v", encodeErr, wantErr)
			}
			if wantErr {
				var validationErr *CatalogMetaValidationError
				if !errors.As(decodeErr, &validationErr) {
					t.Errorf("decodeSessionMeta() error = %T, want *CatalogMetaValidationError", decodeErr)
				}
				if !errors.As(encodeErr, &validationErr) {
					t.Errorf("encodeSessionMeta() error = %T, want *CatalogMetaValidationError", encodeErr)
				}
			}
		})
	}
}

func TestSessionMetaRejectsDuplicateJSONFields(t *testing.T) {
	t.Parallel()
	loopID := fixedUUID(0x33).String()
	tests := []struct {
		name string
		data string
	}{
		{name: "duplicate root field", data: `{"title":"first","title":"second"}`},
		{name: "case aliased root field", data: `{"title":"first","TITLE":"second"}`},
		{name: "duplicate nested runtime field", data: `{"loops":[{"loop_id":"` + loopID + `","runtime":{"key":{"Provider":"provider","Model":"model"},"limits":{},"effort":"low","effort":"high"},"runtime_seq":1,"runtime_value_seq":1}]}`},
		{name: "case aliased nested model key", data: `{"loops":[{"loop_id":"` + loopID + `","runtime":{"key":{"Provider":"provider","PROVIDER":"other","Model":"model"},"limits":{}},"runtime_seq":1,"runtime_value_seq":1}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeSessionMeta([]byte(tt.data))
			var duplicateErr *CatalogDuplicateFieldError
			if !errors.As(err, &duplicateErr) {
				t.Fatalf("decodeSessionMeta() error = %T %v, want *CatalogDuplicateFieldError", err, err)
			}
		})
	}
}

func TestCatalogReadAllowsOpaqueEventJSONFields(t *testing.T) {
	t.Parallel()
	sessionID, loopID := fixedUUID(0x34), fixedUUID(0x35)
	tests := []struct {
		name  string
		input json.RawMessage
	}{
		{name: "case distinct tool input keys remain opaque", input: json.RawMessage(`{"foo":1,"FOO":2}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			step := event.StepDone{
				Header: event.Header{Coordinates: identity.Coordinates{
					SessionID: sessionID,
					LoopID:    loopID,
					TurnID:    fixedUUID(0x36),
					StepID:    fixedUUID(0x37),
				}, EventID: fixedUUID(0x38)},
				Messages: content.AgenticMessages{&content.AIMessage{Message: content.Message{
					Role: content.RoleAssistant,
					Blocks: []content.Block{&content.ToolUseBlock{
						ID: "call-1", Name: "tool", Input: tt.input,
					}},
				}}},
			}
			summary, err := newEventSummary(step, 2)
			if err != nil {
				t.Fatalf("newEventSummary() error = %v", err)
			}
			meta := SessionMeta{SessionID: sessionID, LastJournalSeq: 2, LastStep: summary}
			encoded, err := encodeSessionMeta(meta)
			if err != nil {
				t.Fatalf("encodeSessionMeta() error = %v", err)
			}
			store, err := Open(memstore.New())
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			key, err := sessionName(sessionID)
			if err != nil {
				t.Fatalf("sessionName() error = %v", err)
			}
			if _, err := store.backend.KV.Put(context.Background(), key, 0, encoded); err != nil {
				t.Fatalf("KV.Put() error = %v", err)
			}
			got, found, err := store.OpenCatalog().ReadMeta(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("ReadMeta() error = %v", err)
			}
			if !found || got.LastStep == nil {
				t.Fatalf("ReadMeta() found=%v LastStep=%#v, want stored opaque event", found, got.LastStep)
			}
		})
	}
}

func TestCatalogRepairReplacesInvalidCachedProjection(t *testing.T) {
	t.Parallel()
	sessionID, loopID := fixedUUID(0x44), fixedUUID(0x45)
	runtime := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}}
	tests := []struct {
		name string
	}{
		{name: "typed read failure remains repairable from journal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, err := Open(memstore.New())
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			key, err := sessionName(sessionID)
			if err != nil {
				t.Fatalf("sessionName() error = %v", err)
			}
			bad := `{"session_id":"` + sessionID.String() + `","loops":[{"loop_id":"` + loopID.String() + `","runtime_seq":1,"runtime_value_seq":2}]}`
			if _, err := store.backend.KV.Put(context.Background(), key, 0, []byte(bad)); err != nil {
				t.Fatalf("KV.Put() error = %v", err)
			}
			catalog := store.OpenCatalog(WithCatalogReplayer(&fakeOpener{events: []event.Event{
				event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}},
				event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}, Runtime: runtime},
			}}))
			_, _, readErr := catalog.ReadMeta(context.Background(), sessionID)
			var catalogReadErr *CatalogReadError
			var validationErr *CatalogMetaValidationError
			if !errors.As(readErr, &catalogReadErr) || !errors.As(readErr, &validationErr) {
				t.Fatalf("ReadMeta() error = %T %v, want typed catalog validation failure", readErr, readErr)
			}
			meta, err := catalog.RepairCatalog(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("RepairCatalog() error = %v", err)
			}
			if len(meta.Loops) != 1 || meta.Loops[0].Runtime != runtime {
				t.Errorf("RepairCatalog() loops = %#v, want repaired runtime %#v", meta.Loops, runtime)
			}
		})
	}
}
