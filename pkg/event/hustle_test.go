package event

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

func hustleUUID(seed byte) uuid.UUID {
	var id uuid.UUID
	for index := range id {
		id[index] = seed
	}
	return id
}

func validHustleDescriptor(t *testing.T) hustle.DefinitionDescriptor {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName("conversation.compact"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1, OutputBytes: 1}),
		hustle.WithCurrentLoopModel(),
		hustle.WithSystemPrompt("raw-secret-prompt", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return definition.Descriptor()
}

func validHustleRun(t *testing.T, runtime ModelRuntime) HustleRunDescriptor {
	t.Helper()
	return HustleRunDescriptor{
		Definition: validHustleDescriptor(t),
		RunID:      hustle.RunID(hustleUUID(0x31)),
		Runtime:    runtime,
	}
}

func validHustleHeader(visibility EventVisibility) Header {
	return Header{
		Coordinates:     identity.Coordinates{SessionID: hustleUUID(0x11)},
		EventID:         hustleUUID(0x12),
		EventVisibility: visibility,
	}
}

func validHustleRuntime() ModelRuntime {
	return ModelRuntime{Key: inference.ModelKey{Provider: "test", Model: "model"}}
}

func TestEventVisibilityWireAndFilter(t *testing.T) {
	t.Parallel()
	legacy := SessionStarted{Header: validHustleHeader(Public)}
	legacyWire, err := MarshalEvent(legacy)
	if err != nil {
		t.Fatalf("MarshalEvent(legacy) error = %v", err)
	}
	tests := []struct {
		name            string
		wire            []byte
		wantVisibility  EventVisibility
		wantErr         bool
		wantFixedPoint  bool
		wantDeliverable bool
	}{
		{name: "legacy zero omitted and fixed point", wire: legacyWire, wantVisibility: Public, wantFixedPoint: true, wantDeliverable: true},
		{name: "unknown visibility rejected", wire: bytes.Replace(legacyWire, []byte(`"v":1`), []byte(`"v":1,"visibility":99`), 1), wantErr: true},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if testCase.wantFixedPoint && bytes.Contains(testCase.wire, []byte("visibility")) {
				t.Fatalf("legacy wire contains additive zero visibility: %s", testCase.wire)
			}
			ev, decodeErr := UnmarshalEvent(testCase.wire)
			if (decodeErr != nil) != testCase.wantErr {
				t.Fatalf("UnmarshalEvent() error = %v, wantErr %v", decodeErr, testCase.wantErr)
			}
			if testCase.wantErr {
				var invalid *InvalidEventError
				if !errors.As(decodeErr, &invalid) || invalid.Field != FieldVisibility {
					t.Fatalf("error = %T %v, want visibility InvalidEventError", decodeErr, decodeErr)
				}
				return
			}
			if ev.Visibility() != testCase.wantVisibility {
				t.Fatalf("Visibility() = %d, want %d", ev.Visibility(), testCase.wantVisibility)
			}
			if got := ShouldDeliver(EventFilter{}, ev); got != testCase.wantDeliverable {
				t.Fatalf("ShouldDeliver() = %v, want %v", got, testCase.wantDeliverable)
			}
			if testCase.wantFixedPoint {
				remarshaled, marshalErr := MarshalEvent(ev)
				if marshalErr != nil || !bytes.Equal(remarshaled, testCase.wire) {
					t.Fatalf("fixed point = (%s,%v), want %s", remarshaled, marshalErr, testCase.wire)
				}
			}
		})
	}
}

func TestHustleLifecycleRoundTripAndPrivacy(t *testing.T) {
	t.Parallel()
	usage := &content.Usage{InputTokens: 4, OutputTokens: 3, ReasoningTokens: 1}
	runtime := validHustleRuntime()
	tests := []struct {
		name string
		ev   Event
	}{
		{name: "started", ev: HustleStarted{Header: validHustleHeader(Internal), Run: validHustleRun(t, ModelRuntime{})}},
		{name: "completed", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: validHustleRun(t, runtime), Duration: time.Nanosecond, Usage: usage}},
		{name: "failed before resolution", ev: HustleFailed{Header: validHustleHeader(Internal), Run: validHustleRun(t, ModelRuntime{}), Duration: 0, Stage: hustle.StageModelResolution, ReasonCode: hustle.ReasonModelResolution}},
		{name: "failed after inference", ev: HustleFailed{Header: validHustleHeader(Internal), Run: validHustleRun(t, runtime), Duration: time.Second, Stage: hustle.StageOutput, ReasonCode: hustle.ReasonInvalidOutput, Usage: usage}},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			wire, err := MarshalEvent(testCase.ev)
			if err != nil {
				t.Fatalf("MarshalEvent() error = %v", err)
			}
			for _, forbidden := range []string{"raw-secret-prompt", "raw-input", "raw-output", "provider-error", "credential"} {
				if strings.Contains(string(wire), forbidden) {
					t.Fatalf("wire leaked %q: %s", forbidden, wire)
				}
			}
			decoded, err := UnmarshalEvent(wire)
			if err != nil {
				t.Fatalf("UnmarshalEvent() error = %v\nwire=%s", err, wire)
			}
			if !reflect.DeepEqual(decoded, testCase.ev) {
				t.Fatalf("round trip = %#v, want %#v", decoded, testCase.ev)
			}
			if decoded.Visibility() != Internal || ShouldDeliver(EventFilter{Enduring: LoopScope{All: true}}, decoded) {
				t.Fatalf("internal lifecycle visibility/delivery = %d/%v", decoded.Visibility(), ShouldDeliver(EventFilter{Enduring: LoopScope{All: true}}, decoded))
			}
		})
	}
	usage.InputTokens = 99
}

func TestHustleLifecycleValidation(t *testing.T) {
	t.Parallel()
	validRuntime := validHustleRuntime()
	validRun := validHustleRun(t, validRuntime)
	zeroRuntimeRun := validHustleRun(t, ModelRuntime{})
	invalidUsage := &content.Usage{OutputTokens: 1, ReasoningTokens: 2}
	tests := []struct {
		name    string
		ev      Event
		wantErr bool
	}{
		{name: "started minimum", ev: HustleStarted{Header: validHustleHeader(Internal), Run: zeroRuntimeRun}},
		{name: "started resolved runtime invalid", ev: HustleStarted{Header: validHustleHeader(Internal), Run: validRun}, wantErr: true},
		{name: "completed zero duration", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: validRun}},
		{name: "completed negative duration", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: validRun, Duration: -1}, wantErr: true},
		{name: "completed missing runtime", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: zeroRuntimeRun}, wantErr: true},
		{name: "completed invalid usage", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: validRun, Usage: invalidUsage}, wantErr: true},
		{name: "failed queue without runtime", ev: HustleFailed{Header: validHustleHeader(Internal), Run: zeroRuntimeRun, Stage: hustle.StageQueue, ReasonCode: hustle.ReasonCanceled}},
		{name: "failed resolution without runtime", ev: HustleFailed{Header: validHustleHeader(Internal), Run: zeroRuntimeRun, Stage: hustle.StageModelResolution, ReasonCode: hustle.ReasonModelResolution}},
		{name: "failed inference missing runtime", ev: HustleFailed{Header: validHustleHeader(Internal), Run: zeroRuntimeRun, Stage: hustle.StageInference, ReasonCode: hustle.ReasonInference}, wantErr: true},
		{name: "failed pre-resolution usage invalid", ev: HustleFailed{Header: validHustleHeader(Internal), Run: zeroRuntimeRun, Stage: hustle.StageQueue, ReasonCode: hustle.ReasonCanceled, Usage: &content.Usage{}}, wantErr: true},
		{name: "failed unknown stage", ev: HustleFailed{Header: validHustleHeader(Internal), Run: validRun, Stage: hustle.StageUnknown, ReasonCode: hustle.ReasonInference}, wantErr: true},
		{name: "failed unknown reason", ev: HustleFailed{Header: validHustleHeader(Internal), Run: validRun, Stage: hustle.StageInference, ReasonCode: hustle.ReasonUnknown}, wantErr: true},
		{name: "failed negative duration", ev: HustleFailed{Header: validHustleHeader(Internal), Run: validRun, Duration: -1, Stage: hustle.StageInference, ReasonCode: hustle.ReasonInference}, wantErr: true},
		{name: "zero run id", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: HustleRunDescriptor{Definition: validRun.Definition, Runtime: validRuntime}}, wantErr: true},
		{name: "zero definition", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: HustleRunDescriptor{RunID: validRun.RunID, Runtime: validRuntime}}, wantErr: true},
		{name: "public lifecycle invalid", ev: HustleStarted{Header: validHustleHeader(Public), Run: zeroRuntimeRun}, wantErr: true},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateEvent(testCase.ev)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("ValidateEvent() error = %v, wantErr %v", err, testCase.wantErr)
			}
			_, marshalErr := MarshalEvent(testCase.ev)
			if (marshalErr != nil) != testCase.wantErr {
				t.Fatalf("MarshalEvent() error = %v, wantErr %v", marshalErr, testCase.wantErr)
			}
		})
	}
}

func TestHustleLifecycleCodecRejectsDuplicateAliasesAndCopiesUsage(t *testing.T) {
	t.Parallel()
	usage := &content.Usage{InputTokens: 7, OutputTokens: 2}
	ev := HustleCompleted{Header: validHustleHeader(Internal), Run: validHustleRun(t, validHustleRuntime()), Usage: usage}
	wire, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}
	tests := []struct {
		name    string
		wire    []byte
		wantErr bool
	}{
		{name: "valid defensive decode", wire: wire},
		{name: "duplicate case alias", wire: bytes.Replace(wire, []byte(`"visibility":1`), []byte(`"visibility":1,"Visibility":1`), 1), wantErr: true},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			decoded, decodeErr := UnmarshalEvent(testCase.wire)
			if (decodeErr != nil) != testCase.wantErr {
				t.Fatalf("UnmarshalEvent() error = %v, wantErr %v, wire=%s", decodeErr, testCase.wantErr, testCase.wire)
			}
			if testCase.wantErr {
				return
			}
			usage.InputTokens = 99
			got := decoded.(HustleCompleted).Usage
			if got == nil || got.InputTokens != 7 {
				t.Fatalf("decoded usage = %#v, want independent input=7", got)
			}
		})
	}
}
