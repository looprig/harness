package present_test

import (
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/present"
)

func FuzzFrameValidate(f *testing.F) {
	f.Add(uint8(0), "")
	f.Add(uint8(1), "prefix")
	f.Add(uint8(present.MaxFrameBlocks+1), "x")
	f.Add(uint8(2), "")
	f.Fuzz(func(t *testing.T, count uint8, body string) {
		// Bound allocation while retaining the over-cap boundary.
		n := int(count) % (present.MaxFrameBlocks + 2)
		blocks := make([]content.Block, n)
		for i := range blocks {
			blocks[i] = &content.TextBlock{Text: body}
		}
		frame := present.Frame{Prefix: blocks[:n/2], Suffix: blocks[n/2:]}
		wantValid := n == 0 || (n <= present.MaxFrameBlocks && body != "" && n*len(body) <= present.MaxFrameTextBytes)
		if gotValid := frame.Validate() == nil; gotValid != wantValid {
			t.Fatalf("Validate(count=%d, body bytes=%d) valid=%t, want %t", n, len(body), gotValid, wantValid)
		}
	})
}
