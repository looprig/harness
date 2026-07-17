package gate

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func confirmSchema() PromptSchema {
	return PromptSchema{Fields: []Field{
		{Name: "confirm", Label: "Proceed?", Kind: FieldConfirm, Required: true},
	}}
}

func richFormSchema() PromptSchema {
	return PromptSchema{Fields: []Field{
		{Name: "account", Label: "Account", Kind: FieldText, Required: true},
		{Name: "note", Label: "Note", Kind: FieldText},
		{
			Name:     "region",
			Label:    "Region",
			Kind:     FieldSelect,
			Required: true,
			Options:  []Option{{Value: "eu", Label: "EU"}, {Value: "us", Label: "US"}},
		},
		{Name: "confirm", Label: "Proceed?", Kind: FieldConfirm, Required: true},
	}}
}

func TestFormPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload FormPayload
	}{
		{
			name:    "confirmation only",
			payload: FormPayload{Title: "Confirm", Body: "Proceed?", Schema: confirmSchema()},
		},
		{
			name:    "rich form",
			payload: FormPayload{Title: "Details", Body: "Fill in", Schema: richFormSchema()},
		},
		{
			name:    "title and body omitted",
			payload: FormPayload{Schema: confirmSchema()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := MarshalPayload(tt.payload)
			if err != nil {
				t.Fatalf("MarshalPayload() error = %v", err)
			}
			got, err := UnmarshalPayload(data)
			if err != nil {
				t.Fatalf("UnmarshalPayload() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.payload) {
				t.Fatalf("round-trip = %#v, want %#v", got, tt.payload)
			}
		})
	}
}

func TestFormPayloadUsesFormKindTag(t *testing.T) {
	t.Parallel()

	data, err := MarshalPayload(FormPayload{Schema: confirmSchema()})
	if err != nil {
		t.Fatalf("MarshalPayload() error = %v", err)
	}
	var wrapper struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if wrapper.Kind != string(payloadKindForm) {
		t.Fatalf("kind = %q, want %q", wrapper.Kind, payloadKindForm)
	}
}

func TestFormPayloadPointerFormMarshals(t *testing.T) {
	t.Parallel()

	payload := &FormPayload{Title: "Confirm", Schema: confirmSchema()}
	data, err := MarshalPayload(payload)
	if err != nil {
		t.Fatalf("MarshalPayload() error = %v", err)
	}
	got, err := UnmarshalPayload(data)
	if err != nil {
		t.Fatalf("UnmarshalPayload() error = %v", err)
	}
	if !reflect.DeepEqual(got, *payload) {
		t.Fatalf("round-trip = %#v, want %#v", got, *payload)
	}
}

func TestFormPayloadNilPointerFailsClosed(t *testing.T) {
	t.Parallel()

	var payload *FormPayload
	_, err := MarshalPayload(payload)
	var nilErr *NilPayloadError
	if !errors.As(err, &nilErr) {
		t.Fatalf("MarshalPayload() error = %v, want *NilPayloadError", err)
	}
}

// A bad schema must not reach the journal, and must not survive restore.
func TestFormPayloadInvalidSchemaFailsBothBoundaries(t *testing.T) {
	t.Parallel()

	bad := FormPayload{Schema: PromptSchema{Fields: []Field{
		{Name: "picks", Kind: FieldMultiSelect, Options: []Option{{Value: "a"}}},
	}}}

	_, err := MarshalPayload(bad)
	var encodeErr *PayloadEncodeError
	if !errors.As(err, &encodeErr) {
		t.Fatalf("MarshalPayload() error = %v, want *PayloadEncodeError", err)
	}
	var schemaErr *FormSchemaError
	if !errors.As(err, &schemaErr) || schemaErr.Kind != FormSchemaFieldKindUnsupported {
		t.Fatalf("MarshalPayload() cause = %v, want FormSchemaFieldKindUnsupported", err)
	}

	// Hand-rolled record: a journal written by an older or hostile writer must
	// still be rejected on the way back in.
	raw := []byte(`{"kind":"form","data":{"schema":{"fields":[{"name":"picks","kind":"multi_select"}]}}}`)
	_, err = UnmarshalPayload(raw)
	var decodeErr *PayloadDecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("UnmarshalPayload() error = %v, want *PayloadDecodeError", err)
	}
	if !errors.As(err, &schemaErr) || schemaErr.Kind != FormSchemaFieldKindUnsupported {
		t.Fatalf("UnmarshalPayload() cause = %v, want FormSchemaFieldKindUnsupported", err)
	}
}

func TestValidateFormSchema(t *testing.T) {
	t.Parallel()

	tooManyFields := make([]Field, 0, maxFormFields+1)
	for i := range maxFormFields + 1 {
		tooManyFields = append(tooManyFields, Field{
			Name: "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Kind: FieldText,
		})
	}

	tooManyOptions := make([]Option, 0, maxFormFieldOptions+1)
	for i := range maxFormFieldOptions + 1 {
		tooManyOptions = append(tooManyOptions, Option{Value: string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}

	tests := []struct {
		name     string
		schema   PromptSchema
		wantKind FormSchemaErrorKind
	}{
		{name: "confirmation only", schema: confirmSchema()},
		{name: "rich form", schema: richFormSchema()},
		{
			name:   "max fields is allowed",
			schema: PromptSchema{Fields: tooManyFields[:maxFormFields]},
		},
		{
			name:     "no fields",
			schema:   PromptSchema{},
			wantKind: FormSchemaEmpty,
		},
		{
			name:     "too many fields",
			schema:   PromptSchema{Fields: tooManyFields},
			wantKind: FormSchemaTooManyFields,
		},
		{
			name:     "empty field name",
			schema:   PromptSchema{Fields: []Field{{Kind: FieldText}}},
			wantKind: FormSchemaFieldNameEmpty,
		},
		{
			name: "over-long field name",
			schema: PromptSchema{Fields: []Field{
				{Name: strings.Repeat("n", maxFormFieldNameBytes+1), Kind: FieldText},
			}},
			wantKind: FormSchemaFieldNameTooLong,
		},
		{
			name: "max-length field name is allowed",
			schema: PromptSchema{Fields: []Field{
				{Name: strings.Repeat("n", maxFormFieldNameBytes), Kind: FieldText},
			}},
		},
		{
			name: "duplicate field name",
			schema: PromptSchema{Fields: []Field{
				{Name: "a", Kind: FieldText},
				{Name: "a", Kind: FieldText},
			}},
			wantKind: FormSchemaFieldNameDuplicate,
		},
		{
			name: "multi select is unsupported",
			schema: PromptSchema{Fields: []Field{
				{Name: "picks", Kind: FieldMultiSelect, Options: []Option{{Value: "a"}}},
			}},
			wantKind: FormSchemaFieldKindUnsupported,
		},
		{
			name:     "unknown field kind fails closed",
			schema:   PromptSchema{Fields: []Field{{Name: "x", Kind: FieldKind("wat")}}},
			wantKind: FormSchemaFieldKindUnsupported,
		},
		{
			name:     "empty field kind fails closed",
			schema:   PromptSchema{Fields: []Field{{Name: "x"}}},
			wantKind: FormSchemaFieldKindUnsupported,
		},
		{
			name:     "select with no options",
			schema:   PromptSchema{Fields: []Field{{Name: "r", Kind: FieldSelect}}},
			wantKind: FormSchemaFieldOptionsInvalid,
		},
		{
			name: "select with too many options",
			schema: PromptSchema{Fields: []Field{
				{Name: "r", Kind: FieldSelect, Options: tooManyOptions},
			}},
			wantKind: FormSchemaFieldOptionsInvalid,
		},
		{
			name: "select with empty option value",
			schema: PromptSchema{Fields: []Field{
				{Name: "r", Kind: FieldSelect, Options: []Option{{Label: "blank"}}},
			}},
			wantKind: FormSchemaFieldOptionsInvalid,
		},
		{
			name: "text field with options",
			schema: PromptSchema{Fields: []Field{
				{Name: "t", Kind: FieldText, Options: []Option{{Value: "a"}}},
			}},
			wantKind: FormSchemaFieldOptionsInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateFormSchema(tt.schema)
			if tt.wantKind == "" {
				if err != nil {
					t.Fatalf("ValidateFormSchema() error = %v, want nil", err)
				}
				return
			}
			var schemaErr *FormSchemaError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("ValidateFormSchema() error = %v, want *FormSchemaError", err)
			}
			if schemaErr.Kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", schemaErr.Kind, tt.wantKind)
			}
		})
	}
}

func TestParseFormAnswers(t *testing.T) {
	t.Parallel()

	values := func(pairs map[string]string) map[string]json.RawMessage {
		out := make(map[string]json.RawMessage, len(pairs))
		for k, v := range pairs {
			out[k] = json.RawMessage(v)
		}
		return out
	}

	tests := []struct {
		name     string
		schema   PromptSchema
		values   map[string]json.RawMessage
		want     map[string]string
		wantKind FormAnswerErrorKind
	}{
		{
			name:   "happy path",
			schema: richFormSchema(),
			values: values(map[string]string{
				"account": `"acct-1"`,
				"note":    `"hello"`,
				"region":  `"eu"`,
				"confirm": `true`,
			}),
			want: map[string]string{
				"account": "acct-1",
				"note":    "hello",
				"region":  "eu",
				"confirm": "true",
			},
		},
		{
			name:   "confirm false normalizes",
			schema: confirmSchema(),
			values: values(map[string]string{"confirm": `false`}),
			want:   map[string]string{"confirm": "false"},
		},
		{
			name:   "optional field omitted",
			schema: richFormSchema(),
			values: values(map[string]string{
				"account": `"acct-1"`,
				"region":  `"us"`,
				"confirm": `true`,
			}),
			want: map[string]string{
				"account": "acct-1",
				"region":  "us",
				"confirm": "true",
			},
		},
		{
			name:   "optional field explicit null omitted",
			schema: richFormSchema(),
			values: values(map[string]string{
				"account": `"acct-1"`,
				"note":    `null`,
				"region":  `"us"`,
				"confirm": `true`,
			}),
			want: map[string]string{
				"account": "acct-1",
				"region":  "us",
				"confirm": "true",
			},
		},
		{
			name:   "max-length value is allowed",
			schema: PromptSchema{Fields: []Field{{Name: "note", Kind: FieldText}}},
			values: values(map[string]string{
				"note": `"` + strings.Repeat("x", maxFormValueBytes) + `"`,
			}),
			want: map[string]string{"note": strings.Repeat("x", maxFormValueBytes)},
		},
		{
			name:     "unknown field",
			schema:   confirmSchema(),
			values:   values(map[string]string{"confirm": `true`, "extra": `"x"`}),
			wantKind: FormAnswerUnknownField,
		},
		{
			name:     "missing required field",
			schema:   confirmSchema(),
			values:   values(map[string]string{}),
			wantKind: FormAnswerMissingRequired,
		},
		{
			name:     "required field explicit null",
			schema:   confirmSchema(),
			values:   values(map[string]string{"confirm": `null`}),
			wantKind: FormAnswerMissingRequired,
		},
		{
			name:     "required text field empty string",
			schema:   PromptSchema{Fields: []Field{{Name: "a", Kind: FieldText, Required: true}}},
			values:   values(map[string]string{"a": `""`}),
			wantKind: FormAnswerMissingRequired,
		},
		{
			name:     "confirm given a string",
			schema:   confirmSchema(),
			values:   values(map[string]string{"confirm": `"true"`}),
			wantKind: FormAnswerTypeInvalid,
		},
		{
			name:     "text given a number",
			schema:   PromptSchema{Fields: []Field{{Name: "a", Kind: FieldText}}},
			values:   values(map[string]string{"a": `12`}),
			wantKind: FormAnswerTypeInvalid,
		},
		{
			name:     "text given an array",
			schema:   PromptSchema{Fields: []Field{{Name: "a", Kind: FieldText}}},
			values:   values(map[string]string{"a": `["x"]`}),
			wantKind: FormAnswerTypeInvalid,
		},
		{
			name:   "value too long",
			schema: PromptSchema{Fields: []Field{{Name: "note", Kind: FieldText}}},
			values: values(map[string]string{
				"note": `"` + strings.Repeat("x", maxFormValueBytes+1) + `"`,
			}),
			wantKind: FormAnswerTooLong,
		},
		{
			name:     "select value outside options",
			schema:   richFormSchema(),
			values:   values(map[string]string{"account": `"a"`, "region": `"mars"`, "confirm": `true`}),
			wantKind: FormAnswerOptionNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseFormAnswers(tt.schema, tt.values)
			if tt.wantKind == "" {
				if err != nil {
					t.Fatalf("ParseFormAnswers() error = %v, want nil", err)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("ParseFormAnswers() = %#v, want %#v", got, tt.want)
				}
				return
			}
			var answerErr *FormAnswerError
			if !errors.As(err, &answerErr) {
				t.Fatalf("ParseFormAnswers() error = %v, want *FormAnswerError", err)
			}
			if answerErr.Kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", answerErr.Kind, tt.wantKind)
			}
			if got != nil {
				t.Fatalf("ParseFormAnswers() = %#v, want nil on error", got)
			}
		})
	}
}

// An unvalidated schema must never be used to accept answers.
func TestParseFormAnswersRejectsInvalidSchemaBeforeReadingValues(t *testing.T) {
	t.Parallel()

	schema := PromptSchema{Fields: []Field{{Name: "picks", Kind: FieldMultiSelect}}}
	got, err := ParseFormAnswers(schema, map[string]json.RawMessage{"picks": json.RawMessage(`"a"`)})
	var schemaErr *FormSchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("ParseFormAnswers() error = %v, want *FormSchemaError", err)
	}
	if got != nil {
		t.Fatalf("ParseFormAnswers() = %#v, want nil", got)
	}
}

func TestParseFormAnswersNilValues(t *testing.T) {
	t.Parallel()

	// A nil map is an empty submission: optional-only schemas accept it.
	got, err := ParseFormAnswers(PromptSchema{Fields: []Field{{Name: "note", Kind: FieldText}}}, nil)
	if err != nil {
		t.Fatalf("ParseFormAnswers() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ParseFormAnswers() = %#v, want empty", got)
	}

	// ...but a required field still fails closed.
	if _, err := ParseFormAnswers(confirmSchema(), nil); err == nil {
		t.Fatal("ParseFormAnswers() error = nil, want missing-required")
	}
}
