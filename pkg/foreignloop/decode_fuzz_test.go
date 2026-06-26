package foreignloop

import "testing"

func FuzzDecodeStreamLine(f *testing.F) {
	f.Add([]byte(`{"type":"system","subtype":"init","session_id":"x"}`))
	f.Add([]byte(`{"type":"result","subtype":"success"}`))
	f.Add([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}`))
	f.Add([]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"n","input":{}}]}}`))
	f.Add([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]}}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		// must never panic; either yields events or a *DecodeError.
		_, _ = decodeStreamLine(line)
	})
}

func FuzzDecodeTranscriptLine(f *testing.F) {
	f.Add([]byte(`{"type":"user","isSidechain":false,"message":{"role":"user","content":"hi"}}`))
	f.Add([]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`))
	f.Add([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"x"}]}}`))
	f.Add([]byte(`{"type":"file-history-snapshot"}`))
	f.Fuzz(func(t *testing.T, line []byte) {
		// must never panic; either yields messages or a *DecodeError.
		_, _, _ = decodeTranscriptLine(line)
	})
}
