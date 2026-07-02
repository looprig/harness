package bedrock

import "encoding/json"

// Bedrock InvokeModel body constants for Anthropic-on-Bedrock.
const (
	// fieldModel is the top-level key the Anthropic Messages body carries the model
	// id in. Bedrock takes the model id in the request URL path instead, so this key
	// is removed from the body.
	fieldModel = "model"
	// fieldAnthropicVersion is the key Bedrock requires in place of "model" to select
	// the Anthropic wire contract.
	fieldAnthropicVersion = "anthropic_version"
	// anthropicVersionBedrock is the pinned Bedrock Anthropic wire-contract version.
	anthropicVersionBedrock = "bedrock-2023-05-31"
)

// toBedrockBody rewrites an Anthropic Messages request body into the Bedrock
// InvokeModel body for Anthropic models: it removes the top-level "model" field
// (Bedrock takes the model id in the URL path, not the body) and adds
// "anthropic_version":"bedrock-2023-05-31". The transform is a decode/rewrite/
// re-encode over the JSON object at the serialization boundary — using
// map[string]json.RawMessage keeps every other field byte-identical (no lossy
// round-trip through a typed struct that could drop a field the codec added), and
// leaves domain typing to the codec that produced the input.
func toBedrockBody(anthropicBody []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(anthropicBody, &fields); err != nil {
		return nil, &BodyTransformError{Err: err}
	}

	delete(fields, fieldModel)

	version, err := json.Marshal(anthropicVersionBedrock)
	if err != nil {
		return nil, &BodyTransformError{Err: err}
	}
	fields[fieldAnthropicVersion] = version

	out, err := json.Marshal(fields)
	if err != nil {
		return nil, &BodyTransformError{Err: err}
	}
	return out, nil
}
