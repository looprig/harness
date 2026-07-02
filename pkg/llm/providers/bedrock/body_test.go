package bedrock_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/auth"
	"github.com/ciram-co/looprig/pkg/llm/codec/anthropicapi"
	"github.com/ciram-co/looprig/pkg/llm/providers/bedrock"
)

// TestBedrockBodyTransform verifies the Anthropic->Bedrock body rewrite via the
// exported Invoke path: the request that reaches the server must (a) drop the
// top-level "model" field, (b) carry "anthropic_version":"bedrock-2023-05-31", and
// (c) preserve the codec's other fields (messages, max_tokens). It is driven
// through a real httptest.Server so the transform is exercised end-to-end, not in
// isolation. Table covers the happy transform plus a body with an explicit
// max_tokens override that must survive the rewrite.
func TestBedrockBodyTransform(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		override     *llm.Sampling
		wantMaxToken float64
	}{
		{name: "default max_tokens survives, model dropped, version added", wantMaxToken: 4096},
		{name: "explicit max_tokens override survives rewrite", override: &llm.Sampling{MaxTokens: intptr(256)}, wantMaxToken: 256},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, bodyCh := bodyCaptureServer(t)
			defer srv.Close()

			c := bedrock.NewWithEndpoint(testCreds(), "us-east-1", srv.URL)
			req := bedrockRequest("anthropic.claude-3-5-sonnet-20241022-v2:0")
			req.Override = tt.override

			if _, err := c.Invoke(context.Background(), req); err != nil {
				t.Fatalf("Invoke() err = %v, want nil", err)
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(<-bodyCh, &fields); err != nil {
				t.Fatalf("unmarshal captured body: %v", err)
			}

			if _, ok := fields["model"]; ok {
				t.Error(`body still carries top-level "model"; Bedrock takes the model id in the URL`)
			}
			var version string
			if raw, ok := fields["anthropic_version"]; !ok {
				t.Error(`body missing "anthropic_version"`)
			} else if err := json.Unmarshal(raw, &version); err != nil || version != "bedrock-2023-05-31" {
				t.Errorf(`anthropic_version = %q (err %v), want "bedrock-2023-05-31"`, version, err)
			}
			if _, ok := fields["messages"]; !ok {
				t.Error(`body missing "messages" (codec field not preserved)`)
			}
			var maxTokens float64
			if raw, ok := fields["max_tokens"]; !ok {
				t.Error(`body missing "max_tokens"`)
			} else if err := json.Unmarshal(raw, &maxTokens); err != nil || maxTokens != tt.wantMaxToken {
				t.Errorf("max_tokens = %v (err %v), want %v", maxTokens, err, tt.wantMaxToken)
			}
		})
	}
}

// TestBedrockBodyTransformPreservesCodecOutput cross-checks that the transform is a
// pure add/remove: encoding the same request with the anthropicapi codec directly
// and diffing keys shows exactly {-model, +anthropic_version} and every other key
// byte-identical.
func TestBedrockBodyTransformPreservesCodecOutput(t *testing.T) {
	t.Parallel()

	req := bedrockRequest("anthropic.claude-3-5-sonnet-20241022-v2:0")
	anthropicBody, err := anthropicapi.Codec{}.EncodeRequest(req, llm.RequestModeInvoke)
	if err != nil {
		t.Fatalf("codec EncodeRequest: %v", err)
	}
	var anthropicFields map[string]json.RawMessage
	if err := json.Unmarshal(anthropicBody, &anthropicFields); err != nil {
		t.Fatalf("unmarshal anthropic body: %v", err)
	}

	srv, bodyCh := bodyCaptureServer(t)
	defer srv.Close()
	c := bedrock.NewWithEndpoint(testCreds(), "us-east-1", srv.URL)
	if _, err := c.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke() err = %v", err)
	}
	var bedrockFields map[string]json.RawMessage
	if err := json.Unmarshal(<-bodyCh, &bedrockFields); err != nil {
		t.Fatalf("unmarshal bedrock body: %v", err)
	}

	// Every anthropic field except "model" must survive byte-identical.
	for k, v := range anthropicFields {
		if k == "model" {
			continue
		}
		if !bytesEqualJSON(bedrockFields[k], v) {
			t.Errorf("field %q changed: bedrock=%s anthropic=%s", k, bedrockFields[k], v)
		}
	}
	// Bedrock adds exactly anthropic_version and drops model.
	if _, ok := bedrockFields["anthropic_version"]; !ok {
		t.Error("bedrock body missing anthropic_version")
	}
	if _, ok := bedrockFields["model"]; ok {
		t.Error("bedrock body still has model")
	}
	if len(bedrockFields) != len(anthropicFields) {
		// -model +anthropic_version nets to the same count.
		t.Errorf("field count = %d, want %d (=anthropic count; -model +anthropic_version)", len(bedrockFields), len(anthropicFields))
	}
}

func bytesEqualJSON(a, b json.RawMessage) bool {
	return string(a) == string(b)
}

// bedrockRequest builds a minimal valid ProviderBedrock request for name.
func bedrockRequest(name string) llm.Request {
	return llm.Request{
		Model: llm.ClaudeOnBedrock(name),
		Messages: content.AgenticMessages{
			&content.UserMessage{Message: content.Message{
				Role:   content.RoleUser,
				Blocks: []content.Block{&content.TextBlock{Text: "hello"}},
			}},
		},
	}
}

func testCreds() auth.SigV4Credentials {
	return auth.SigV4Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
}

func intptr(v int) *int { return &v }
