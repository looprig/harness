package content_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
)

// TestToolMessageJSONPreservesToolUseID is the key regression: ToolMessage must
// NOT inherit Message's promoted MarshalJSON/UnmarshalJSON, which would silently
// drop ToolUseID. Marshal then unmarshal a ToolMessage and assert ToolUseID
// survives along with the blocks.
func TestToolMessageJSONPreservesToolUseID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   content.ToolMessage
	}{
		{
			name: "tool message with id and content block",
			in: content.ToolMessage{
				Message: content.Message{
					Role:   content.RoleTool,
					Blocks: []content.Block{&content.TextBlock{Text: "result"}},
				},
				ToolUseID: "tu_42",
			},
		},
		{
			name: "tool message with id and no blocks",
			in: content.ToolMessage{
				Message:   content.Message{Role: content.RoleTool},
				ToolUseID: "tu_7",
			},
		},
		{
			name: "tool message with nested tool_result block",
			in: content.ToolMessage{
				Message: content.Message{
					Role: content.RoleTool,
					Blocks: []content.Block{
						&content.ToolResultBlock{
							ToolUseID: "tu_inner",
							Content:   []content.Block{&content.TextBlock{Text: "nested"}},
						},
					},
				},
				ToolUseID: "tu_outer",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("json.Marshal(ToolMessage) error = %v", err)
			}
			var got content.ToolMessage
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal(ToolMessage) error = %v", err)
			}
			if got.ToolUseID != tt.in.ToolUseID {
				t.Errorf("ToolUseID = %q, want %q (dropped by promoted method?)", got.ToolUseID, tt.in.ToolUseID)
			}
			if !reflect.DeepEqual(got, tt.in) {
				t.Errorf("round trip = %#v, want %#v", got, tt.in)
			}
		})
	}
}

// TestMessageJSONRoundTrip verifies Message and the three message types that
// inherit its promoted codec (User/AI/System) round-trip through encoding/json.
func TestMessageJSONRoundTrip(t *testing.T) {
	t.Parallel()

	blocks := []content.Block{
		&content.TextBlock{Text: "hello"},
		&content.ThinkingBlock{Thinking: "hmm", Signature: "s"},
	}

	tests := []struct {
		name    string
		marshal func() ([]byte, error)
		decode  func([]byte) (any, error)
		want    any
	}{
		{
			name: "Message",
			marshal: func() ([]byte, error) {
				return json.Marshal(content.Message{Role: content.RoleUser, Blocks: blocks})
			},
			decode: func(b []byte) (any, error) {
				var m content.Message
				err := json.Unmarshal(b, &m)
				return m, err
			},
			want: content.Message{Role: content.RoleUser, Blocks: blocks},
		},
		{
			name: "UserMessage",
			marshal: func() ([]byte, error) {
				return json.Marshal(content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: blocks}})
			},
			decode: func(b []byte) (any, error) {
				var m content.UserMessage
				err := json.Unmarshal(b, &m)
				return m, err
			},
			want: content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: blocks}},
		},
		{
			name: "AIMessage",
			marshal: func() ([]byte, error) {
				return json.Marshal(content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}})
			},
			decode: func(b []byte) (any, error) {
				var m content.AIMessage
				err := json.Unmarshal(b, &m)
				return m, err
			},
			want: content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}},
		},
		{
			name: "SystemMessage",
			marshal: func() ([]byte, error) {
				return json.Marshal(content.SystemMessage{Message: content.Message{Role: content.RoleSystem, Blocks: blocks}})
			},
			decode: func(b []byte) (any, error) {
				var m content.SystemMessage
				err := json.Unmarshal(b, &m)
				return m, err
			},
			want: content.SystemMessage{Message: content.Message{Role: content.RoleSystem, Blocks: blocks}},
		},
		{
			name: "Message with nil blocks",
			marshal: func() ([]byte, error) {
				return json.Marshal(content.Message{Role: content.RoleSystem})
			},
			decode: func(b []byte) (any, error) {
				var m content.Message
				err := json.Unmarshal(b, &m)
				return m, err
			},
			want: content.Message{Role: content.RoleSystem},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := tt.marshal()
			if err != nil {
				t.Fatalf("marshal error = %v", err)
			}
			got, err := tt.decode(data)
			if err != nil {
				t.Fatalf("decode error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("round trip = %#v, want %#v", got, tt.want)
			}
		})
	}
}
