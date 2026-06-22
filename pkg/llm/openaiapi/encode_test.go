package openaiapi_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/openaiapi"
)

// mustDecode unmarshals raw JSON into a map for field inspection.
func mustDecode(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func messagesFromRaw(t *testing.T, raw map[string]json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(raw["messages"], &msgs); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	return msgs
}

func roleOf(t *testing.T, msg map[string]json.RawMessage) string {
	t.Helper()
	var r string
	if err := json.Unmarshal(msg["role"], &r); err != nil {
		t.Fatalf("unmarshal role: %v", err)
	}
	return r
}

func contentStr(t *testing.T, msg map[string]json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(msg["content"], &s); err != nil {
		t.Fatalf("unmarshal content as string: %v", err)
	}
	return s
}

func userMsg(blocks ...content.Block) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: blocks}}
}

func aiMsg(blocks ...content.Block) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}
}

func sysMsg(blocks ...content.Block) *content.SystemMessage {
	return &content.SystemMessage{Message: content.Message{Role: content.RoleSystem, Blocks: blocks}}
}

func toolMsg(id string, blocks ...content.Block) *content.ToolResultMessage {
	return &content.ToolResultMessage{Message: content.Message{Role: content.RoleTool, Blocks: blocks}, ToolUseID: id}
}

func textBlock(s string) content.Block {
	return &content.TextBlock{Text: s}
}

func imageURLBlock(url string) content.Block {
	return &content.ImageBlock{
		MediaType: content.MediaTypeImageJPEG,
		Source:    content.ImageSource{URL: url},
	}
}

func imageDataBlock(mediaType content.MediaType, data []byte) content.Block {
	return &content.ImageBlock{
		MediaType: mediaType,
		Source:    content.ImageSource{Data: data},
	}
}

func thinkingBlock(text string) content.Block {
	return &content.ThinkingBlock{Thinking: text}
}

func toolUseBlock(id, name string, input json.RawMessage) content.Block {
	return &content.ToolUseBlock{ID: id, Name: name, Input: input}
}

// --- TestEncodeRequest_System ---

func TestEncodeRequest_System(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		systemSpec  string
		messages    content.AgenticMessages
		wantFirst   string
		wantMsgRole string
	}{
		{
			name:       "system from ModelSpec prepended",
			systemSpec: "You are helpful.",
			messages:   content.AgenticMessages{userMsg(textBlock("hi"))},
			wantFirst:  "system",
		},
		{
			name:       "no system: first message is user",
			systemSpec: "",
			messages:   content.AgenticMessages{userMsg(textBlock("hi"))},
			wantFirst:  "user",
		},
		{
			name:       "empty system string treated as absent",
			systemSpec: "",
			messages:   content.AgenticMessages{userMsg(textBlock("hello"))},
			wantFirst:  "user",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := llm.Request{
				Model:    llm.ModelSpec{Model: "test-model", System: tc.systemSpec},
				Messages: tc.messages,
			}
			got, err := openaiapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest error: %v", err)
			}

			raw := mustDecode(t, got)
			msgs := messagesFromRaw(t, raw)
			if len(msgs) == 0 {
				t.Fatal("expected at least one message")
			}
			if got := roleOf(t, msgs[0]); got != tc.wantFirst {
				t.Errorf("first message role = %q, want %q", got, tc.wantFirst)
			}
		})
	}
}

// --- TestEncodeRequest_StreamFlag ---

func TestEncodeRequest_StreamFlag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		stream     bool
		wantStream bool
	}{
		{name: "stream true", stream: true, wantStream: true},
		{name: "stream false", stream: false, wantStream: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := llm.Request{
				Model:    llm.ModelSpec{Model: "m"},
				Messages: content.AgenticMessages{userMsg(textBlock("x"))},
			}
			got, err := openaiapi.EncodeRequest(req, tc.stream)
			if err != nil {
				t.Fatalf("EncodeRequest error: %v", err)
			}

			raw := mustDecode(t, got)
			streamRaw, exists := raw["stream"]
			if tc.wantStream {
				if !exists {
					t.Fatal("expected stream key in JSON")
				}
				var v bool
				if err := json.Unmarshal(streamRaw, &v); err != nil {
					t.Fatalf("unmarshal stream: %v", err)
				}
				if !v {
					t.Error("expected stream=true")
				}
			} else {
				if exists {
					var v bool
					if err := json.Unmarshal(streamRaw, &v); err == nil && v {
						t.Error("expected stream to be absent or false")
					}
				}
			}
		})
	}
}

// --- TestEncodeRequest_Messages ---

func TestEncodeRequest_Messages(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		msgs    content.AgenticMessages
		checkFn func(t *testing.T, msgs []map[string]json.RawMessage)
	}{
		{
			name: "text-only user message: content is string",
			msgs: content.AgenticMessages{userMsg(textBlock("hello world"))},
			checkFn: func(t *testing.T, msgs []map[string]json.RawMessage) {
				t.Helper()
				if len(msgs) < 1 {
					t.Fatal("expected at least 1 message")
				}
				s := contentStr(t, msgs[0])
				if s != "hello world" {
					t.Errorf("content = %q, want %q", s, "hello world")
				}
			},
		},
		{
			name: "mixed user message (text + image URL): content is []chatContentPart",
			msgs: content.AgenticMessages{userMsg(textBlock("look"), imageURLBlock("https://example.com/img.jpg"))},
			checkFn: func(t *testing.T, msgs []map[string]json.RawMessage) {
				t.Helper()
				if len(msgs) < 1 {
					t.Fatal("expected at least 1 message")
				}
				var parts []map[string]json.RawMessage
				if err := json.Unmarshal(msgs[0]["content"], &parts); err != nil {
					t.Fatalf("expected content to be array: %v", err)
				}
				if len(parts) != 2 {
					t.Fatalf("expected 2 parts, got %d", len(parts))
				}
				var typ0 string
				if err := json.Unmarshal(parts[0]["type"], &typ0); err != nil {
					t.Fatalf("failed to unmarshal parts[0].type: %v", err)
				}
				if typ0 != "text" {
					t.Errorf("parts[0].type = %q, want \"text\"", typ0)
				}
				var typ1 string
				if err := json.Unmarshal(parts[1]["type"], &typ1); err != nil {
					t.Fatalf("failed to unmarshal parts[1].type: %v", err)
				}
				if typ1 != "image_url" {
					t.Errorf("parts[1].type = %q, want \"image_url\"", typ1)
				}
			},
		},
		{
			name: "AI message with text: content is string",
			msgs: content.AgenticMessages{aiMsg(textBlock("I am the AI"))},
			checkFn: func(t *testing.T, msgs []map[string]json.RawMessage) {
				t.Helper()
				if len(msgs) < 1 {
					t.Fatal("expected at least 1 message")
				}
				s := contentStr(t, msgs[0])
				if s != "I am the AI" {
					t.Errorf("content = %q, want %q", s, "I am the AI")
				}
			},
		},
		{
			name: "AI message with tool call: has tool_calls, content is empty string",
			msgs: content.AgenticMessages{aiMsg(toolUseBlock("call-1", "my_tool", json.RawMessage(`{"key":"val"}`)))},
			checkFn: func(t *testing.T, msgs []map[string]json.RawMessage) {
				t.Helper()
				if len(msgs) < 1 {
					t.Fatal("expected at least 1 message")
				}
				tcRaw, ok := msgs[0]["tool_calls"]
				if !ok {
					t.Fatal("expected tool_calls key")
				}
				var tc []map[string]json.RawMessage
				if err := json.Unmarshal(tcRaw, &tc); err != nil {
					t.Fatalf("unmarshal tool_calls: %v", err)
				}
				if len(tc) != 1 {
					t.Fatalf("expected 1 tool call, got %d", len(tc))
				}
				s := contentStr(t, msgs[0])
				if s != "" {
					t.Errorf("content = %q, want empty string", s)
				}
			},
		},
		{
			name: "tool message: role=tool, has tool_call_id",
			msgs: content.AgenticMessages{toolMsg("call-99", textBlock("result"))},
			checkFn: func(t *testing.T, msgs []map[string]json.RawMessage) {
				t.Helper()
				if len(msgs) < 1 {
					t.Fatal("expected at least 1 message")
				}
				if r := roleOf(t, msgs[0]); r != "tool" {
					t.Errorf("role = %q, want \"tool\"", r)
				}
				var id string
				if err := json.Unmarshal(msgs[0]["tool_call_id"], &id); err != nil {
					t.Fatalf("unmarshal tool_call_id: %v", err)
				}
				if id != "call-99" {
					t.Errorf("tool_call_id = %q, want \"call-99\"", id)
				}
			},
		},
		{
			name: "system message in conversation: role=system",
			msgs: content.AgenticMessages{sysMsg(textBlock("Be concise."))},
			checkFn: func(t *testing.T, msgs []map[string]json.RawMessage) {
				t.Helper()
				if len(msgs) < 1 {
					t.Fatal("expected at least 1 message")
				}
				if r := roleOf(t, msgs[0]); r != "system" {
					t.Errorf("role = %q, want \"system\"", r)
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := llm.Request{
				Model:    llm.ModelSpec{Model: "m"},
				Messages: tc.msgs,
			}
			got, err := openaiapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest error: %v", err)
			}

			raw := mustDecode(t, got)
			msgs := messagesFromRaw(t, raw)
			tc.checkFn(t, msgs)
		})
	}
}

// --- TestEncodeRequest_ToolResultErrorReachesModel ---

// TestEncodeRequest_ToolResultErrorReachesModel locks the IsError reconciliation:
// the OpenAI Chat Completions tool message has NO structured error flag, so
// ToolResultMessage.IsError is intentionally NOT emitted on the request. The model
// learns a tool errored only through the result's text content (the loop
// error-prefixes it). The encoded tool message must carry role=tool, the
// tool_call_id, and the error text as content — and must NOT carry an is_error field.
func TestEncodeRequest_ToolResultErrorReachesModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		msg         *content.ToolResultMessage
		wantContent string
	}{
		{
			name: "error result: IsError true, error text reaches model",
			msg: func() *content.ToolResultMessage {
				m := toolMsg("call-err", textBlock("tool error: boom"))
				m.IsError = true
				return m
			}(),
			wantContent: "tool error: boom",
		},
		{
			name:        "success result: IsError false, result text reaches model",
			msg:         toolMsg("call-ok", textBlock("ok")),
			wantContent: "ok",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := llm.Request{
				Model:    llm.ModelSpec{Model: "m"},
				Messages: content.AgenticMessages{tc.msg},
			}
			got, err := openaiapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest error: %v", err)
			}

			raw := mustDecode(t, got)
			msgs := messagesFromRaw(t, raw)
			if len(msgs) != 1 {
				t.Fatalf("expected exactly 1 message, got %d", len(msgs))
			}
			if r := roleOf(t, msgs[0]); r != "tool" {
				t.Errorf("role = %q, want \"tool\"", r)
			}
			var id string
			if err := json.Unmarshal(msgs[0]["tool_call_id"], &id); err != nil {
				t.Fatalf("unmarshal tool_call_id: %v", err)
			}
			if id != tc.msg.ToolUseID {
				t.Errorf("tool_call_id = %q, want %q", id, tc.msg.ToolUseID)
			}
			if s := contentStr(t, msgs[0]); s != tc.wantContent {
				t.Errorf("content = %q, want %q (error text must reach the model)", s, tc.wantContent)
			}
			// The OpenAI schema has no is_error field on a tool message: it must NOT
			// be emitted, regardless of ToolResultMessage.IsError.
			if _, ok := msgs[0]["is_error"]; ok {
				t.Error("tool message carries a non-standard is_error field; OpenAI Chat Completions has no such field")
			}
		})
	}
}

// --- TestEncodeRequest_Tools ---

func TestEncodeRequest_Tools(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		tools     []llm.Tool
		wantKey   bool
		wantCount int
	}{
		{
			name:    "no tools: wire has no tools key",
			tools:   nil,
			wantKey: false,
		},
		{
			name: "one tool: wire has tools array",
			tools: []llm.Tool{
				{Name: "search", Description: "search the web", Schema: json.RawMessage(`{"type":"object","properties":{}}`)},
			},
			wantKey:   true,
			wantCount: 1,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := llm.Request{
				Model:    llm.ModelSpec{Model: "m"},
				Messages: content.AgenticMessages{userMsg(textBlock("hello"))},
				Tools:    tc.tools,
			}
			got, err := openaiapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest error: %v", err)
			}

			raw := mustDecode(t, got)
			toolsRaw, exists := raw["tools"]
			if tc.wantKey && !exists {
				t.Fatal("expected tools key in JSON")
			}
			if !tc.wantKey && exists {
				t.Fatal("expected no tools key in JSON")
			}
			if tc.wantKey {
				var tools []map[string]json.RawMessage
				if err := json.Unmarshal(toolsRaw, &tools); err != nil {
					t.Fatalf("unmarshal tools: %v", err)
				}
				if len(tools) != tc.wantCount {
					t.Errorf("tools count = %d, want %d", len(tools), tc.wantCount)
				}
				// Verify shape: type and function fields exist
				var typ string
				if err := json.Unmarshal(tools[0]["type"], &typ); err != nil {
					t.Fatalf("failed to unmarshal tool type: %v", err)
				}
				if typ != "function" {
					t.Errorf("tool type = %q, want \"function\"", typ)
				}
				if _, ok := tools[0]["function"]; !ok {
					t.Error("expected function key in tool")
				}
			}
		})
	}
}

// --- TestEncodeRequest_ThinkingIgnored ---

func TestEncodeRequest_ThinkingIgnored(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msgs content.AgenticMessages
	}{
		{
			name: "thinking block alone: content empty string, no tool_calls",
			msgs: content.AgenticMessages{aiMsg(thinkingBlock("secret thoughts"))},
		},
		{
			name: "thinking block mixed with text: only text survives",
			msgs: content.AgenticMessages{aiMsg(thinkingBlock("hidden"), textBlock("visible"))},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := llm.Request{
				Model:    llm.ModelSpec{Model: "m"},
				Messages: tc.msgs,
			}
			got, err := openaiapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest error: %v", err)
			}

			raw := mustDecode(t, got)
			msgs := messagesFromRaw(t, raw)
			if len(msgs) < 1 {
				t.Fatal("expected at least 1 message")
			}

			// Content must not contain thinking text
			contentBytes := msgs[0]["content"]
			var contentStr string
			if err := json.Unmarshal(contentBytes, &contentStr); err == nil {
				if contentStr == "secret thoughts" || contentStr == "hidden" {
					t.Errorf("thinking text leaked into content: %q", contentStr)
				}
			}

			// No tool_calls
			if _, ok := msgs[0]["tool_calls"]; ok {
				t.Error("unexpected tool_calls key for thinking-only message")
			}
		})
	}
}

// --- TestEncodeRequest_ImageBlock_DataURL ---

func TestEncodeRequest_ImageBlock_DataURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		mediaType content.MediaType
		data      []byte
	}{
		{
			name:      "PNG data becomes data URI",
			mediaType: content.MediaTypeImagePNG,
			data:      []byte{0x89, 0x50, 0x4E, 0x47},
		},
		{
			name:      "JPEG data becomes data URI",
			mediaType: content.MediaTypeImageJPEG,
			data:      []byte{0xFF, 0xD8, 0xFF},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := llm.Request{
				Model:    llm.ModelSpec{Model: "m"},
				Messages: content.AgenticMessages{userMsg(imageDataBlock(tc.mediaType, tc.data))},
			}
			got, err := openaiapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest error: %v", err)
			}

			raw := mustDecode(t, got)
			msgs := messagesFromRaw(t, raw)
			if len(msgs) < 1 {
				t.Fatal("expected at least 1 message")
			}

			var parts []map[string]json.RawMessage
			if err := json.Unmarshal(msgs[0]["content"], &parts); err != nil {
				t.Fatalf("expected content to be array: %v", err)
			}

			found := false
			for _, p := range parts {
				var typ string
				if err := json.Unmarshal(p["type"], &typ); err != nil {
					t.Fatalf("failed to unmarshal part type: %v", err)
				}
				if typ != "image_url" {
					continue
				}
				found = true
				var imgURL map[string]json.RawMessage
				if err := json.Unmarshal(p["image_url"], &imgURL); err != nil {
					t.Fatalf("failed to unmarshal image_url object: %v", err)
				}
				var urlStr string
				if err := json.Unmarshal(imgURL["url"], &urlStr); err != nil {
					t.Fatalf("failed to unmarshal url string: %v", err)
				}

				expectedPrefix := "data:" + string(tc.mediaType) + ";base64,"
				if len(urlStr) < len(expectedPrefix) || urlStr[:len(expectedPrefix)] != expectedPrefix {
					snippet := urlStr
					if len(snippet) > len(expectedPrefix)+10 {
						snippet = snippet[:len(expectedPrefix)+10]
					}
					t.Errorf("URL prefix = %q, want prefix %q", snippet, expectedPrefix)
				}
				expectedB64 := base64.StdEncoding.EncodeToString(tc.data)
				if urlStr != expectedPrefix+expectedB64 {
					t.Errorf("data URI = %q, want %q", urlStr, expectedPrefix+expectedB64)
				}
			}
			if !found {
				t.Error("no image_url part found in content")
			}
		})
	}
}

// --- TestEncodeRequest_ValidJSON ---

func TestEncodeRequest_ValidJSON(t *testing.T) {
	t.Parallel()

	temp := 0.7
	maxTok := 100
	cases := []struct {
		name string
		req  llm.Request
	}{
		{
			name: "minimal request",
			req: llm.Request{
				Model:    llm.ModelSpec{Model: "m"},
				Messages: content.AgenticMessages{userMsg(textBlock("hi"))},
			},
		},
		{
			name: "full request with tools and system",
			req: llm.Request{
				Model: llm.ModelSpec{
					Model:       "gpt-4o",
					System:      "Be helpful.",
					Temperature: &temp,
					MaxTokens:   &maxTok,
					Stop:        []string{"STOP"},
				},
				Messages: content.AgenticMessages{
					userMsg(textBlock("hello"), imageURLBlock("https://x.com/img.jpg")),
					aiMsg(textBlock("hi there")),
					toolMsg("id1", textBlock("result")),
				},
				Tools: []llm.Tool{
					{Name: "calc", Description: "math", Schema: json.RawMessage(`{"type":"object"}`)},
				},
			},
		},
		{
			name: "stream=true",
			req: llm.Request{
				Model:    llm.ModelSpec{Model: "m"},
				Messages: content.AgenticMessages{userMsg(textBlock("stream me"))},
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stream := tc.name == "stream=true"
			got, err := openaiapi.EncodeRequest(tc.req, stream)
			if err != nil {
				t.Fatalf("EncodeRequest error: %v", err)
			}
			if !json.Valid(got) {
				t.Errorf("output is not valid JSON: %s", got)
			}
		})
	}
}
