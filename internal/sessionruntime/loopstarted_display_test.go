package sessionruntime

import (
	"context"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// TestLoopStartedCarriesDisplayMetadata is the end-to-end proof that a newly started
// loop's LoopStarted event carries the bound definition's DisplayName + Description
// when declared, and empty strings when the loop declares none. A subscriber attached
// BEFORE NewLoop observes the published event (the hub has no replay).
func TestLoopStartedCarriesDisplayMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		extra           []loop.Option
		wantDisplayName string
		wantDescription string
	}{
		{
			name: "declared",
			extra: []loop.Option{
				loop.WithDisplayName("Planner"),
				loop.WithDescription("plans the work"),
			},
			wantDisplayName: "Planner",
			wantDescription: "plans the work",
		},
		{
			name:            "undeclared",
			extra:           nil,
			wantDisplayName: "",
			wantDescription: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, err := newTestSession(context.Background(), cfg(&stubLLM{chunks: []content.Chunk{textChunk("x")}}))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

			sub, err := s.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents: %v", err)
			}
			t.Cleanup(func() { _ = sub.Close() })

			model := validModel("m")
			model.Limits = testContextLimits{WindowTokens: 64_000, MaxOutputTokens: 8_000}
			model.Sampling.Effort = testEffortHigh
			opts := append([]loop.Option{
				loop.WithName("agent"),
				loop.WithInference(&stubLLM{chunks: []content.Chunk{textChunk("y")}}, model),
				loop.WithDrainTimeout(100 * time.Millisecond),
			}, tt.extra...)
			child := mustDefine(opts...)

			if _, err := s.NewLoop(loop.Provenance{}, child); err != nil {
				t.Fatalf("NewLoop: %v", err)
			}

			ls, ok := firstMatching[event.LoopStarted](t, sub)
			if !ok {
				t.Fatal("no LoopStarted observed on the fan-in")
			}
			if ls.DisplayName != tt.wantDisplayName {
				t.Errorf("LoopStarted.DisplayName = %q, want %q", ls.DisplayName, tt.wantDisplayName)
			}
			if ls.Description != tt.wantDescription {
				t.Errorf("LoopStarted.Description = %q, want %q", ls.Description, tt.wantDescription)
			}
			wantRuntime := runtimeForModel(model)
			if ls.Runtime != wantRuntime {
				t.Errorf("LoopStarted.Runtime = %+v, want %+v", ls.Runtime, wantRuntime)
			}
		})
	}
}
