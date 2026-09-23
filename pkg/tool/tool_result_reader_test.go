package tool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
)

type stubToolResultReader struct{}

func (stubToolResultReader) ReadToolResult(context.Context, ToolResultPageRequest) (ToolResultPage, error) {
	return ToolResultPage{}, nil
}

// TestToolResultReaderIsHandedOnlyToDefinitionsThatDeclareIt pins least
// privilege: the reader reaches a factory only through the requirement bit, and
// a definition declaring the bit without a bound reader fails closed.
func TestToolResultReaderIsHandedOnlyToDefinitionsThatDeclareIt(t *testing.T) {
	t.Parallel()
	ids := Bindings{SessionID: uuid.MustParse("0192d1c4-6d7e-7abc-8def-0123456789ab"), LoopID: uuid.MustParse("0192d1c4-6d7e-7abc-8def-0123456789ac")}
	withReader := ids
	withReader.ToolResults = stubToolResultReader{}
	var typedNil *stubToolResultReaderPtr
	withTypedNil := ids
	withTypedNil.ToolResults = typedNil

	tests := []struct {
		name         string
		requirements Requirements
		bindings     Bindings
		wantReader   bool
		wantMissing  bool
	}{
		{name: "declared and bound", requirements: RequiresToolResultReader, bindings: withReader, wantReader: true},
		{name: "not declared", requirements: 0, bindings: withReader},
		{name: "declared but unbound", requirements: RequiresToolResultReader, bindings: ids, wantMissing: true},
		{name: "declared but typed nil", requirements: RequiresToolResultReader, bindings: withTypedNil, wantMissing: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got ToolResultReader
			definition := NewDefinition("reader", tt.requirements, func(_ context.Context, b Bindings) ([]InvokableTool, error) {
				got = b.ToolResults
				return []InvokableTool{namedReaderTool{}}, nil
			})
			_, err := definition.Build(context.Background(), tt.bindings)
			var missing *MissingBindingError
			if tt.wantMissing {
				if !errors.As(err, &missing) || missing.Requirement != RequiresToolResultReader {
					t.Fatalf("Build error = %v, want missing tool result reader", err)
				}
				if !strings.Contains(err.Error(), "tool result reader") {
					t.Fatalf("error %q does not name the missing binding", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if (got != nil) != tt.wantReader {
				t.Fatalf("factory received reader = %v, want %v", got != nil, tt.wantReader)
			}
		})
	}
}

type namedReaderTool struct{}

func (namedReaderTool) Info(context.Context) (*ToolInfo, error) {
	return &ToolInfo{Name: "reader", Schema: []byte(`{"type":"object"}`)}, nil
}

func (namedReaderTool) InvokableRun(context.Context, string) (*ToolResult, error) {
	return TextResult(""), nil
}

type stubToolResultReaderPtr struct{}

func (*stubToolResultReaderPtr) ReadToolResult(context.Context, ToolResultPageRequest) (ToolResultPage, error) {
	return ToolResultPage{}, nil
}

func TestToolResultPageRender(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("0192d1c4-6d7e-7abc-8def-0123456789ab")
	tests := []struct {
		name string
		page ToolResultPage
		want string
	}{
		{
			name: "first of several utf-8 pages",
			page: ToolResultPage{CaptureID: id, Offset: 0, Data: []byte("hello"), CapturedBytes: 10, OriginalBytes: 10, OriginalExact: true, Encoding: ToolResultEncodingUTF8},
			want: "hello\n[capture 0192d1c4-6d7e-7abc-8def-0123456789ab: bytes 0-4 of 10 retained (original 10); next_offset=5]",
		},
		{
			name: "last page of a truncated capture",
			page: ToolResultPage{CaptureID: id, Offset: 5, Data: []byte("world"), CapturedBytes: 10, OriginalBytes: 25, OriginalExact: true, Truncated: true, TruncationReason: "capture_ceiling", Encoding: ToolResultEncodingUTF8},
			want: "world\n[capture 0192d1c4-6d7e-7abc-8def-0123456789ab: bytes 5-9 of 10 retained (original 25); end; truncated: 15 bytes not retained (capture_ceiling)]",
		},
		{
			name: "binary is base64 and says so",
			page: ToolResultPage{CaptureID: id, Offset: 0, Data: []byte{0xff, 0x00, 0x10}, CapturedBytes: 3, OriginalBytes: 9, Truncated: true, TruncationReason: "capture_ceiling", Encoding: ToolResultEncodingBinary},
			want: "/wAQ\n[capture 0192d1c4-6d7e-7abc-8def-0123456789ab: bytes 0-2 of 3 retained (original at least 9); encoding=base64; end; truncated: at least 6 bytes not retained (capture_ceiling)]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.page.Render(); got != tt.want {
				t.Fatalf("Render() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestToolResultReadErrorNeverRendersItsCause(t *testing.T) {
	t.Parallel()
	for _, kind := range []ToolResultReadErrorKind{ToolResultReadUnknownCapture, ToolResultReadNotRetained, ToolResultReadOffsetOutOfRange, ToolResultReadIntegrity, ToolResultReadUnavailable} {
		cause := errors.New("s3://bucket/tenants/secret-key")
		err := &ToolResultReadError{Kind: kind, Cause: cause}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: model-visible text %q carries the store diagnostic", kind, err.Error())
		}
		if !errors.Is(err, cause) {
			t.Errorf("%s: the cause is not reachable for logging", kind)
		}
	}
}
