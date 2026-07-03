package loop

import (
	"context"
	"testing"

	"github.com/looprig/harness/pkg/content"
)

// fakeRuntimeContextProvider is a trivial in-test implementation proving the
// RuntimeContextProvider contract is satisfiable by an outside type.
type fakeRuntimeContextProvider struct {
	blocks []content.Block
}

func (f fakeRuntimeContextProvider) Blocks(context.Context) []content.Block {
	return f.blocks
}

func TestRuntimeContextProviderContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		blocks []content.Block
		want   int
	}{
		{name: "nil blocks", blocks: nil, want: 0},
		{name: "empty blocks", blocks: []content.Block{}, want: 0},
		{name: "single block", blocks: []content.Block{&content.TextBlock{Text: "x"}}, want: 1},
		{
			name:   "multiple blocks",
			blocks: []content.Block{&content.TextBlock{Text: "a"}, &content.TextBlock{Text: "b"}},
			want:   2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var p RuntimeContextProvider = fakeRuntimeContextProvider{blocks: tt.blocks}
			got := p.Blocks(context.Background())
			if len(got) != tt.want {
				t.Errorf("Blocks() len = %d, want %d", len(got), tt.want)
			}
		})
	}
}
