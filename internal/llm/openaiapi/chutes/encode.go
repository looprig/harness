package chutes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/inventivepotter/urvi/internal/llm"
	"github.com/inventivepotter/urvi/internal/llm/openaiapi"
)

// encodeRequest converts a provider-neutral llm.Request to an OpenAI chat
// completions JSON body via openaiapi.EncodeRequest, then injects the
// e2e_response_pk field so the server can seal its response to the client's
// ephemeral ML-KEM encapsulation key.
func encodeRequest(req llm.Request, stream bool, responseEK []byte) ([]byte, error) {
	body, err := openaiapi.EncodeRequest(req, stream)
	if err != nil {
		return nil, err
	}
	return injectResponsePK(body, responseEK)
}

// injectResponsePK adds the base64 ephemeral ML-KEM encapsulation key as the
// e2e_response_pk field of the request JSON object (WIRE.md sections 3-4); the
// server reads it after decrypting and seals its response to it.
func injectResponsePK(plaintext, ek []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(plaintext, &obj); err != nil {
		return nil, fmt.Errorf("chutes encode: re-parse request body: %w", err)
	}
	pk, _ := json.Marshal(base64.StdEncoding.EncodeToString(ek))
	obj["e2e_response_pk"] = pk
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("chutes encode: re-encode request body: %w", err)
	}
	return out, nil
}
