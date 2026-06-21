package tool

import (
	"errors"
	"reflect"
	"testing"
)

// sameRequest reports whether two PermissionRequests are observably equal through
// the sealed interface: same ToolName, Description, and AllowedScopes. The codec's
// full-fidelity contract is exactly that a reconstructed value answers these three
// methods identically to the original.
func sameRequest(t *testing.T, got, want PermissionRequest) {
	t.Helper()
	if got.ToolName() != want.ToolName() {
		t.Errorf("ToolName() = %q, want %q", got.ToolName(), want.ToolName())
	}
	if got.Description() != want.Description() {
		t.Errorf("Description() = %q, want %q", got.Description(), want.Description())
	}
	if !reflect.DeepEqual(got.AllowedScopes(), want.AllowedScopes()) {
		t.Errorf("AllowedScopes() = %v, want %v", got.AllowedScopes(), want.AllowedScopes())
	}
}

// TestPermissionRequestCodecRoundTrip proves every concrete request type survives
// Marshal→Unmarshal with full fidelity: the reconstructed value's
// ToolName()/Description()/AllowedScopes() equal the original's.
func TestPermissionRequestCodecRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  PermissionRequest
	}{
		{name: "file write", req: FileWriteRequest{Path: "/repo/main.go"}},
		{name: "file write empty", req: FileWriteRequest{}},
		{name: "bash", req: BashRequest{Command: "ls -la | grep go"}},
		{name: "bash empty", req: BashRequest{}},
		{name: "fetch", req: FetchRequest{Method: "POST", URL: "https://example.com/x?q=1"}},
		{name: "fetch empty", req: FetchRequest{}},
		{name: "web search", req: WebSearchRequest{Query: "golang sealed interface"}},
		{name: "web search empty", req: WebSearchRequest{}},
		{name: "unknown", req: UnknownRequest{Tool: "MysteryTool", Summary: "did a redacted thing"}},
		{name: "unknown empty", req: UnknownRequest{}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalPermissionRequest(tt.req)
			if err != nil {
				t.Fatalf("MarshalPermissionRequest() error = %v", err)
			}
			got, err := UnmarshalPermissionRequest(data)
			if err != nil {
				t.Fatalf("UnmarshalPermissionRequest() error = %v", err)
			}
			sameRequest(t, got, tt.req)
			// The reconstructed concrete type must match exactly, not merely answer
			// the methods the same way (a different concrete type could coincide).
			if reflect.TypeOf(got) != reflect.TypeOf(tt.req) {
				t.Errorf("reconstructed type = %T, want %T", got, tt.req)
			}
		})
	}
}

// TestUnmarshalPermissionRequestUnknownTag proves an unknown or missing tag fails
// closed with a typed *UnknownPermissionRequestError (mirrors blockTag).
func TestUnmarshalPermissionRequestUnknownTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
		want string // expected Type on the UnknownPermissionRequestError
	}{
		{name: "foreign tag", data: `{"type":"telepathy","Foo":"bar"}`, want: "telepathy"},
		{name: "missing tag", data: `{"Path":"/x"}`, want: ""},
		{name: "empty tag", data: `{"type":"","Path":"/x"}`, want: ""},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := UnmarshalPermissionRequest([]byte(tt.data))
			var unknown *UnknownPermissionRequestError
			if !errors.As(err, &unknown) {
				t.Fatalf("UnmarshalPermissionRequest() error = %v, want *UnknownPermissionRequestError", err)
			}
			if unknown.Type != tt.want {
				t.Errorf("UnknownPermissionRequestError.Type = %q, want %q", unknown.Type, tt.want)
			}
		})
	}
}

// TestUnmarshalPermissionRequestMalformed proves malformed bytes fail with a typed
// decode error, never a panic.
func TestUnmarshalPermissionRequestMalformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "not json", data: `not json`},
		{name: "truncated", data: `{"type":"bash",`},
		{name: "wrong field type", data: `{"type":"bash","command":42}`},
		{name: "array not object", data: `["bash"]`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := UnmarshalPermissionRequest([]byte(tt.data)); err == nil {
				t.Fatalf("UnmarshalPermissionRequest(%q) error = nil, want non-nil", tt.data)
			}
		})
	}
}

// TestMarshalPermissionRequestNil proves a nil or typed-nil request fails closed
// with a typed error rather than emitting a tagless or "null" record (mirrors the
// blockTag / NilBlockError fail-secure contract).
func TestMarshalPermissionRequestNil(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  PermissionRequest
	}{
		{name: "nil interface", req: nil},
		{name: "typed-nil pointer", req: (*nilRequest)(nil)},
		{name: "foreign type", req: foreignRequest{}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := MarshalPermissionRequest(tt.req); err == nil {
				t.Fatalf("MarshalPermissionRequest(%s) error = nil, want non-nil", tt.name)
			}
		})
	}
}

// nilRequest is a sealed concrete type used only to exercise the typed-nil path; it
// implements the sealed interface so the compiler accepts it as a PermissionRequest,
// then the codec must reject a nil pointer to it.
type nilRequest struct{}

func (*nilRequest) permissionRequest()             {}
func (*nilRequest) ToolName() string               { return "nil" }
func (*nilRequest) Description() string            { return "" }
func (*nilRequest) AllowedScopes() []ApprovalScope { return nil }

// foreignRequest is a sealed concrete type NOT in the codec's tagged union, used to
// prove an unrecognized concrete type fails closed on marshal.
type foreignRequest struct{}

func (foreignRequest) permissionRequest()             {}
func (foreignRequest) ToolName() string               { return "foreign" }
func (foreignRequest) Description() string            { return "" }
func (foreignRequest) AllowedScopes() []ApprovalScope { return nil }

// FuzzUnmarshalPermissionRequest exercises the untrusted restore boundary with
// arbitrary bytes. It must never panic. When Unmarshal succeeds, re-marshaling and
// re-unmarshaling must be a stable fixed point (observably equal), proving the codec
// is idempotent on the values it accepts.
func FuzzUnmarshalPermissionRequest(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"type":"file_write","Path":"/repo/main.go"}`),
		[]byte(`{"type":"bash","Command":"ls"}`),
		[]byte(`{"type":"fetch","Method":"GET","URL":"https://x"}`),
		[]byte(`{"type":"web_search","Query":"q"}`),
		[]byte(`{"type":"unknown","Tool":"T","Summary":"s"}`),
		[]byte(`{"type":"telepathy"}`),
		[]byte(`not json`),
		[]byte(``),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := UnmarshalPermissionRequest(data)
		if err != nil {
			return // rejected input is fine; only crashes fail the fuzz
		}
		out, err := MarshalPermissionRequest(req)
		if err != nil {
			t.Fatalf("re-Marshal of accepted request failed: %v", err)
		}
		req2, err := UnmarshalPermissionRequest(out)
		if err != nil {
			t.Fatalf("re-Unmarshal of re-marshaled bytes failed: %v", err)
		}
		if !reflect.DeepEqual(req, req2) {
			t.Fatalf("codec not stable: first = %#v, second = %#v", req, req2)
		}
	})
}
