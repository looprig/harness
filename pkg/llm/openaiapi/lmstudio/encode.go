// pkg/llm/openaiapi/lmstudio/encode.go
package lmstudio

import (
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/codec/openaiapi"
)

func encodeRequest(req llm.Request, stream bool) ([]byte, error) {
	return openaiapi.EncodeRequest(req, stream)
}
