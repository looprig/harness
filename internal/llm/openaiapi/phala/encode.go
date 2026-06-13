package phala

import (
	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

func encodeRequest(req llm.Request, stream bool) ([]byte, error) {
	return openaiapi.EncodeRequest(req, stream)
}
