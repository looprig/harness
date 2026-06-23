package loop

import (
	"context"
	"testing"
)

func TestToolUseIDFrom(t *testing.T) {
	ctx := WithToolUseID(context.Background(), "toolu_123")
	if got, ok := ToolUseIDFrom(ctx); !ok || got != "toolu_123" {
		t.Fatalf("ToolUseIDFrom = (%q,%v), want (toolu_123,true)", got, ok)
	}
	if _, ok := ToolUseIDFrom(context.Background()); ok {
		t.Error("ToolUseIDFrom on bare ctx = ok, want !ok")
	}
}
