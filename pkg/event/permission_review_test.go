package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

func permissionReviewHeader() Header {
	return Header{
		Coordinates: identity.Coordinates{
			SessionID: uuid.UUID{0x11},
			LoopID:    uuid.UUID{0x12},
			TurnID:    uuid.UUID{0x13},
			StepID:    uuid.UUID{0x14},
		},
		EventID:         uuid.UUID{0x15},
		EventVisibility: Internal,
	}
}

func validPermissionReviewStarted() PermissionReviewStarted {
	return PermissionReviewStarted{
		Header:             permissionReviewHeader(),
		GateID:             gate.ID{0x21},
		ToolExecutionID:    uuid.UUID{0x22},
		Classifier:         hustle.Name("command-safety"),
		ClassifierRevision: "classifier-v1",
	}
}

func validPermissionReviewCompleted() PermissionReviewCompleted {
	return PermissionReviewCompleted{
		Header:             permissionReviewHeader(),
		GateID:             gate.ID{0x21},
		ToolExecutionID:    uuid.UUID{0x22},
		Classifier:         hustle.Name("command-safety"),
		ClassifierRevision: "classifier-v1",
		Status:             gate.ReviewStatusAllowed,
		Risk:               gate.ReviewRiskLow,
		Authorization:      gate.ReviewAuthorizationUnknown,
		Categories:         []gate.ReviewRiskCategory{gate.ReviewCategoryMutableNetwork},
		AutoApproved:       true,
	}
}

func TestPermissionReviewLifecycleContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ev   Event
	}{
		{name: "started", ev: validPermissionReviewStarted()},
		{name: "completed", ev: validPermissionReviewCompleted()},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.ev.Class() != Enduring {
				t.Errorf("Class() = %v, want Enduring", tt.ev.Class())
			}
			if tt.ev.Scope() != ScopeLoop {
				t.Errorf("Scope() = %v, want ScopeLoop", tt.ev.Scope())
			}
			if tt.ev.EndsTurn() {
				t.Error("EndsTurn() = true, want false")
			}
			if tt.ev.Visibility() != Internal {
				t.Errorf("Visibility() = %v, want Internal", tt.ev.Visibility())
			}
		})
	}
}

func TestPermissionReviewStartedValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*PermissionReviewStarted)
		field  FieldName
		rule   Rule
	}{
		{
			name:   "valid",
			mutate: func(*PermissionReviewStarted) {},
		},
		{
			name:   "public visibility",
			mutate: func(e *PermissionReviewStarted) { e.EventVisibility = Public },
			field:  FieldVisibility,
			rule:   RuleInvalid,
		},
		{
			name:   "missing session",
			mutate: func(e *PermissionReviewStarted) { e.SessionID = uuid.UUID{} },
			field:  FieldSessionID,
			rule:   RuleRequired,
		},
		{
			name:   "missing loop",
			mutate: func(e *PermissionReviewStarted) { e.LoopID = uuid.UUID{} },
			field:  FieldLoopID,
			rule:   RuleRequired,
		},
		{
			name:   "missing turn",
			mutate: func(e *PermissionReviewStarted) { e.TurnID = uuid.UUID{} },
			field:  FieldTurnID,
			rule:   RuleRequired,
		},
		{
			name:   "missing step",
			mutate: func(e *PermissionReviewStarted) { e.StepID = uuid.UUID{} },
			field:  FieldStepID,
			rule:   RuleRequired,
		},
		{
			name:   "missing gate",
			mutate: func(e *PermissionReviewStarted) { e.GateID = gate.ID{} },
			field:  FieldGateID,
			rule:   RuleRequired,
		},
		{
			name:   "missing tool execution",
			mutate: func(e *PermissionReviewStarted) { e.ToolExecutionID = uuid.UUID{} },
			field:  FieldToolExecutionID,
			rule:   RuleRequired,
		},
		{
			name:   "blank classifier",
			mutate: func(e *PermissionReviewStarted) { e.Classifier = " \t" },
			field:  FieldClassifier,
			rule:   RuleInvalid,
		},
		{
			name:   "reserved classifier",
			mutate: func(e *PermissionReviewStarted) { e.Classifier = "_looprig.private" },
			field:  FieldClassifier,
			rule:   RuleInvalid,
		},
		{
			name:   "blank revision",
			mutate: func(e *PermissionReviewStarted) { e.ClassifierRevision = " \n" },
			field:  FieldClassifierRevision,
			rule:   RuleInvalid,
		},
		{
			name:   "invalid utf8 revision",
			mutate: func(e *PermissionReviewStarted) { e.ClassifierRevision = string([]byte{0xff}) },
			field:  FieldClassifierRevision,
			rule:   RuleInvalid,
		},
		{
			name: "overlong revision",
			mutate: func(e *PermissionReviewStarted) {
				e.ClassifierRevision = strings.Repeat("r", gate.MaxPermissionClassifierRevisionBytes+1)
			},
			field: FieldClassifierRevision,
			rule:  RuleInvalid,
		},
		{
			name: "revision at limit",
			mutate: func(e *PermissionReviewStarted) {
				e.ClassifierRevision = strings.Repeat("r", gate.MaxPermissionClassifierRevisionBytes)
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := validPermissionReviewStarted()
			tt.mutate(&ev)
			assertPermissionReviewValidation(t, ev, tt.field, tt.rule)
		})
	}
}

func TestPermissionReviewCompletedStatusValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*PermissionReviewCompleted)
		field  FieldName
	}{
		{
			name:   "allowed",
			mutate: func(*PermissionReviewCompleted) {},
		},
		{
			name: "allowed without categories",
			mutate: func(e *PermissionReviewCompleted) {
				e.Categories = nil
			},
		},
		{
			name: "allowed with every category",
			mutate: func(e *PermissionReviewCompleted) {
				e.Categories = allPermissionReviewCategories()
			},
		},
		{
			name: "needs human",
			mutate: func(e *PermissionReviewCompleted) {
				e.Status = gate.ReviewStatusNeedsHuman
				e.AutoApproved = false
			},
		},
		{
			name: "not applicable",
			mutate: func(e *PermissionReviewCompleted) {
				clearPermissionReviewAssessment(e, gate.ReviewStatusNotApplicable)
			},
		},
		{
			name: "timed out",
			mutate: func(e *PermissionReviewCompleted) {
				clearPermissionReviewAssessment(e, gate.ReviewStatusTimedOut)
			},
		},
		{
			name: "failed",
			mutate: func(e *PermissionReviewCompleted) {
				clearPermissionReviewAssessment(e, gate.ReviewStatusFailed)
			},
		},
		{
			name: "cancelled",
			mutate: func(e *PermissionReviewCompleted) {
				clearPermissionReviewAssessment(e, gate.ReviewStatusCancelled)
			},
		},
		{
			name: "stale",
			mutate: func(e *PermissionReviewCompleted) {
				clearPermissionReviewAssessment(e, gate.ReviewStatusStale)
			},
		},
		{
			name:   "missing status",
			mutate: func(e *PermissionReviewCompleted) { e.Status = "" },
			field:  FieldStatus,
		},
		{
			name:   "unknown status",
			mutate: func(e *PermissionReviewCompleted) { e.Status = "future" },
			field:  FieldStatus,
		},
		{
			name:   "allowed missing risk",
			mutate: func(e *PermissionReviewCompleted) { e.Risk = "" },
			field:  FieldRisk,
		},
		{
			name:   "allowed unknown risk",
			mutate: func(e *PermissionReviewCompleted) { e.Risk = "future" },
			field:  FieldRisk,
		},
		{
			name:   "allowed missing authorization",
			mutate: func(e *PermissionReviewCompleted) { e.Authorization = "" },
			field:  FieldAuthorization,
		},
		{
			name:   "allowed unknown authorization",
			mutate: func(e *PermissionReviewCompleted) { e.Authorization = "future" },
			field:  FieldAuthorization,
		},
		{
			name: "allowed duplicate categories",
			mutate: func(e *PermissionReviewCompleted) {
				e.Categories = []gate.ReviewRiskCategory{
					gate.ReviewCategoryMutableNetwork,
					gate.ReviewCategoryMutableNetwork,
				}
			},
			field: FieldCategories,
		},
		{
			name: "allowed unknown category",
			mutate: func(e *PermissionReviewCompleted) {
				e.Categories = []gate.ReviewRiskCategory{"future"}
			},
			field: FieldCategories,
		},
		{
			name: "allowed too many categories",
			mutate: func(e *PermissionReviewCompleted) {
				e.Categories = make([]gate.ReviewRiskCategory, gate.MaxReviewCategories+1)
			},
			field: FieldCategories,
		},
		{
			name:   "allowed without auto approval",
			mutate: func(e *PermissionReviewCompleted) { e.AutoApproved = false },
			field:  FieldAutoApproved,
		},
		{
			name: "needs human with auto approval",
			mutate: func(e *PermissionReviewCompleted) {
				e.Status = gate.ReviewStatusNeedsHuman
				e.AutoApproved = true
			},
			field: FieldAutoApproved,
		},
		{
			name: "failed with risk",
			mutate: func(e *PermissionReviewCompleted) {
				clearPermissionReviewAssessment(e, gate.ReviewStatusFailed)
				e.Risk = gate.ReviewRiskLow
			},
			field: FieldRisk,
		},
		{
			name: "failed with authorization",
			mutate: func(e *PermissionReviewCompleted) {
				clearPermissionReviewAssessment(e, gate.ReviewStatusFailed)
				e.Authorization = gate.ReviewAuthorizationUnknown
			},
			field: FieldAuthorization,
		},
		{
			name: "failed with category",
			mutate: func(e *PermissionReviewCompleted) {
				clearPermissionReviewAssessment(e, gate.ReviewStatusFailed)
				e.Categories = []gate.ReviewRiskCategory{gate.ReviewCategoryInsufficientEvidence}
			},
			field: FieldCategories,
		},
		{
			name: "failed with auto approval",
			mutate: func(e *PermissionReviewCompleted) {
				clearPermissionReviewAssessment(e, gate.ReviewStatusFailed)
				e.AutoApproved = true
			},
			field: FieldAutoApproved,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := validPermissionReviewCompleted()
			tt.mutate(&ev)
			assertPermissionReviewValidation(t, ev, tt.field, RuleInvalid)
		})
	}
}

func TestPermissionReviewCompletedMetadataValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*PermissionReviewCompleted)
		field  FieldName
		rule   Rule
	}{
		{
			name:   "public visibility",
			mutate: func(e *PermissionReviewCompleted) { e.EventVisibility = Public },
			field:  FieldVisibility,
			rule:   RuleInvalid,
		},
		{
			name:   "missing step",
			mutate: func(e *PermissionReviewCompleted) { e.StepID = uuid.UUID{} },
			field:  FieldStepID,
			rule:   RuleRequired,
		},
		{
			name:   "missing gate",
			mutate: func(e *PermissionReviewCompleted) { e.GateID = gate.ID{} },
			field:  FieldGateID,
			rule:   RuleRequired,
		},
		{
			name:   "missing tool execution",
			mutate: func(e *PermissionReviewCompleted) { e.ToolExecutionID = uuid.UUID{} },
			field:  FieldToolExecutionID,
			rule:   RuleRequired,
		},
		{
			name:   "invalid classifier",
			mutate: func(e *PermissionReviewCompleted) { e.Classifier = "_looprig.private" },
			field:  FieldClassifier,
			rule:   RuleInvalid,
		},
		{
			name:   "invalid revision",
			mutate: func(e *PermissionReviewCompleted) { e.ClassifierRevision = " " },
			field:  FieldClassifierRevision,
			rule:   RuleInvalid,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := validPermissionReviewCompleted()
			tt.mutate(&ev)
			assertPermissionReviewValidation(t, ev, tt.field, tt.rule)
		})
	}
}

func TestPermissionReviewValidationErrorsDoNotEchoRejectedValues(t *testing.T) {
	t.Parallel()

	secretName := "_looprig.classifier-sensitive-name"
	secretRevision := "revision-sensitive-" + strings.Repeat("x", gate.MaxPermissionClassifierRevisionBytes)
	tests := []struct {
		name   string
		ev     Event
		secret string
	}{
		{
			name: "classifier",
			ev: func() Event {
				value := validPermissionReviewStarted()
				value.Classifier = hustle.Name(secretName)
				return value
			}(),
			secret: secretName,
		},
		{
			name: "revision",
			ev: func() Event {
				value := validPermissionReviewStarted()
				value.ClassifierRevision = secretRevision
				return value
			}(),
			secret: secretRevision,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateEvent(tt.ev)
			if err == nil {
				t.Fatal("ValidateEvent() error = nil, want invalid review metadata")
			}
			if strings.Contains(err.Error(), tt.secret) {
				t.Errorf("error leaks rejected value %q: %v", tt.secret, err)
			}
		})
	}
}

func TestPermissionReviewCodecRoundTripAndFixedPoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tag  string
		ev   Event
	}{
		{
			name: "started",
			tag:  "PermissionReviewStarted",
			ev:   validPermissionReviewStarted(),
		},
		{
			name: "completed",
			tag:  "PermissionReviewCompleted",
			ev:   validPermissionReviewCompleted(),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first, err := MarshalEvent(tt.ev)
			if err != nil {
				t.Fatalf("MarshalEvent() error = %v", err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(first, &wire); err != nil {
				t.Fatalf("json.Unmarshal(wire) error = %v", err)
			}
			assertReviewWireString(t, wire, "type", tt.tag)
			if got := string(wire["v"]); got != "1" {
				t.Errorf("v = %s, want 1", got)
			}
			for _, key := range []string{
				"gate_id",
				"tool_execution_id",
				"classifier",
				"classifier_revision",
			} {
				if _, ok := wire[key]; !ok {
					t.Errorf("wire missing %q: %s", key, first)
				}
			}
			if tt.tag == "PermissionReviewCompleted" {
				for _, key := range []string{
					"status",
					"risk",
					"authorization",
					"categories",
					"auto_approved",
				} {
					if _, ok := wire[key]; !ok {
						t.Errorf("completed wire missing %q: %s", key, first)
					}
				}
			}

			decoded, err := UnmarshalEvent(first)
			if err != nil {
				t.Fatalf("UnmarshalEvent() error = %v\nwire: %s", err, first)
			}
			if !reflect.DeepEqual(decoded, tt.ev) {
				t.Errorf("round trip mismatch:\n got: %#v\nwant: %#v", decoded, tt.ev)
			}
			if err := ValidateEvent(decoded); err != nil {
				t.Errorf("ValidateEvent(decoded) error = %v", err)
			}
			second, err := MarshalEvent(decoded)
			if err != nil {
				t.Fatalf("MarshalEvent(decoded) error = %v", err)
			}
			if !bytes.Equal(second, first) {
				t.Errorf("re-marshal not fixed point:\nfirst:  %s\nsecond: %s", first, second)
			}
		})
	}
}

func TestPermissionReviewCodecAllowsAdditiveUnknownFields(t *testing.T) {
	t.Parallel()

	original, err := MarshalEvent(validPermissionReviewCompleted())
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(original, &wire); err != nil {
		t.Fatalf("json.Unmarshal(wire) error = %v", err)
	}
	wire["future_audit_field"] = json.RawMessage(`{"nested":true}`)
	withUnknown, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal(wire) error = %v", err)
	}
	decoded, err := UnmarshalEvent(withUnknown)
	if err != nil {
		t.Fatalf("UnmarshalEvent(additive unknown field) error = %v", err)
	}
	if err := ValidateEvent(decoded); err != nil {
		t.Errorf("ValidateEvent(decoded) error = %v", err)
	}
	remarshal, err := MarshalEvent(decoded)
	if err != nil {
		t.Fatalf("MarshalEvent(decoded) error = %v", err)
	}
	if !bytes.Equal(remarshal, original) {
		t.Errorf("known projection changed after unknown field:\n got: %s\nwant: %s", remarshal, original)
	}
}

func TestPermissionReviewCodecPreservesPresentEmptyCategories(t *testing.T) {
	t.Parallel()

	original := validPermissionReviewCompleted()
	original.Categories = []gate.ReviewRiskCategory{}
	data, err := MarshalEvent(original)
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}
	decoded, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent() error = %v", err)
	}
	completed, ok := decoded.(PermissionReviewCompleted)
	if !ok {
		t.Fatalf("UnmarshalEvent() = %T, want PermissionReviewCompleted", decoded)
	}
	if completed.Categories == nil {
		t.Error("Categories = nil, want present empty slice preserved")
	}
	if !reflect.DeepEqual(completed, original) {
		t.Errorf("round trip mismatch:\n got: %#v\nwant: %#v", completed, original)
	}
}

func TestPermissionReviewPayloadHasNoSensitiveFields(t *testing.T) {
	t.Parallel()

	forbidden := []string{
		"command",
		"argument",
		"context",
		"evidence",
		"output",
		"prompt",
		"rationale",
		"reasoning",
		"credential",
		"secret",
		"rule",
		"candidate",
		"grant",
		"token",
		"response",
		"request",
		"content",
	}
	events := []Event{
		validPermissionReviewStarted(),
		validPermissionReviewCompleted(),
	}
	for _, ev := range events {
		ev := ev
		t.Run(reflect.TypeOf(ev).Name(), func(t *testing.T) {
			t.Parallel()
			assertReviewTypeHasNoSensitiveFields(t, reflect.TypeOf(ev), forbidden)

			data, err := MarshalEvent(ev)
			if err != nil {
				t.Fatalf("MarshalEvent() error = %v", err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatalf("json.Unmarshal(wire) error = %v", err)
			}
			for key := range wire {
				if key == "type" || key == "v" || reviewHeaderJSONKey(key) {
					continue
				}
				assertReviewIdentifierAllowed(t, "JSON key", key, forbidden)
			}

			sourceOnlySecrets := []string{
				"source-command-secret-9a7c",
				"source-evidence-secret-4b2d",
				"source-rationale-secret-6f1e",
			}
			for _, secret := range sourceOnlySecrets {
				if bytes.Contains(data, []byte(secret)) {
					t.Errorf("durable event contains source-only secret %q", secret)
				}
			}
		})
	}
}

func assertReviewWireString(t *testing.T, wire map[string]json.RawMessage, key, want string) {
	t.Helper()
	var got string
	if err := json.Unmarshal(wire[key], &got); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", key, err)
	}
	if got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

func assertReviewTypeHasNoSensitiveFields(t *testing.T, typ reflect.Type, forbidden []string) {
	t.Helper()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if field.Type == reflect.TypeOf(Header{}) ||
			field.PkgPath != "" {
			continue
		}
		assertReviewIdentifierAllowed(t, "Go field", field.Name, forbidden)
		assertReviewTypeHasNoSensitiveNestedFields(t, field.Type, forbidden)
	}
}

func assertReviewTypeHasNoSensitiveNestedFields(t *testing.T, typ reflect.Type, forbidden []string) {
	t.Helper()
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		typ = typ.Elem()
	}
	if typ.PkgPath() != "github.com/looprig/harness/pkg/event" || typ.Kind() != reflect.Struct {
		return
	}
	assertReviewTypeHasNoSensitiveFields(t, typ, forbidden)
}

func assertReviewIdentifierAllowed(t *testing.T, kind, identifier string, forbidden []string) {
	t.Helper()
	lower := strings.ToLower(identifier)
	for _, word := range forbidden {
		if strings.Contains(lower, word) {
			t.Errorf("%s %q contains forbidden sensitive-data term %q", kind, identifier, word)
		}
	}
}

func reviewHeaderJSONKey(key string) bool {
	switch key {
	case "session_id",
		"loop_id",
		"turn_id",
		"step_id",
		"agent_name",
		"event_id",
		"created_at",
		"cause",
		"visibility":
		return true
	default:
		return false
	}
}

func clearPermissionReviewAssessment(e *PermissionReviewCompleted, status gate.ReviewStatus) {
	e.Status = status
	e.Risk = ""
	e.Authorization = ""
	e.Categories = nil
	e.AutoApproved = false
}

func allPermissionReviewCategories() []gate.ReviewRiskCategory {
	return []gate.ReviewRiskCategory{
		gate.ReviewCategoryDataExfiltration,
		gate.ReviewCategoryCredentialAccess,
		gate.ReviewCategoryCredentialProbing,
		gate.ReviewCategoryDestructiveLocal,
		gate.ReviewCategoryDestructiveShared,
		gate.ReviewCategoryPersistentSecurityWeakening,
		gate.ReviewCategoryProductionMutation,
		gate.ReviewCategoryProtectedSourceControl,
		gate.ReviewCategoryUntrustedCodeExecution,
		gate.ReviewCategoryMutableNetwork,
		gate.ReviewCategoryPromptInjection,
		gate.ReviewCategoryAuthorizationConflict,
		gate.ReviewCategoryTargetAmbiguity,
		gate.ReviewCategoryInsufficientEvidence,
	}
}

func assertPermissionReviewValidation(t *testing.T, ev Event, field FieldName, rule Rule) {
	t.Helper()

	err := ValidateEvent(ev)
	if field == "" {
		if err != nil {
			t.Fatalf("ValidateEvent() error = %v, want nil", err)
		}
		return
	}
	var invalid *InvalidEventError
	if !errors.As(err, &invalid) {
		t.Fatalf("ValidateEvent() error = %T (%v), want *InvalidEventError", err, err)
	}
	if invalid.Field != field || invalid.Rule != rule {
		t.Errorf("InvalidEventError = {Field:%q Rule:%q}, want {Field:%q Rule:%q}", invalid.Field, invalid.Rule, field, rule)
	}
}
