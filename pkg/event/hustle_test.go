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
	model "github.com/looprig/inference/model"
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
	return ModelRuntime{Key: model.ModelKey{Provider: "test", Model: "model"}}
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

func TestMarshalEventVisibilityBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		visibility EventVisibility
		wantErr    bool
	}{
		{name: "public zero valid", visibility: Public},
		{name: "internal valid", visibility: Internal},
		{name: "unknown invalid", visibility: EventVisibility(99), wantErr: true},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := MarshalEvent(SessionStarted{Header: Header{EventVisibility: testCase.visibility}})
			if (err != nil) != testCase.wantErr {
				t.Fatalf("MarshalEvent() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if testCase.wantErr {
				var invalid *InvalidEventError
				if !errors.As(err, &invalid) || invalid.Field != FieldVisibility || invalid.Rule != RuleInvalid {
					t.Fatalf("error = %T %v, want visibility InvalidEventError", err, err)
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
	currentWithNamedKey := validRun
	currentWithNamedKey.Definition.NamedModelKey = model.ModelKey{Provider: "forbidden", Model: "named"}
	currentWithNamedRevision := validRun
	currentWithNamedRevision.Definition.NamedModelPolicyRevision = "forbidden-named-policy"
	reservedDefinition := validRun
	reservedDefinition.Definition.Name = "_looprig.forged"
	overLimitDefinition := validRun
	overLimitDefinition.Definition.Limits.InputBytes = 16*1024*1024 + 1
	zeroPromptHashDefinition := validRun
	zeroPromptHashDefinition.Definition.PromptSHA256 = [32]byte{}
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
		{name: "failed queue rejects resolved runtime", ev: HustleFailed{Header: validHustleHeader(Internal), Run: validRun, Stage: hustle.StageQueue, ReasonCode: hustle.ReasonCanceled}, wantErr: true},
		{name: "failed resolution rejects resolved runtime", ev: HustleFailed{Header: validHustleHeader(Internal), Run: validRun, Stage: hustle.StageModelResolution, ReasonCode: hustle.ReasonModelResolution}, wantErr: true},
		{name: "failed inference missing runtime", ev: HustleFailed{Header: validHustleHeader(Internal), Run: zeroRuntimeRun, Stage: hustle.StageInference, ReasonCode: hustle.ReasonInference}, wantErr: true},
		{name: "failed pre-resolution usage invalid", ev: HustleFailed{Header: validHustleHeader(Internal), Run: zeroRuntimeRun, Stage: hustle.StageQueue, ReasonCode: hustle.ReasonCanceled, Usage: &content.Usage{}}, wantErr: true},
		{name: "failed unknown stage", ev: HustleFailed{Header: validHustleHeader(Internal), Run: validRun, Stage: hustle.StageUnknown, ReasonCode: hustle.ReasonInference}, wantErr: true},
		{name: "failed unknown reason", ev: HustleFailed{Header: validHustleHeader(Internal), Run: validRun, Stage: hustle.StageInference, ReasonCode: hustle.ReasonUnknown}, wantErr: true},
		{name: "failed negative duration", ev: HustleFailed{Header: validHustleHeader(Internal), Run: validRun, Duration: -1, Stage: hustle.StageInference, ReasonCode: hustle.ReasonInference}, wantErr: true},
		{name: "zero run id", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: HustleRunDescriptor{Definition: validRun.Definition, Runtime: validRuntime}}, wantErr: true},
		{name: "zero definition", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: HustleRunDescriptor{RunID: validRun.RunID, Runtime: validRuntime}}, wantErr: true},
		{name: "completed current loop rejects named key", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: currentWithNamedKey}, wantErr: true},
		{name: "failed current loop rejects named key", ev: HustleFailed{Header: validHustleHeader(Internal), Run: currentWithNamedKey, Stage: hustle.StageInference, ReasonCode: hustle.ReasonInference}, wantErr: true},
		{name: "completed current loop rejects named policy revision", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: currentWithNamedRevision}, wantErr: true},
		{name: "failed current loop rejects named policy revision", ev: HustleFailed{Header: validHustleHeader(Internal), Run: currentWithNamedRevision, Stage: hustle.StageInference, ReasonCode: hustle.ReasonInference}, wantErr: true},
		{name: "forged reserved definition", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: reservedDefinition}, wantErr: true},
		{name: "forged definition over payload limit", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: overLimitDefinition}, wantErr: true},
		{name: "forged definition zero prompt hash", ev: HustleCompleted{Header: validHustleHeader(Internal), Run: zeroPromptHashDefinition}, wantErr: true},
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

func TestHustleFailedStageReasonCompatibility(t *testing.T) {
	t.Parallel()
	allowed := map[hustle.Stage]map[hustle.ReasonCode]bool{
		hustle.StageQueue:           {hustle.ReasonRejected: true, hustle.ReasonCanceled: true, hustle.ReasonTimeout: true, hustle.ReasonInternal: true},
		hustle.StageModelResolution: {hustle.ReasonCanceled: true, hustle.ReasonTimeout: true, hustle.ReasonModelResolution: true, hustle.ReasonInternal: true},
		hustle.StageInference:       {hustle.ReasonCanceled: true, hustle.ReasonTimeout: true, hustle.ReasonInference: true, hustle.ReasonInternal: true},
		hustle.StageOutput:          {hustle.ReasonCanceled: true, hustle.ReasonTimeout: true, hustle.ReasonInvalidOutput: true, hustle.ReasonInternal: true},
		hustle.StageTerminal:        {hustle.ReasonTimeout: true, hustle.ReasonTerminal: true, hustle.ReasonInternal: true},
		hustle.StageFinalization:    {hustle.ReasonTimeout: true, hustle.ReasonFinalization: true, hustle.ReasonInternal: true},
	}
	tests := make([]struct {
		name   string
		stage  hustle.Stage
		reason hustle.ReasonCode
		want   bool
	}, 0, int(hustle.StageFinalization)*int(hustle.ReasonInternal))
	for stage := hustle.StageQueue; stage <= hustle.StageFinalization; stage++ {
		for reason := hustle.ReasonRejected; reason <= hustle.ReasonInternal; reason++ {
			tests = append(tests, struct {
				name   string
				stage  hustle.Stage
				reason hustle.ReasonCode
				want   bool
			}{name: "pair", stage: stage, reason: reason, want: allowed[stage][reason]})
		}
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			runtime := validHustleRuntime()
			if testCase.stage == hustle.StageQueue || testCase.stage == hustle.StageModelResolution {
				runtime = ModelRuntime{}
			}
			ev := HustleFailed{
				Header: validHustleHeader(Internal), Run: validHustleRun(t, runtime),
				Stage: testCase.stage, ReasonCode: testCase.reason,
			}
			err := ValidateEvent(ev)
			if (err == nil) != testCase.want {
				t.Fatalf("ValidateEvent(stage=%d reason=%d) error = %v, wantAllowed %v", testCase.stage, testCase.reason, err, testCase.want)
			}
			if !testCase.want {
				var invalid *InvalidEventError
				if !errors.As(err, &invalid) || invalid.Field != FieldReasonCode || invalid.Rule != RuleInvalid {
					t.Fatalf("error = %T %v, want reason-code InvalidEventError", err, err)
				}
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
