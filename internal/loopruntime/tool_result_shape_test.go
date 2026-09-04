package loopruntime

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/event"
)

func TestShapeToolResultTextUnderLimitIsIdentity(t *testing.T) {
	t.Parallel()
	text := "small tool output"
	if got := shapeToolResultText(text, len(text)+1); got != text {
		t.Fatalf("shapeToolResultText() = %q, want %q", got, text)
	}
}

func TestShapeToolResultTextLargeValidUnderLimitUsesBoundedAllocations(t *testing.T) {
	text := strings.Repeat("x", 1<<20)
	allocs := testing.AllocsPerRun(10, func() {
		if got := shapeToolResultText(text, len(text)+1); got != text {
			t.Fatalf("shapeToolResultText() changed under-limit valid text")
		}
	})
	if allocs > 1 {
		t.Fatalf("under-limit valid shaping allocations = %.1f, want <= 1", allocs)
	}
}

func TestShapeToolResultTextZeroLimitIsIdentity(t *testing.T) {
	t.Parallel()
	text := string([]byte{'a', 0xff, 'b'})
	if got := shapeToolResultText(text, 0); got != text {
		t.Fatalf("shapeToolResultText() = %q, want original bytes %q", got, text)
	}
}

func TestShapeToolResultTextExactLimitIsIdentity(t *testing.T) {
	t.Parallel()
	text := "exactly at the configured limit"
	if got := shapeToolResultText(text, len(text)); got != text {
		t.Fatalf("shapeToolResultText() = %q, want %q", got, text)
	}
}

func TestShapeToolResultTextMarkerCountsTowardByteLimit(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("x", 1024)
	const limit = 256
	got := shapeToolResultText(text, limit)
	if len(got) > limit {
		t.Fatalf("shaped bytes = %d, want <= %d", len(got), limit)
	}
	if len(got) != limit {
		t.Fatalf("shaped bytes = %d, want exact limit %d", len(got), limit)
	}
	if !strings.Contains(got, fmt.Sprintf(" of %d bytes]\n", len(text))) {
		t.Fatalf("shaped output %q does not report original byte count %d", got, len(text))
	}
}

func TestShapeToolResultTextKeepsHeadAndTailSentinels(t *testing.T) {
	t.Parallel()
	text := "HEAD-SENTINEL\n" + strings.Repeat("middle\n", 256) + "TAIL-SENTINEL"
	got := shapeToolResultText(text, 256)
	if !strings.HasPrefix(got, "HEAD-SENTINEL") {
		t.Fatalf("shaped output lost head sentinel: %q", got)
	}
	if !strings.HasSuffix(got, "TAIL-SENTINEL") {
		t.Fatalf("shaped output lost tail sentinel: %q", got)
	}
}

func TestShapeToolResultTextKeepsUTF8Boundaries(t *testing.T) {
	t.Parallel()
	text := "前頭" + strings.Repeat("世界🙂", 256) + "末尾"
	got := shapeToolResultText(text, 256)
	if !utf8.ValidString(got) {
		t.Fatalf("shaped output is invalid UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "前頭") {
		t.Fatalf("shaped output lost multibyte head sentinel: %q", got)
	}
	if !strings.HasSuffix(got, "末尾") {
		t.Fatalf("shaped output lost multibyte tail sentinel: %q", got)
	}
}

func TestShapeToolResultTextNormalizesInvalidUTF8(t *testing.T) {
	t.Parallel()
	text := string([]byte{'h', 0xff, 'i'})
	want := strings.ToValidUTF8(text, "\uFFFD")
	if got := shapeToolResultText(text, len(want)+1); got != want {
		t.Fatalf("shapeToolResultText() = %q, want normalized %q", got, want)
	}
}

func TestShapeToolResultTextMarkerReportsOriginalBytesAfterNormalization(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("x", 1024) + string([]byte{0xff, 0xfe})
	got := shapeToolResultText(text, 256)
	if !utf8.ValidString(got) {
		t.Fatalf("shaped output is invalid UTF-8: %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf(" of %d bytes]\n", len(text))) {
		t.Fatalf("shaped output %q does not report original byte count %d", got, len(text))
	}
}

func TestShapeToolResultTextAccountsOriginalBytesForAlternatingInvalidUTF8(t *testing.T) {
	t.Parallel()
	text := string(bytes.Repeat([]byte{'A', 0xff}, 600))
	got := shapeToolResultText(text, 256)
	const markerPrefix = "\n[tool output truncated: omitted "
	markerStart := strings.Index(got, markerPrefix)
	if markerStart < 0 {
		t.Fatalf("shaped output lacks truncation marker: %q", got)
	}
	markerEnd := strings.Index(got[markerStart+len(markerPrefix):], "]\n")
	if markerEnd < 0 {
		t.Fatalf("shaped output marker is incomplete: %q", got)
	}
	markerEnd += markerStart + len(markerPrefix)
	var omitted, total int
	if _, err := fmt.Sscanf(got[markerStart+len(markerPrefix):markerEnd], "%d of %d bytes", &omitted, &total); err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	if omitted > total {
		t.Fatalf("marker omitted %d bytes out of total %d bytes", omitted, total)
	}
	retained := got[:markerStart] + got[markerEnd+2:]
	represented := 0
	for _, r := range retained {
		if r == '\uFFFD' {
			represented++
		} else {
			represented += len(string(r))
		}
	}
	if omitted+represented != len(text) {
		t.Fatalf("marker accounting = omitted %d + represented %d = %d, want original %d", omitted, represented, omitted+represented, len(text))
	}
}

func TestShapeToolResultTextIsDeterministic(t *testing.T) {
	t.Parallel()
	text := "head" + strings.Repeat("🙂middle", 256) + "tail"
	want := shapeToolResultText(text, 256)
	for i := 0; i < 10; i++ {
		if got := shapeToolResultText(text, 256); got != want {
			t.Fatalf("run %d = %q, want stable %q", i, got, want)
		}
	}
}

func TestShapeToolResultTextTinyLimitsStayBoundedAndValid(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("0123456789", 128)
	for _, limit := range []int{1, 2, 3, 4, 16, 32} {
		limit := limit
		t.Run(fmt.Sprintf("limit-%d", limit), func(t *testing.T) {
			t.Parallel()
			got := shapeToolResultText(text, limit)
			if len(got) > limit {
				t.Fatalf("shaped bytes = %d, want <= %d", len(got), limit)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("shaped output is invalid UTF-8: %q", got)
			}
		})
	}
}

func TestResolveToolSetCapsResultBytes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		in   int
		want int
	}{
		{name: "zero stays off", in: 0, want: 0},
		{name: "positive preserved", in: 1024, want: 1024},
		{name: "negative becomes off", in: -1, want: 0},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveToolSetCaps(ToolSet{MaxToolResultBytes: tt.in})
			if got.MaxToolResultBytes != tt.want {
				t.Fatalf("MaxToolResultBytes = %d, want %d", got.MaxToolResultBytes, tt.want)
			}
		})
	}
}

// TestToolResultRetainedMarkerRendersInexactSizeAsLowerBound is the direct
// coverage toolResultRetainedMarker's doc comment points at. The materialized
// path always knows the producer's exact length, so this branch is reached only
// through a capture whose OriginalBytes is nil — which is why it is exercised
// here rather than through a turn.
func TestToolResultRetainedMarkerRendersInexactSizeAsLowerBound(t *testing.T) {
	t.Parallel()
	exact := uint64(900)
	tests := []struct {
		name    string
		capture event.ToolResultCapture
		want    string
	}{
		{
			name:    "exact and complete",
			capture: event.ToolResultCapture{ToolUseID: "tu-1", CapturedBytes: 900, OriginalBytes: &exact},
			want:    "\n[tool output shaped; all 900 bytes retained for tool_use_id \"tu-1\"]\n",
		},
		{
			name:    "exact and truncated",
			capture: event.ToolResultCapture{ToolUseID: "tu-1", CapturedBytes: 100, OriginalBytes: &exact, Truncated: true},
			want:    "\n[tool output shaped; 100 of 900 bytes retained for tool_use_id \"tu-1\"]\n",
		},
		{
			name:    "inexact reports a lower bound",
			capture: event.ToolResultCapture{ToolUseID: "tu-1", CapturedBytes: 100, OriginalBytesLowerBound: 900, Truncated: true},
			want:    "\n[tool output shaped; 100 of at least 900 bytes retained for tool_use_id \"tu-1\"]\n",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := toolResultRetainedMarker(tt.capture); got != tt.want {
				t.Fatalf("marker = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestToolResultRetainedMarkerNamesTheCallNotTheObject pins that the identity of
// the retained object never reaches the model prompt. The identity is opaque, so
// leaking it would not disclose a backend path, but the marker is prompt text and
// the model has no use for an identity it cannot resolve.
func TestToolResultRetainedMarkerNamesTheCallNotTheObject(t *testing.T) {
	t.Parallel()
	size := uint64(1200)
	reference := newCaptureReference(strings.Repeat("ab", 32))
	marker := toolResultRetainedMarker(event.ToolResultCapture{
		ToolUseID: "tu-7", CapturedBytes: 1200, OriginalBytes: &size, Reference: &reference,
	})
	if !strings.Contains(marker, `"tu-7"`) {
		t.Fatalf("marker %q does not name the call it describes", marker)
	}
	if strings.Contains(marker, reference.ObjectID) || strings.Contains(marker, captureObjectIDPrefix) {
		t.Fatalf("marker %q carries the object identity", marker)
	}
}

// TestToolResultCapturePreviewFitsTheModelBudget enumerates limits either side
// of the marker length rather than pinning one, because the function's bound is
// stated conditionally: at most limit whenever limit exceeds len(marker), and the
// marker alone otherwise.
func TestToolResultCapturePreviewFitsTheModelBudget(t *testing.T) {
	t.Parallel()
	const marker = "\n[marker]\n"
	text := strings.Repeat("s", 4096)
	for _, limit := range []int{1, len(marker) - 1, len(marker), len(marker) + 1, 64, 256, 4096, 8192} {
		got := shapeCapturedToolResultText(text, limit, marker)
		if !strings.HasSuffix(got, marker) {
			t.Fatalf("limit=%d: result %q does not end with the marker", limit, got)
		}
		switch {
		case limit > len(marker):
			if len(got) > limit {
				t.Errorf("limit=%d: result is %d bytes, want <= %d", limit, len(got), limit)
			}
			if len(got) <= len(marker) {
				t.Errorf("limit=%d: result is marker-only although the budget allows a preview", limit)
			}
		default:
			if got != marker {
				t.Errorf("limit=%d: result = %q, want the marker alone", limit, got)
			}
		}
	}
	if got := shapeCapturedToolResultText(text, 0, marker); got != text+marker {
		t.Fatal("a zero limit must leave the preview unbounded, matching shapeToolResultText")
	}
}
