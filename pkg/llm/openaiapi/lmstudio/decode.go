// internal/llm/openaiapi/lmstudio/decode.go
package lmstudio

import (
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/codec/openaiapi"
)

func decodeResponse(body []byte) (*llm.Response, error) {
	return openaiapi.DecodeResponse(body)
}
