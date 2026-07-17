package gate

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Form gate response actions. They are the Control.Action / ResponseRequest.Action
// values a form gate understands, and are answered through the ordinary
// ResponsePolicy machinery (a PolicyRespond template naming FormActionDecline is
// the fail-secure default an integration should configure for an unattended
// form).
const (
	// FormActionAccept submits answers; Values must satisfy ParseFormAnswers.
	FormActionAccept = "accept"
	// FormActionDecline records an explicit human refusal to answer.
	FormActionDecline = "decline"
	// FormActionCancel records that the request was withdrawn or timed out
	// rather than refused.
	FormActionCancel = "cancel"
)

// Form schema and answer bounds. A form is a human-facing prompt, not a data
// channel: these caps keep a hostile or buggy integration from journaling
// unbounded text or rendering an unanswerable prompt. They are enforced at both
// codec boundaries and when parsing a response.
const (
	// maxFormFields caps the number of fields in a form schema.
	maxFormFields = 32
	// maxFormFieldNameBytes caps a field name.
	maxFormFieldNameBytes = 128
	// maxFormFieldOptions caps the option list of a select field.
	maxFormFieldOptions = 64
	// maxFormValueBytes caps a single submitted answer.
	maxFormValueBytes = 4096
)

// FormSchemaErrorKind classifies a rejected form schema.
type FormSchemaErrorKind string

const (
	// FormSchemaEmpty reports a schema with no fields.
	FormSchemaEmpty FormSchemaErrorKind = "schema_empty"
	// FormSchemaTooManyFields reports more than maxFormFields fields.
	FormSchemaTooManyFields FormSchemaErrorKind = "schema_too_many_fields"
	// FormSchemaFieldNameEmpty reports a field with no name.
	FormSchemaFieldNameEmpty FormSchemaErrorKind = "field_name_empty"
	// FormSchemaFieldNameTooLong reports an over-long field name.
	FormSchemaFieldNameTooLong FormSchemaErrorKind = "field_name_too_long"
	// FormSchemaFieldNameDuplicate reports two fields sharing a name.
	FormSchemaFieldNameDuplicate FormSchemaErrorKind = "field_name_duplicate"
	// FormSchemaFieldKindUnsupported reports a field kind a form cannot answer.
	FormSchemaFieldKindUnsupported FormSchemaErrorKind = "field_kind_unsupported"
	// FormSchemaFieldOptionsInvalid reports a select field with no options, too
	// many options, or an empty option value.
	FormSchemaFieldOptionsInvalid FormSchemaErrorKind = "field_options_invalid"
)

// FormSchemaError reports a form schema that violates the form contract.
type FormSchemaError struct {
	Kind  FormSchemaErrorKind
	Field string
}

func (e *FormSchemaError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("gate: invalid form schema (%s)", string(e.Kind))
	}
	return fmt.Sprintf("gate: invalid form schema field %q (%s)", e.Field, string(e.Kind))
}

// FormAnswerErrorKind classifies a rejected form answer set.
type FormAnswerErrorKind string

const (
	// FormAnswerUnknownField reports a submitted value naming no schema field.
	FormAnswerUnknownField FormAnswerErrorKind = "unknown_field"
	// FormAnswerMissingRequired reports a required field with no value.
	FormAnswerMissingRequired FormAnswerErrorKind = "missing_required"
	// FormAnswerTypeInvalid reports a value whose JSON type does not match the
	// field kind (a string for text/select, a bool for confirm).
	FormAnswerTypeInvalid FormAnswerErrorKind = "type_invalid"
	// FormAnswerTooLong reports a value exceeding maxFormValueBytes.
	FormAnswerTooLong FormAnswerErrorKind = "too_long"
	// FormAnswerOptionNotAllowed reports a select value outside the field options.
	FormAnswerOptionNotAllowed FormAnswerErrorKind = "option_not_allowed"
)

// FormAnswerError reports a form response that does not satisfy its schema.
type FormAnswerError struct {
	Kind  FormAnswerErrorKind
	Field string
}

func (e *FormAnswerError) Error() string {
	return fmt.Sprintf("gate: invalid form answer for field %q (%s)", e.Field, string(e.Kind))
}

// formAnswerableKinds are the field kinds a form gate can request.
//
// FieldMultiSelect is deliberately EXCLUDED. A form answer is a
// map[string]string — one string per field — which cannot represent a
// multi-value selection without inventing an in-band separator that would be
// ambiguous against option values containing it. Rather than encode answers
// lossily or silently drop selections, a form schema that asks for one is
// rejected at every boundary. FieldMultiSelect remains valid in a Gate.Prompt
// for the existing kinds, which do not route through ParseFormAnswers.
func formAnswerableKind(kind FieldKind) bool {
	switch kind {
	case FieldText, FieldSelect, FieldConfirm:
		return true
	case FieldMultiSelect:
		return false
	default:
		return false
	}
}

// ValidateFormSchema reports whether a schema is a well-formed, bounded,
// answerable form request. It fails closed: an unknown field kind is rejected
// rather than treated as free text.
func ValidateFormSchema(schema PromptSchema) error {
	if len(schema.Fields) == 0 {
		return &FormSchemaError{Kind: FormSchemaEmpty}
	}
	if len(schema.Fields) > maxFormFields {
		return &FormSchemaError{Kind: FormSchemaTooManyFields}
	}
	seen := make(map[string]struct{}, len(schema.Fields))
	for _, field := range schema.Fields {
		if field.Name == "" {
			return &FormSchemaError{Kind: FormSchemaFieldNameEmpty}
		}
		if len(field.Name) > maxFormFieldNameBytes {
			return &FormSchemaError{Kind: FormSchemaFieldNameTooLong, Field: field.Name}
		}
		if _, dup := seen[field.Name]; dup {
			return &FormSchemaError{Kind: FormSchemaFieldNameDuplicate, Field: field.Name}
		}
		seen[field.Name] = struct{}{}
		if !formAnswerableKind(field.Kind) {
			return &FormSchemaError{Kind: FormSchemaFieldKindUnsupported, Field: field.Name}
		}
		if err := validateFormFieldOptions(field); err != nil {
			return err
		}
	}
	return nil
}

// validateFormFieldOptions enforces that a select field offers a usable,
// bounded option list and that a non-select field offers none.
func validateFormFieldOptions(field Field) error {
	if field.Kind != FieldSelect {
		if len(field.Options) > 0 {
			return &FormSchemaError{Kind: FormSchemaFieldOptionsInvalid, Field: field.Name}
		}
		return nil
	}
	if len(field.Options) == 0 || len(field.Options) > maxFormFieldOptions {
		return &FormSchemaError{Kind: FormSchemaFieldOptionsInvalid, Field: field.Name}
	}
	for _, option := range field.Options {
		if option.Value == "" {
			return &FormSchemaError{Kind: FormSchemaFieldOptionsInvalid, Field: field.Name}
		}
	}
	return nil
}

// ParseFormAnswers validates a FormActionAccept response's Values against the
// form schema and returns the answers keyed by field name.
//
// It is strict in both directions and fails closed: every submitted value must
// name a schema field, match that field's JSON type (a string for text and
// select, a bool for confirm), stay within maxFormValueBytes, and — for a select
// — name a declared option. Every required field must be present. A schema that
// does not satisfy ValidateFormSchema is rejected before any value is read, so a
// caller cannot smuggle an answer past an unvalidated field.
//
// A confirm answer is normalized to "true"/"false".
func ParseFormAnswers(schema PromptSchema, values map[string]json.RawMessage) (map[string]string, error) {
	if err := ValidateFormSchema(schema); err != nil {
		return nil, err
	}
	fields := make(map[string]Field, len(schema.Fields))
	for _, field := range schema.Fields {
		fields[field.Name] = field
	}
	for name := range values {
		if _, ok := fields[name]; !ok {
			return nil, &FormAnswerError{Kind: FormAnswerUnknownField, Field: name}
		}
	}
	answers := make(map[string]string, len(schema.Fields))
	for _, field := range schema.Fields {
		raw, ok := values[field.Name]
		if !ok || isExplicitJSONNull(raw) {
			if field.Required {
				return nil, &FormAnswerError{Kind: FormAnswerMissingRequired, Field: field.Name}
			}
			continue
		}
		answer, err := parseFormFieldAnswer(field, raw)
		if err != nil {
			return nil, err
		}
		answers[field.Name] = answer
	}
	return answers, nil
}

// parseFormFieldAnswer decodes and validates one field's raw JSON value.
func parseFormFieldAnswer(field Field, raw json.RawMessage) (string, error) {
	if field.Kind == FieldConfirm {
		var confirmed bool
		if err := json.Unmarshal(raw, &confirmed); err != nil {
			return "", &FormAnswerError{Kind: FormAnswerTypeInvalid, Field: field.Name}
		}
		if confirmed {
			return "true", nil
		}
		return "false", nil
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", &FormAnswerError{Kind: FormAnswerTypeInvalid, Field: field.Name}
	}
	if len(value) > maxFormValueBytes {
		return "", &FormAnswerError{Kind: FormAnswerTooLong, Field: field.Name}
	}
	if field.Required && value == "" {
		return "", &FormAnswerError{Kind: FormAnswerMissingRequired, Field: field.Name}
	}
	if field.Kind == FieldSelect && !formOptionAllowed(field, value) {
		return "", &FormAnswerError{Kind: FormAnswerOptionNotAllowed, Field: field.Name}
	}
	return value, nil
}

// NewFormAudit builds the durable, redacted audit for answers that already
// satisfied ParseFormAnswers against schema.
//
// It is the single place the redaction rule of FormAudit is applied, so a caller
// cannot journal a form answer without going through it. Every answered field
// contributes its NAME; only FieldSelect and FieldConfirm additionally
// contribute their VALUE, because only those values are drawn from a closed set
// the schema already made durable. A FieldText value is dropped here — see
// FormAudit for why that is a fail-closed choice rather than a lossy one.
//
// The schema drives the walk (not the answers map), so a field name that never
// passed schema validation cannot reach the record.
func NewFormAudit(schema PromptSchema, answers map[string]string) FormAudit {
	audit := FormAudit{}
	for _, field := range schema.Fields {
		value, ok := answers[field.Name]
		if !ok {
			continue
		}
		audit.AnsweredFields = append(audit.AnsweredFields, field.Name)
		switch field.Kind {
		case FieldSelect, FieldConfirm:
			if audit.Choices == nil {
				audit.Choices = make(map[string]string)
			}
			audit.Choices[field.Name] = value
		case FieldText, FieldMultiSelect:
			// Free text is never journaled. FieldMultiSelect cannot reach a
			// validated form answer at all (formAnswerableKind rejects it).
		}
	}
	sort.Strings(audit.AnsweredFields)
	return audit
}

func formOptionAllowed(field Field, value string) bool {
	for _, option := range field.Options {
		if option.Value == value {
			return true
		}
	}
	return false
}
