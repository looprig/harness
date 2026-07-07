package gate

type FieldKind string

const (
	FieldText        FieldKind = "text"
	FieldSelect      FieldKind = "select"
	FieldMultiSelect FieldKind = "multi_select"
)

type Prompt struct {
	Title    string       `json:"title,omitempty"`
	Body     string       `json:"body,omitempty"`
	Schema   PromptSchema `json:"schema,omitzero"`
	Controls []Control    `json:"controls,omitempty"`
}

type Control struct {
	Action string `json:"action,omitempty"`
	Label  string `json:"label,omitempty"`
}

type Field struct {
	Name     string      `json:"name,omitempty"`
	Label    string      `json:"label,omitempty"`
	Kind     FieldKind   `json:"kind,omitempty"`
	Required bool        `json:"required,omitzero"`
	Options  []Option    `json:"options,omitempty"`
	Default  interface{} `json:"default,omitempty"`
}

type Option struct {
	Value string `json:"value,omitempty"`
	Label string `json:"label,omitempty"`
}

type PromptSchema struct {
	Fields []Field `json:"fields,omitempty"`
}
