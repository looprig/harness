// internal/llm/openaiapi/lmstudio/decode.go
package lmstudio

import (
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

func decodeResponse(body []byte) (*llm.Response, error) {
	return openaiapi.DecodeResponse(body)
}
