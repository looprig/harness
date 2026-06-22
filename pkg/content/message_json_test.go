package content_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
)

// TestToolResultMessageJSONPreservesToolUseID is the key regression:
// ToolResultMessage must NOT inherit Message's promoted MarshalJSON/UnmarshalJSON,
// which would silently drop ToolUseID. Marshal then unmarshal a ToolResultMessage
// and assert ToolUseID survives along with the blocks. It also pins the wire form:
// the rename is Go-type-only, so the JSON must still carry "role":"tool" and the
// "tool_use_id" field byte-for-byte.
func TestToolResultMessageJSONPreservesToolUseID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		in           content.ToolResultMessage
		wantContains []string
	}{
		{
			name: "tool message with id and content block",
			in: content.ToolResultMessage{
				Message: content.Message{
					Role:   content.RoleTool,
					Blocks: []content.Block{&content.TextBlock{Text: "result"}},
				},
				ToolUseID: "tu_42",
			},
			wantContains: []string{`"role":"tool"`, `"tool_use_id":"tu_42"`},
		},
		{
			name: "tool message with id and no blocks",
			in: content.ToolResultMessage{
				Message:   content.Message{Role: content.RoleTool},
				ToolUseID: "tu_7",
			},
			wantContains: []string{`"role":"tool"`, `"tool_use_id":"tu_7"`},
		},
		{
			name: "tool message with nested tool_result block",
			in: content.ToolResultMessage{
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
			wantContains: []string{`"role":"tool"`, `"tool_use_id":"tu_outer"`},
		},
		{
			name: "error result preserves IsError true on the wire",
			in: content.ToolResultMessage{
				Message: content.Message{
					Role:   content.RoleTool,
					Blocks: []content.Block{&content.TextBlock{Text: "tool error: boom"}},
				},
				ToolUseID: "tu_err",
				IsError:   true,
			},
			// omitempty omits false; true is emitted as "is_error":true.
			wantContains: []string{`"role":"tool"`, `"tool_use_id":"tu_err"`, `"is_error":true`},
		},
		{
			name: "success result omits IsError on the wire (false)",
			in: content.ToolResultMessage{
				Message: content.Message{
					Role:   content.RoleTool,
					Blocks: []content.Block{&content.TextBlock{Text: "ok"}},
				},
				ToolUseID: "tu_ok",
				IsError:   false,
			},
			wantContains: []string{`"role":"tool"`, `"tool_use_id":"tu_ok"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("json.Marshal(ToolResultMessage) error = %v", err)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(string(data), want) {
					t.Errorf("marshalled JSON = %s, want it to contain %s", data, want)
				}
			}
			var got content.ToolResultMessage
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("json.Unmarshal(ToolResultMessage) error = %v", err)
			}
			if got.ToolUseID != tt.in.ToolUseID {
				t.Errorf("ToolUseID = %q, want %q (dropped by promoted method?)", got.ToolUseID, tt.in.ToolUseID)
			}
			if got.IsError != tt.in.IsError {
				t.Errorf("IsError = %v, want %v (dropped by codec?)", got.IsError, tt.in.IsError)
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
