package auto

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/llm"
)

// TestModelAPIFormatSelectsCodecEndToEnd proves the Model.APIFormat axis drives
// codec selection all the way to the wire. Two LM Studio models differ in NOTHING
// but APIFormat (openai vs anthropic); each is built via auto.New and driven,
// through the real transport, at an httptest server that captures the request
// body. The SAME neutral Request must serialize to DIFFERENT dialect-shaped bytes:
// OpenAI chat-completions encodes a single-text user turn as a plain string
// content and omits max_tokens; Anthropic messages encodes it as an array of typed
// blocks and always emits max_tokens. This exercises New -> genericHTTP -> codecFor
// -> transport.Client.Invoke end-to-end, not just the codec registry in isolation.
func TestModelAPIFormatSelectsCodecEndToEnd(t *testing.T) {
	t.Parallel()

	userTurn := content.AgenticMessages{
		&content.UserMessage{Message: content.Message{
			Role:   content.RoleUser,
			Blocks: []content.Block{&content.TextBlock{Text: "ping"}},
		}},
	}

	cases := []struct {
		name             string
		format           llm.APIFormat
		wantContentKind  byte // first byte of messages[0].content: '"' string (openai) or '[' array (anthropic)
		wantMaxTokensKey bool
	}{
		{
			name:             "openai format selects the chat-completions codec",
			format:           llm.APIFormatOpenAI,
			wantContentKind:  '"',
			wantMaxTokensKey: false,
		},
		{
			name:             "anthropic format selects the messages codec",
			format:           llm.APIFormatAnthropic,
			wantContentKind:  '[',
			wantMaxTokensKey: true,
		},
	}

	// Populated by the (sequential, non-parallel) subtests below, then cross-checked.
	bodies := make(map[llm.APIFormat][]byte)
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Deliberately NOT parallel: the subtests populate the shared bodies map,
			// which the cross-check after the loop reads.
			body := captureRequestBody(t, tc.format, userTurn)
			bodies[tc.format] = body

			top := topLevel(t, body)
			c := firstMessageContent(t, top)
			if len(c) == 0 || c[0] != tc.wantContentKind {
				t.Errorf("%s: messages[0].content = %s, want it to start with %q", tc.format, c, tc.wantContentKind)
			}
			if _, ok := top["max_tokens"]; ok != tc.wantMaxTokensKey {
				t.Errorf("%s: max_tokens present = %v, want %v", tc.format, ok, tc.wantMaxTokensKey)
			}
		})
	}

	// The single decisive assertion: one identical Request, two APIFormats, two
	// DIFFERENT wire bodies. If APIFormat were ignored (a single hardwired codec)
	// these would be byte-identical.
	if bytes.Equal(bodies[llm.APIFormatOpenAI], bodies[llm.APIFormatAnthropic]) {
		t.Fatalf("APIFormat did not change the wire body; both encoded to:\n%s", bodies[llm.APIFormatOpenAI])
	}
}

// captureRequestBody builds an LM Studio model of the given APIFormat, wires it
// via auto.New, and Invokes it against a throwaway server that records the POST
// body. The decoded response (and any Invoke error) is intentionally ignored: the
// assertion target is the encoded REQUEST, captured before the server replies.
func captureRequestBody(t *testing.T, format llm.APIFormat, msgs content.AgenticMessages) []byte {
	t.Helper()
	bodyCh := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case bodyCh <- b:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// httptest listens on 127.0.0.1, so the http:// BaseURL clears Model.Validate's
	// loopback exception; LM Studio (AuthNone) needs no key.
	model := llm.CustomModel(llm.ProviderLMStudio, format, srv.URL, "local-model")
	client, err := New(model, "")
	if err != nil {
		t.Fatalf("New(LMStudio, %q) err = %v, want nil", format, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = client.Invoke(ctx, llm.Request{Model: model, Messages: msgs})

	select {
	case b := <-bodyCh:
		if len(b) == 0 {
			t.Fatalf("captured empty request body for %q", format)
		}
		return b
	case <-time.After(3 * time.Second):
		t.Fatalf("no request reached the test server for %q", format)
		return nil
	}
}

// topLevel unmarshals a wire body into a field-addressable top-level object.
func topLevel(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal body: %v (%s)", err, data)
	}
	return m
}

// firstMessageContent returns the raw `content` of messages[0] — a JSON string in
// the OpenAI dialect, a JSON array of blocks in the Anthropic dialect.
func firstMessageContent(t *testing.T, top map[string]json.RawMessage) json.RawMessage {
	t.Helper()
	var msgs []json.RawMessage
	if err := json.Unmarshal(top["messages"], &msgs); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("no messages in body")
	}
	var first map[string]json.RawMessage
	if err := json.Unmarshal(msgs[0], &first); err != nil {
		t.Fatalf("unmarshal messages[0]: %v", err)
	}
	return first["content"]
}
