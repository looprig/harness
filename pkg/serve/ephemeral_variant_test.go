package serve

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/looprig/core/content/blocktest"
)

// TestEncodeChunkDeltaEncodesEverySealedChunkVariant is the anti-drift guard on
// the live transport. encodeChunkDelta dispatches on the concrete chunk type and
// skips the frame on anything it does not recognize, which is fail-closed for a
// malformed value and data loss for a real one: the turn accumulates a chunk the
// live stream never shows, so a viewer's picture of the reply diverges from the
// message that is actually committed.
//
// The distinct-tag half matters as much as the coverage half. A refusal mapped
// onto "text" would encode successfully and pass any "did it encode" assertion,
// while telling every client the model answered a request it declined.
func TestEncodeChunkDeltaEncodesEverySealedChunkVariant(t *testing.T) {
	t.Parallel()

	seen := map[string]string{}
	for _, chunk := range blocktest.Chunks(t) {
		name := fmt.Sprintf("%T", chunk)
		delta, ok := encodeChunkDelta(chunk)
		if !ok {
			t.Fatalf("encodeChunkDelta(%s) = not ok; every chunk variant needs an arm", name)
		}
		var tagged struct {
			ChunkType string `json:"chunk_type"`
		}
		if err := json.Unmarshal(delta, &tagged); err != nil {
			t.Fatalf("encodeChunkDelta(%s) produced unreadable delta %s: %v", name, delta, err)
		}
		if tagged.ChunkType == "" {
			t.Fatalf("encodeChunkDelta(%s) produced an untagged delta %s", name, delta)
		}
		if other, duplicate := seen[tagged.ChunkType]; duplicate {
			t.Fatalf("%s and %s both encode as chunk_type %q; a client cannot tell them apart",
				name, other, tagged.ChunkType)
		}
		seen[tagged.ChunkType] = name
	}
}
