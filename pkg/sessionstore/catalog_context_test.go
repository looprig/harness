package sessionstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
)

func catalogContextMeasurement(seed byte) event.ContextMeasurement {
	return event.ContextMeasurement{
		Basis: event.ContextBasis{Revision: event.ContextRevision(seed), ThroughEventID: uuid.UUID{seed}},
		Model: inference.ModelKey{Provider: "provider", Model: "model"}, RequestFingerprint: [32]byte{seed},
		InputTokens: 80, InputLimit: 100, Quality: inference.CountQualityExactLocal,
	}
}

func TestCatalogFoldsLatestContextMeasurement(t *testing.T) {
	t.Parallel()
	loopID := uuid.UUID{8}
	header := event.Header{Coordinates: identity.Coordinates{SessionID: uuid.UUID{7}, LoopID: loopID}}
	runtime := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}}
	first := catalogContextMeasurement(1)
	second := catalogContextMeasurement(2)
	tests := []struct {
		name  string
		apply []struct {
			ev  event.Event
			seq uint64
		}
		want         event.ContextMeasurement
		wantSeq      uint64
		wantValueSeq uint64
		wantIgnored  bool
	}{
		{name: "latest measurement", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: first}, 2}, {event.ContextMeasured{Header: header, Measurement: second}, 3}}, want: second, wantSeq: 3, wantValueSeq: 3},
		{name: "older delayed measurement ignored", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: second}, 3}, {event.ContextMeasured{Header: header, Measurement: first}, 2}}, want: second, wantSeq: 3, wantValueSeq: 3, wantIgnored: true},
		{name: "runtime change invalidates", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: first}, 2}, {event.LoopInferenceChanged{Header: header, Runtime: runtime}, 3}}, wantSeq: 3},
		{name: "context mutation invalidates", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: first}, 2}, {event.TurnStarted{Header: header}, 3}}, wantSeq: 3},
		{name: "delayed measurement cannot cross mutation watermark", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: first}, 2}, {event.TurnStarted{Header: header}, 5}, {event.ContextMeasured{Header: header, Measurement: second}, 4}}, wantSeq: 5, wantIgnored: true},
		{name: "delayed measurement cannot cross runtime watermark", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: first}, 2}, {event.LoopInferenceChanged{Header: header, Runtime: runtime}, 5}, {event.ContextMeasured{Header: header, Measurement: second}, 4}}, wantSeq: 5, wantIgnored: true},
		{name: "equal measurement sequence is an idempotent duplicate", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: first}, 2}, {event.ContextMeasured{Header: header, Measurement: second}, 2}}, want: first, wantSeq: 2, wantValueSeq: 2, wantIgnored: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := SessionMeta{}
			changed := false
			for _, item := range tt.apply {
				var err error
				meta, changed, err = applyEvent(meta, item.ev, item.seq, func() time.Time { return time.Time{} })
				if err != nil {
					t.Fatal(err)
				}
			}
			if changed == tt.wantIgnored {
				t.Fatalf("final changed = %v, want ignored=%v", changed, tt.wantIgnored)
			}
			if len(meta.Loops) != 1 || meta.Loops[0].CurrentContext != tt.want || meta.Loops[0].ContextSeq != tt.wantSeq || meta.Loops[0].ContextValueSeq != tt.wantValueSeq {
				t.Fatalf("loops = %#v", meta.Loops)
			}
		})
	}
}

func TestCatalogContextOrderingIsPerLoop(t *testing.T) {
	t.Parallel()
	sessionID := uuid.UUID{7}
	stepLoopID := uuid.UUID{8}
	measuredLoopID := uuid.UUID{9}
	measurement := catalogContextMeasurement(1)
	tests := []struct {
		name string
	}{
		{name: "unrelated newer step does not reject measurement"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			step := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: stepLoopID}}, Messages: validStepMessages()}
			meta, _, err := applyEvent(SessionMeta{}, step, 10, func() time.Time { return time.Time{} })
			if err != nil {
				t.Fatal(err)
			}
			measured := event.ContextMeasured{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: measuredLoopID}}, Measurement: measurement}
			meta, _, err = applyEvent(meta, measured, 9, func() time.Time { return time.Time{} })
			if err != nil {
				t.Fatal(err)
			}
			index, found := loopUsageIndex(meta.Loops, measuredLoopID)
			if !found || meta.Loops[index].CurrentContext != measurement || meta.Loops[index].ContextSeq != 9 || meta.Loops[index].ContextValueSeq != 9 {
				t.Fatalf("measured loop = %#v, found=%v", meta.Loops, found)
			}
		})
	}
}

func TestCatalogContextLiveUpdatePreservesWatermark(t *testing.T) {
	t.Parallel()
	sessionID := uuid.UUID{7}
	loopID := uuid.UUID{8}
	header := event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}
	first := catalogContextMeasurement(1)
	second := catalogContextMeasurement(2)
	tests := []struct {
		name  string
		apply []struct {
			ev  event.Event
			seq uint64
		}
	}{
		{name: "delayed measurement stays behind live mutation", apply: []struct {
			ev  event.Event
			seq uint64
		}{
			{event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}}, 1},
			{event.ContextMeasured{Header: header, Measurement: first}, 2},
			{event.TurnStarted{Header: header}, 5},
			{event.ContextMeasured{Header: header, Measurement: second}, 4},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, err := Open(memstore.New())
			if err != nil {
				t.Fatal(err)
			}
			catalog := store.OpenCatalog()
			for _, item := range tt.apply {
				if err := catalog.UpdateOnEvent(context.Background(), item.ev, item.seq); err != nil {
					t.Fatal(err)
				}
			}
			meta, found, err := catalog.ReadMeta(context.Background(), sessionID)
			if err != nil || !found {
				t.Fatalf("ReadMeta() = %#v, %v, %v", meta, found, err)
			}
			index, loopFound := loopUsageIndex(meta.Loops, loopID)
			if !loopFound || meta.Loops[index].ContextSeq != 5 || meta.Loops[index].ContextValueSeq != 0 || meta.Loops[index].CurrentContext != (event.ContextMeasurement{}) {
				t.Fatalf("live loop = %#v, found=%v", meta.Loops, loopFound)
			}
		})
	}
}

func TestCatalogContextValidationAndRoundTrip(t *testing.T) {
	t.Parallel()
	loopID := uuid.UUID{8}
	measurement := catalogContextMeasurement(1)
	valid := SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, CurrentContext: measurement, ContextSeq: 2, ContextValueSeq: 2}}}
	tests := []struct {
		name    string
		meta    SessionMeta
		wantErr bool
	}{
		{name: "valid round trip", meta: valid},
		{name: "empty legacy projection", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, CurrentContext: event.ContextMeasurement{}, ContextSeq: 0}}}},
		{name: "invalidated projection preserves watermark", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, ContextSeq: 2}}}},
		{name: "measurement missing sequence", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, CurrentContext: measurement}}}, wantErr: true},
		{name: "measurement missing value sequence", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, CurrentContext: measurement, ContextSeq: 2}}}, wantErr: true},
		{name: "empty projection with value sequence", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, ContextSeq: 2, ContextValueSeq: 2}}}, wantErr: true},
		{name: "value sequence exceeds watermark", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, CurrentContext: measurement, ContextSeq: 2, ContextValueSeq: 3}}}, wantErr: true},
		{name: "value sequence not newer than runtime", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, Runtime: event.ModelRuntime{Key: measurement.Model, Limits: inference.ContextLimits{WindowTokens: 100}}, RuntimeSeq: 2, RuntimeValueSeq: 2, CurrentContext: measurement, ContextSeq: 3, ContextValueSeq: 2}}}, wantErr: true},
		{name: "invalid nonzero measurement", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, CurrentContext: func() event.ContextMeasurement { value := measurement; value.InputLimit = 0; return value }(), ContextSeq: 2, ContextValueSeq: 2}}}, wantErr: true},
		{name: "measurement model mismatches runtime", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, Runtime: event.ModelRuntime{Key: inference.ModelKey{Provider: "other", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}}, CurrentContext: measurement, ContextSeq: 2, ContextValueSeq: 2}}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := encodeSessionMeta(tt.meta)
			if tt.wantErr {
				var target *CatalogMetaValidationError
				if !errors.As(err, &target) {
					t.Fatalf("error = %T %v", err, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeSessionMeta(encoded)
			if err != nil || !reflect.DeepEqual(decoded, tt.meta) {
				t.Fatalf("roundtrip = %#v, %v; want %#v", decoded, err, tt.meta)
			}
		})
	}
}

func TestCatalogContextBackwardCompatibilityAndRepair(t *testing.T) {
	t.Parallel()
	sessionID := uuid.UUID{7}
	loopID := uuid.UUID{8}
	header := event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID}}
	measurement := catalogContextMeasurement(1)
	tests := []struct {
		name    string
		act     func(*testing.T) SessionMeta
		wantSeq uint64
	}{
		{name: "legacy json omits context sequences", act: func(t *testing.T) SessionMeta {
			decoded, err := decodeSessionMeta([]byte(`{"loops":[{"loop_id":"` + loopID.String() + `"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			return decoded
		}},
		{name: "repair preserves invalidation watermark", wantSeq: 3, act: func(t *testing.T) SessionMeta {
			store, err := Open(memstore.New())
			if err != nil {
				t.Fatal(err)
			}
			events := []event.Event{
				event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}}},
				event.ContextMeasured{Header: header, Measurement: measurement},
				event.TurnStarted{Header: header},
			}
			meta, err := store.OpenCatalog(WithCatalogReplayer(&fakeOpener{events: events})).RepairCatalog(context.Background(), sessionID)
			if err != nil {
				t.Fatal(err)
			}
			return meta
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := tt.act(t)
			if len(meta.Loops) != 1 {
				t.Fatalf("loops = %#v", meta.Loops)
			}
			if meta.Loops[0].ContextSeq != tt.wantSeq || meta.Loops[0].ContextValueSeq != 0 || meta.Loops[0].CurrentContext != (event.ContextMeasurement{}) {
				t.Fatalf("repaired loop = %#v", meta.Loops[0])
			}
		})
	}
}

func FuzzContextCatalogJSON(f *testing.F) {
	seed, err := encodeSessionMeta(SessionMeta{Loops: []LoopUsageMeta{{LoopID: uuid.UUID{8}, CurrentContext: catalogContextMeasurement(1), ContextSeq: 2, ContextValueSeq: 2}}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, data []byte) {
		meta, err := decodeSessionMeta(data)
		if err != nil {
			return
		}
		if validateSessionMeta(meta) != nil {
			t.Fatal("decoder accepted invalid catalog")
		}
	})
}
