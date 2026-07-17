package gate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// auditFormSchema is a schema with one field of every answerable kind, so a test
// can assert the redaction rule discriminates between them rather than applying
// uniformly.
func auditFormSchema() PromptSchema {
	return PromptSchema{Fields: []Field{
		{Name: "note", Kind: FieldText},
		{Name: "env", Kind: FieldSelect, Options: []Option{{Value: "prod"}, {Value: "dev"}}},
		{Name: "sure", Kind: FieldConfirm},
	}}
}

// TestNewFormAuditRedactsFreeTextButKeepsClosedSetChoices is the central
// security assertion for FormAudit: the durable audit records WHICH fields were
// answered and WHAT was chosen from a closed set, and never the text a human
// typed.
func TestNewFormAuditRedactsFreeTextButKeepsClosedSetChoices(t *testing.T) {
	t.Parallel()

	const secret = "CANARY-my-passphrase-hunter2"
	answers := map[string]string{"note": secret, "env": "prod", "sure": "true"}

	audit := NewFormAudit(auditFormSchema(), answers)

	// Every answered field is named...
	wantFields := []string{"env", "note", "sure"}
	if !reflect.DeepEqual(audit.AnsweredFields, wantFields) {
		t.Errorf("AnsweredFields = %v, want %v (sorted, all answered fields)", audit.AnsweredFields, wantFields)
	}
	// ...but only the closed-set fields carry a value.
	wantChoices := map[string]string{"env": "prod", "sure": "true"}
	if !reflect.DeepEqual(audit.Choices, wantChoices) {
		t.Errorf("Choices = %v, want %v (select and confirm only)", audit.Choices, wantChoices)
	}
	if _, ok := audit.Choices["note"]; ok {
		t.Error("Choices carries the free-text field: a FieldText answer must never be recorded")
	}

	// The decisive check: the typed text must not survive into the record in ANY
	// form, including truncated or nested somewhere unexpected.
	encoded, err := MarshalResponseAudit(audit)
	if err != nil {
		t.Fatalf("MarshalResponseAudit() error = %v", err)
	}
	if strings.Contains(string(encoded), "CANARY") {
		t.Fatalf("the free-text answer reached the durable audit record: %s", encoded)
	}
}

// TestNewFormAuditOmitsUnansweredFields proves an optional field left blank is
// not reported as answered.
func TestNewFormAuditOmitsUnansweredFields(t *testing.T) {
	t.Parallel()

	audit := NewFormAudit(auditFormSchema(), map[string]string{"env": "dev"})

	if !reflect.DeepEqual(audit.AnsweredFields, []string{"env"}) {
		t.Errorf("AnsweredFields = %v, want only the answered field", audit.AnsweredFields)
	}
	if len(audit.Choices) != 1 || audit.Choices["env"] != "dev" {
		t.Errorf("Choices = %v, want only env=dev", audit.Choices)
	}
}

// TestNewFormAuditIgnoresValuesWithNoSchemaField proves the walk is driven by the
// schema, not the answers map, so a name that never passed schema validation
// cannot reach a durable record.
func TestNewFormAuditIgnoresValuesWithNoSchemaField(t *testing.T) {
	t.Parallel()

	audit := NewFormAudit(auditFormSchema(), map[string]string{
		"env":      "dev",
		"smuggled": "CANARY-not-in-schema",
	})

	for _, name := range audit.AnsweredFields {
		if name == "smuggled" {
			t.Fatal("a field absent from the schema reached AnsweredFields")
		}
	}
	if _, ok := audit.Choices["smuggled"]; ok {
		t.Fatal("a field absent from the schema reached Choices")
	}
}

// TestFormAuditRoundTripsThroughTheSealedCodec proves the new union member is
// registered in every codec arm: tag, marshal, and unmarshal.
func TestFormAuditRoundTripsThroughTheSealedCodec(t *testing.T) {
	t.Parallel()

	original := FormAudit{
		AnsweredFields: []string{"env", "sure"},
		Choices:        map[string]string{"env": "prod", "sure": "false"},
	}

	data, err := MarshalResponseAudit(original)
	if err != nil {
		t.Fatalf("MarshalResponseAudit() error = %v", err)
	}

	// The wrapper must name the form kind, so a decoder can discriminate.
	var wrapper struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("json.Unmarshal wrapper: %v", err)
	}
	if wrapper.Kind != "form" {
		t.Errorf("audit wrapper kind = %q, want %q", wrapper.Kind, "form")
	}

	got, err := UnmarshalResponseAudit(data)
	if err != nil {
		t.Fatalf("UnmarshalResponseAudit() error = %v", err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Errorf("round trip = %#v, want %#v", got, original)
	}
}

// TestFormAuditPointerRoundTripsAndNilFailsClosed covers the pointer arms the
// other union members carry, including the nil-pointer guard.
func TestFormAuditPointerRoundTripsAndNilFailsClosed(t *testing.T) {
	t.Parallel()

	data, err := MarshalResponseAudit(&FormAudit{AnsweredFields: []string{"env"}})
	if err != nil {
		t.Fatalf("MarshalResponseAudit(*FormAudit) error = %v", err)
	}
	got, err := UnmarshalResponseAudit(data)
	if err != nil {
		t.Fatalf("UnmarshalResponseAudit() error = %v", err)
	}
	if !reflect.DeepEqual(got, FormAudit{AnsweredFields: []string{"env"}}) {
		t.Errorf("pointer round trip = %#v", got)
	}

	var nilAudit *FormAudit
	if _, err := MarshalResponseAudit(nilAudit); err == nil {
		t.Fatal("MarshalResponseAudit((*FormAudit)(nil)) succeeded, want a nil-audit error")
	}
}

// TestFormAuditMalformedDataFailsClosed proves the form arm decodes strictly:
// an unknown key is rejected rather than silently dropped, which is what keeps a
// widened record from being accepted by an older reader.
func TestFormAuditMalformedDataFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "unknown field", data: `{"kind":"form","data":{"answered_fields":["a"],"values":{"a":"b"}}}`},
		{name: "wrong type", data: `{"kind":"form","data":{"answered_fields":"not-a-list"}}`},
		{name: "null data", data: `{"kind":"form","data":null}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := UnmarshalResponseAudit([]byte(tt.data)); err == nil {
				t.Fatalf("UnmarshalResponseAudit(%s) succeeded, want a typed decode error", tt.data)
			}
		})
	}
}
