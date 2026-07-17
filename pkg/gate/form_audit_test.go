package gate

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// auditFormSchema is a schema with one field of every answerable kind, so a test
// can assert every kind's answer reaches the durable record.
func auditFormSchema() PromptSchema {
	return PromptSchema{Fields: []Field{
		{Name: "note", Kind: FieldText},
		{Name: "env", Kind: FieldSelect, Options: []Option{{Value: "prod"}, {Value: "dev"}}},
		{Name: "sure", Kind: FieldConfirm},
	}}
}

// TestNewFormAuditRecordsEveryAnswerIncludingFreeText pins the contract: a form
// audit is the durable record of what a human said, and free text is recorded
// verbatim rather than withheld — consistent with command.UserInput, which
// already journals user-authored text block for block.
func TestNewFormAuditRecordsEveryAnswerIncludingFreeText(t *testing.T) {
	t.Parallel()

	const typed = "the staging box was already broken when I got here"
	answers := map[string]string{"note": typed, "env": "prod", "sure": "true"}

	audit := NewFormAudit(auditFormSchema(), answers)

	want := map[string]string{"note": typed, "env": "prod", "sure": "true"}
	if !reflect.DeepEqual(audit.Values, want) {
		t.Errorf("Values = %v, want every answer including the free text %q", audit.Values, typed)
	}

	// The decisive check: the text must survive INTO the durable encoding, intact
	// and untruncated.
	encoded, err := MarshalResponseAudit(audit)
	if err != nil {
		t.Fatalf("MarshalResponseAudit() error = %v", err)
	}
	if !strings.Contains(string(encoded), typed) {
		t.Fatalf("the durable audit record dropped the user's answer: %s", encoded)
	}
}

// TestNewFormAuditOmitsUnansweredFields proves an optional field left blank is
// not invented as an empty answer.
func TestNewFormAuditOmitsUnansweredFields(t *testing.T) {
	t.Parallel()

	audit := NewFormAudit(auditFormSchema(), map[string]string{"env": "dev"})

	if !reflect.DeepEqual(audit.Values, map[string]string{"env": "dev"}) {
		t.Errorf("Values = %v, want only the answered field", audit.Values)
	}
}

// TestNewFormAuditIgnoresValuesWithNoSchemaField proves the walk is driven by the
// schema, not the answers map, so a name the schema never declared cannot reach a
// durable record.
func TestNewFormAuditIgnoresValuesWithNoSchemaField(t *testing.T) {
	t.Parallel()

	audit := NewFormAudit(auditFormSchema(), map[string]string{
		"env":      "dev",
		"smuggled": "never-declared",
	})

	if _, ok := audit.Values["smuggled"]; ok {
		t.Fatal("a field absent from the schema reached the durable audit")
	}
}

// TestFormAuditRoundTripsThroughTheSealedCodec proves the union member is
// registered in every codec arm — tag, marshal, unmarshal — and that a user's
// answer survives the round trip byte for byte.
func TestFormAuditRoundTripsThroughTheSealedCodec(t *testing.T) {
	t.Parallel()

	original := FormAudit{Values: map[string]string{
		"note": "a sentence with \"quotes\", a comma, and a ünicode ✓",
		"env":  "prod",
		"sure": "false",
	}}

	data, err := MarshalResponseAudit(original)
	if err != nil {
		t.Fatalf("MarshalResponseAudit() error = %v", err)
	}

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

	data, err := MarshalResponseAudit(&FormAudit{Values: map[string]string{"env": "dev"}})
	if err != nil {
		t.Fatalf("MarshalResponseAudit(*FormAudit) error = %v", err)
	}
	got, err := UnmarshalResponseAudit(data)
	if err != nil {
		t.Fatalf("UnmarshalResponseAudit() error = %v", err)
	}
	if !reflect.DeepEqual(got, FormAudit{Values: map[string]string{"env": "dev"}}) {
		t.Errorf("pointer round trip = %#v", got)
	}

	var nilAudit *FormAudit
	if _, err := MarshalResponseAudit(nilAudit); err == nil {
		t.Fatal("MarshalResponseAudit((*FormAudit)(nil)) succeeded, want a nil-audit error")
	}
}

// TestFormAuditMalformedDataFailsClosed proves the form arm decodes strictly: an
// unknown key is rejected rather than silently dropped.
func TestFormAuditMalformedDataFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "unknown field", data: `{"kind":"form","data":{"values":{"a":"b"},"choices":{"a":"b"}}}`},
		{name: "wrong type", data: `{"kind":"form","data":{"values":"not-a-map"}}`},
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

// TestValidateFormAuditBoundsAtTheBoundary pins the exact bounds. Unredacted is
// not unbounded: a third-party schema author must not be able to append an
// arbitrarily large record to the journal.
func TestValidateFormAuditBoundsAtTheBoundary(t *testing.T) {
	t.Parallel()

	valuesOfSize := func(n int) map[string]string {
		out := make(map[string]string, n)
		for i := 0; i < n; i++ {
			out[strings.Repeat("x", i+1)] = "v"
		}
		return out
	}

	tests := []struct {
		name    string
		audit   FormAudit
		wantErr FormAuditErrorKind
	}{
		{name: "empty is fine", audit: FormAudit{}},
		{name: "value at the limit", audit: FormAudit{Values: map[string]string{"a": strings.Repeat("x", maxFormValueBytes)}}},
		{
			name:    "value one over the limit",
			audit:   FormAudit{Values: map[string]string{"a": strings.Repeat("x", maxFormValueBytes+1)}},
			wantErr: FormAuditValueTooLong,
		},
		{name: "name at the limit", audit: FormAudit{Values: map[string]string{strings.Repeat("n", maxFormFieldNameBytes): "v"}}},
		{
			name:    "name one over the limit",
			audit:   FormAudit{Values: map[string]string{strings.Repeat("n", maxFormFieldNameBytes+1): "v"}},
			wantErr: FormAuditFieldNameTooLong,
		},
		{name: "count at the limit", audit: FormAudit{Values: valuesOfSize(maxFormFields)}},
		{
			name:    "count one over the limit",
			audit:   FormAudit{Values: valuesOfSize(maxFormFields + 1)},
			wantErr: FormAuditTooManyValues,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateFormAuditBounds(tt.audit)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateFormAuditBounds() error = %v, want nil", err)
				}
				return
			}
			var boundsErr *FormAuditError
			if !errors.As(err, &boundsErr) {
				t.Fatalf("ValidateFormAuditBounds() error = %v, want *FormAuditError", err)
			}
			if boundsErr.Kind != tt.wantErr {
				t.Errorf("error kind = %q, want %q", boundsErr.Kind, tt.wantErr)
			}
		})
	}
}

// TestFormAuditBoundsEnforcedAtBothCodecBoundaries proves an oversized record can
// neither be journaled nor read back. The decode half is what protects a reader
// from a record some other writer produced.
func TestFormAuditBoundsEnforcedAtBothCodecBoundaries(t *testing.T) {
	t.Parallel()

	oversized := FormAudit{Values: map[string]string{"note": strings.Repeat("x", maxFormValueBytes+1)}}
	if _, err := MarshalResponseAudit(oversized); err == nil {
		t.Fatal("MarshalResponseAudit() journaled an oversized form audit")
	}

	// A record written by something that did not enforce the bounds must be
	// rejected on the way back in, not trusted.
	forged, err := json.Marshal(responseAuditWrapper{
		Kind: responseAuditKindForm,
		Data: json.RawMessage(`{"values":{"note":"` + strings.Repeat("x", maxFormValueBytes+1) + `"}}`),
	})
	if err != nil {
		t.Fatalf("json.Marshal(forged): %v", err)
	}
	if _, err := UnmarshalResponseAudit(forged); err == nil {
		t.Fatal("UnmarshalResponseAudit() accepted an oversized form audit record")
	}
}

// TestParseFormAnswersBoundsMatchTheAuditBounds proves the two halves agree: an
// answer ParseFormAnswers accepts always survives ValidateFormAuditBounds, so the
// audit bound can never reject a legitimately answered form.
func TestParseFormAnswersBoundsMatchTheAuditBounds(t *testing.T) {
	t.Parallel()

	atLimit, err := json.Marshal(strings.Repeat("x", maxFormValueBytes))
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	schema := PromptSchema{Fields: []Field{{Name: "note", Kind: FieldText}}}

	answers, err := ParseFormAnswers(schema, map[string]json.RawMessage{"note": atLimit})
	if err != nil {
		t.Fatalf("ParseFormAnswers() rejected an answer at the limit: %v", err)
	}
	if err := ValidateFormAuditBounds(NewFormAudit(schema, answers)); err != nil {
		t.Fatalf("an answer ParseFormAnswers accepted was refused by the audit bounds: %v", err)
	}
}
