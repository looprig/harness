package gate

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

func TestPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	fixedID := uuid.MustParse("123e4567-e89b-12d3-a456-426614174030")
	tests := []struct {
		name    string
		payload Payload
	}{
		{
			name:    "permission",
			payload: PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}},
		},
		{
			name:    "ask user",
			payload: AskUserPayload{Question: "continue?", Choices: []string{"yes", "no"}},
		},
		{
			name:    "resume input",
			payload: ResumeInputPayload{InputID: fixedID, Preview: "..."},
		},
	}

	for _, tt := range tests {
		tt := tt
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

func TestPayloadUnknownKindFailsClosed(t *testing.T) {
	t.Parallel()

	_, err := UnmarshalPayload([]byte(`{"kind":"bogus","data":{}}`))
	var unknown *UnknownPayloadKindError
	if !errors.As(err, &unknown) {
		t.Fatalf("UnmarshalPayload() error = %v, want *UnknownPayloadKindError", err)
	}
	if unknown.Kind != "bogus" {
		t.Fatalf("UnknownPayloadKindError.Kind = %q, want bogus", unknown.Kind)
	}
}

func TestPayloadMalformedFailsClosedWithTypedError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "not json", data: `not json`},
		{name: "truncated", data: `{"kind":"ask_user",`},
		{name: "wrong wrapper shape", data: `[]`},
		{name: "wrong field type", data: `{"kind":"ask_user","data":{"question":42}}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := UnmarshalPayload([]byte(tt.data))
			var decode *PayloadDecodeError
			if !errors.As(err, &decode) {
				t.Fatalf("UnmarshalPayload(%q) error = %v, want *PayloadDecodeError", tt.data, err)
			}
		})
	}
}

func TestPermissionPayloadUsesToolCodec(t *testing.T) {
	t.Parallel()

	data, err := MarshalPayload(PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}})
	if err != nil {
		t.Fatalf("MarshalPayload() error = %v", err)
	}

	var wrapper struct {
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatalf("json.Unmarshal wrapper: %v", err)
	}
	if wrapper.Kind != "permission" {
		t.Fatalf("wrapper.Kind = %q, want permission", wrapper.Kind)
	}
	var nested struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(wrapper.Data, &nested); err != nil {
		t.Fatalf("json.Unmarshal nested permission data: %v", err)
	}
	if nested.Type != "bash" {
		t.Fatalf("nested permission type = %q, want bash", nested.Type)
	}

	got, err := UnmarshalPayload(data)
	if err != nil {
		t.Fatalf("UnmarshalPayload() error = %v", err)
	}
	permission, ok := got.(PermissionPayload)
	if !ok {
		t.Fatalf("payload type = %T, want PermissionPayload", got)
	}
	if _, ok := permission.Request.(tool.BashRequest); !ok {
		t.Fatalf("permission.Request type = %T, want tool.BashRequest", permission.Request)
	}
}

func TestResponseAuditRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		audit ResponseAudit
	}{
		{
			name:  "permission",
			audit: PermissionAudit{AcceptedGrantDescriptions: []string{"allow network egress for: git push", "allow write to /out"}},
		},
		{
			name:  "ask user",
			audit: AskUserAudit{AnswerPreview: "yes, continue"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := MarshalResponseAudit(tt.audit)
			if err != nil {
				t.Fatalf("MarshalResponseAudit() error = %v", err)
			}
			got, err := UnmarshalResponseAudit(data)
			if err != nil {
				t.Fatalf("UnmarshalResponseAudit() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.audit) {
				t.Fatalf("round-trip = %#v, want %#v", got, tt.audit)
			}
		})
	}
}

func TestResponseAuditDoesNotStoreGrantTokens(t *testing.T) {
	t.Parallel()

	const fakeToken = "grant-token-secret-123"
	data, err := MarshalResponseAudit(PermissionAudit{
		AcceptedGrantDescriptions: []string{"allow network egress for: git push"},
	})
	if err != nil {
		t.Fatalf("MarshalResponseAudit() error = %v", err)
	}
	if strings.Contains(string(data), fakeToken) {
		t.Fatalf("response audit JSON contains grant token %q: %s", fakeToken, data)
	}
	if strings.Contains(string(data), "token") {
		t.Fatalf("response audit JSON contains token-like field: %s", data)
	}
}

func TestResponseAuditMalformedFailsClosedWithTypedError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "not json", data: `not json`},
		{name: "truncated", data: `{"kind":"ask_user",`},
		{name: "wrong wrapper shape", data: `[]`},
		{name: "wrong field type", data: `{"kind":"ask_user","data":{"answer_preview":42}}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := UnmarshalResponseAudit([]byte(tt.data))
			var decode *ResponseAuditDecodeError
			if !errors.As(err, &decode) {
				t.Fatalf("UnmarshalResponseAudit(%q) error = %v, want *ResponseAuditDecodeError", tt.data, err)
			}
		})
	}
}
