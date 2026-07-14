package sessionstore

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
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
		want    event.ContextMeasurement
		wantSeq uint64
	}{
		{name: "latest measurement", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: first}, 2}, {event.ContextMeasured{Header: header, Measurement: second}, 3}}, want: second, wantSeq: 3},
		{name: "older delayed measurement ignored", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: second}, 3}, {event.ContextMeasured{Header: header, Measurement: first}, 2}}, want: second, wantSeq: 3},
		{name: "runtime change invalidates", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: first}, 2}, {event.LoopInferenceChanged{Header: header, Runtime: runtime}, 3}}},
		{name: "context mutation invalidates", apply: []struct {
			ev  event.Event
			seq uint64
		}{{event.ContextMeasured{Header: header, Measurement: first}, 2}, {event.TurnStarted{Header: header}, 3}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := SessionMeta{}
			for _, item := range tt.apply {
				var err error
				meta, _, err = applyEvent(meta, item.ev, item.seq, func() time.Time { return time.Time{} })
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(meta.Loops) != 1 || meta.Loops[0].CurrentContext != tt.want || meta.Loops[0].ContextSeq != tt.wantSeq {
				t.Fatalf("loops = %#v", meta.Loops)
			}
		})
	}
}

func TestCatalogContextValidationAndRoundTrip(t *testing.T) {
	t.Parallel()
	loopID := uuid.UUID{8}
	measurement := catalogContextMeasurement(1)
	valid := SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, CurrentContext: measurement, ContextSeq: 2}}}
	tests := []struct {
		name    string
		meta    SessionMeta
		wantErr bool
	}{
		{name: "valid round trip", meta: valid},
		{name: "empty legacy projection", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, CurrentContext: event.ContextMeasurement{}, ContextSeq: 0}}}},
		{name: "empty projection with sequence", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, ContextSeq: 2}}}, wantErr: true},
		{name: "measurement missing sequence", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, CurrentContext: measurement}}}, wantErr: true},
		{name: "invalid nonzero measurement", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, CurrentContext: func() event.ContextMeasurement { value := measurement; value.InputLimit = 0; return value }(), ContextSeq: 2}}}, wantErr: true},
		{name: "measurement model mismatches runtime", meta: SessionMeta{Loops: []LoopUsageMeta{{LoopID: loopID, Runtime: event.ModelRuntime{Key: inference.ModelKey{Provider: "other", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}}, CurrentContext: measurement, ContextSeq: 2}}}, wantErr: true},
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

func FuzzContextCatalogJSON(f *testing.F) {
	seed, err := encodeSessionMeta(SessionMeta{Loops: []LoopUsageMeta{{LoopID: uuid.UUID{8}, CurrentContext: catalogContextMeasurement(1), ContextSeq: 2}}})
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
