package gate

import "encoding/json"

// FieldKind identifies the expected shape of a prompt field value.
type FieldKind string

const (
	// FieldText accepts free-form text input.
	FieldText FieldKind = "text"
	// FieldSelect accepts one value from a fixed option list.
	FieldSelect FieldKind = "select"
	// FieldMultiSelect accepts multiple values from a fixed option list.
	FieldMultiSelect FieldKind = "multi_select"
)

// Prompt is the user-facing content and schema for resolving a gate.
type Prompt struct {
	Title    string       `json:"title,omitempty"`
	Body     string       `json:"body,omitempty"`
	Schema   PromptSchema `json:"schema,omitzero"`
	Controls []Control    `json:"controls,omitempty"`
}

// Control describes an action the resolver may choose for a prompt.
type Control struct {
	Action string `json:"action,omitempty"`
	Label  string `json:"label,omitempty"`
}

// Field describes one structured input in a prompt schema.
type Field struct {
	Name     string          `json:"name,omitempty"`
	Label    string          `json:"label,omitempty"`
	Kind     FieldKind       `json:"kind,omitempty"`
	Required bool            `json:"required,omitzero"`
	Options  []Option        `json:"options,omitempty"`
	Default  json.RawMessage `json:"default,omitempty"`
}

// Option is a selectable value for select-style fields.
type Option struct {
	Value string `json:"value,omitempty"`
	Label string `json:"label,omitempty"`
}

// PromptSchema groups the structured fields requested by a prompt.
type PromptSchema struct {
	Fields []Field `json:"fields,omitempty"`
}
