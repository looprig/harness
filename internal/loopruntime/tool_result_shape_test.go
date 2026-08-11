package loopruntime

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestShapeToolResultTextUnderLimitIsIdentity(t *testing.T) {
	t.Parallel()
	text := "small tool output"
	if got := shapeToolResultText(text, len(text)+1); got != text {
		t.Fatalf("shapeToolResultText() = %q, want %q", got, text)
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
