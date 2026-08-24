package tool_test

import (
	"testing"

	"github.com/looprig/harness/pkg/tool"
)

func TestMutationPreviewIsInert(t *testing.T) {
	// Harness must never interpret the diff. This test exists to pin the
	// contract: the type is data, it has no parsing behaviour, and a zero value
	// is meaningful (no preview).
	var zero tool.MutationPreview
	if zero.UnifiedDiff != "" || zero.Path != "" || zero.Creates {
		t.Fatalf("zero MutationPreview is not empty: %+v", zero)
	}
}

type previewer struct{ ok bool }

func (p previewer) MutationPreview() (tool.MutationPreview, bool) {
	return tool.MutationPreview{Path: "a.go", UnifiedDiff: "@@"}, p.ok
}

func TestMutationPreviewerSignalsFailure(t *testing.T) {
	var iface tool.MutationPreviewer = previewer{ok: false}
	if _, ok := iface.MutationPreview(); ok {
		t.Fatal("a failing previewer reported ok")
	}
}
