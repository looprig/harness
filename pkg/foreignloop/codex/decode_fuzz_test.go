package codex

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/foreignloop"
)

func FuzzDecodeLine(f *testing.F) {
	f.Add([]byte(`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`))
	f.Add([]byte(`{"type":"turn.started"}`))
	f.Add([]byte(`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"done"}}`))
	f.Add([]byte(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}`))
	f.Add([]byte(`{"type":"turn.failed","error":{"message":"model failed","detail":"context limit"}}`))
	f.Add([]byte(`{"type":"error","error":"plain failure"}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		// Must never panic; either yields events or a *foreignloop.DecodeError.
		_, err := decodeLine(line)
		if err == nil {
			return
		}
		var de *foreignloop.DecodeError
		if !errors.As(err, &de) {
			t.Fatalf("decodeLine() error = %T %v, want *foreignloop.DecodeError", err, err)
		}
	})
}
