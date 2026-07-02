package anthropicapi_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/llm/codec/anthropicapi"
)

// --- shared helpers -------------------------------------------------------

func f64ptr(v float64) *float64 { return &v }
func intptr(v int) *int         { return &v }

// decodeObj unmarshals raw JSON into a field-addressable map.
func decodeObj(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal object: %v (%s)", err, data)
	}
	return m
}

func asString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal string: %v (%s)", err, raw)
	}
	return s
}

func asInt(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("unmarshal int: %v (%s)", err, raw)
	}
	return n
}

func asObjs(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var objs []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &objs); err != nil {
		t.Fatalf("unmarshal array: %v (%s)", err, raw)
	}
	return objs
}

func messagesOf(t *testing.T, body map[string]json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	return asObjs(t, body["messages"])
}

func blocksOf(t *testing.T, msg map[string]json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	return asObjs(t, msg["content"])
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

func toolResultMsg(id string, isErr bool, blocks ...content.Block) *content.ToolResultMessage {
	return &content.ToolResultMessage{
		Message:   content.Message{Role: content.RoleTool, Blocks: blocks},
		ToolUseID: id,
		IsError:   isErr,
	}
}

func textBlock(s string) content.Block { return &content.TextBlock{Text: s} }

func imageURLBlock(url string) content.Block {
	return &content.ImageBlock{MediaType: content.MediaTypeImageJPEG, Source: content.ImageSource{URL: url}}
}

func imageDataBlock(mt content.MediaType, data []byte) content.Block {
	return &content.ImageBlock{MediaType: mt, Source: content.ImageSource{Data: data}}
}

// baseModel is a minimal valid Model for encode tests. Caps default off.
func baseModel() llm.Model {
	return llm.Model{
		Provider:  llm.ProviderPhala,
		APIFormat: llm.APIFormatAnthropic,
		BaseURL:   "https://example.test",
		Name:      "claude-opus-4-8",
	}
}

// --- TestEncodeRequest_SystemAndMessages ----------------------------------

func TestEncodeRequest_SystemAndMessages(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		system        string
		messages      content.AgenticMessages
		wantSystem    string // "" means the field must be absent
		wantSystemKey bool
		wantRoles     []string
	}{
		{
			name:          "system + user + assistant",
			system:        "You are helpful.",
			messages:      content.AgenticMessages{userMsg(textBlock("hi")), aiMsg(textBlock("hello"))},
			wantSystem:    "You are helpful.",
			wantSystemKey: true,
			wantRoles:     []string{"user", "assistant"},
		},
		{
			name:          "empty system omits the field",
			system:        "",
			messages:      content.AgenticMessages{userMsg(textBlock("hi"))},
			wantSystemKey: false,
			wantRoles:     []string{"user"},
		},
		{
			name:          "in-thread SystemMessage folds into top-level system",
			system:        "Base.",
			messages:      content.AgenticMessages{sysMsg(textBlock("More.")), userMsg(textBlock("hi"))},
			wantSystem:    "Base.\n\nMore.",
			wantSystemKey: true,
			wantRoles:     []string{"user"}, // system message does not become a wire message
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, err := anthropicapi.EncodeRequest(llm.Request{Model: baseModel(), System: tc.system, Messages: tc.messages}, false)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := decodeObj(t, data)

			if got := asString(t, body["model"]); got != "claude-opus-4-8" {
				t.Errorf("model = %q, want claude-opus-4-8", got)
			}
			_, hasSystem := body["system"]
			if hasSystem != tc.wantSystemKey {
				t.Errorf("system present = %v, want %v", hasSystem, tc.wantSystemKey)
			}
			if tc.wantSystemKey && asString(t, body["system"]) != tc.wantSystem {
				t.Errorf("system = %q, want %q", asString(t, body["system"]), tc.wantSystem)
			}

			msgs := messagesOf(t, body)
			if len(msgs) != len(tc.wantRoles) {
				t.Fatalf("message count = %d, want %d", len(msgs), len(tc.wantRoles))
			}
			for i, wantRole := range tc.wantRoles {
				if got := asString(t, msgs[i]["role"]); got != wantRole {
					t.Errorf("message[%d].role = %q, want %q", i, got, wantRole)
				}
			}
		})
	}
}

// --- TestEncodeRequest_Tools ----------------------------------------------

func TestEncodeRequest_Tools(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		tools      []llm.Tool
		wantTools  bool
		wantName   string
		wantDesc   string
		wantSchema string
	}{
		{
			name: "tool with schema maps name/description/input_schema",
			tools: []llm.Tool{{
				Name:        "get_weather",
				Description: "Look up weather",
				Schema:      json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			}},
			wantTools:  true,
			wantName:   "get_weather",
			wantDesc:   "Look up weather",
			wantSchema: `{"type":"object","properties":{"city":{"type":"string"}}}`,
		},
		{
			name:       "tool with empty schema defaults to object schema",
			tools:      []llm.Tool{{Name: "noargs"}},
			wantTools:  true,
			wantName:   "noargs",
			wantSchema: `{"type":"object"}`,
		},
		{
			name:      "no tools omits the tools field",
			tools:     nil,
			wantTools: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := llm.Request{Model: baseModel(), Messages: content.AgenticMessages{userMsg(textBlock("hi"))}, Tools: tc.tools}
			data, err := anthropicapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := decodeObj(t, data)
			_, hasTools := body["tools"]
			if hasTools != tc.wantTools {
				t.Fatalf("tools present = %v, want %v", hasTools, tc.wantTools)
			}
			if !tc.wantTools {
				return
			}
			tools := asObjs(t, body["tools"])
			if len(tools) != 1 {
				t.Fatalf("tool count = %d, want 1", len(tools))
			}
			if got := asString(t, tools[0]["name"]); got != tc.wantName {
				t.Errorf("tool name = %q, want %q", got, tc.wantName)
			}
			if tc.wantDesc != "" && asString(t, tools[0]["description"]) != tc.wantDesc {
				t.Errorf("tool description = %q, want %q", asString(t, tools[0]["description"]), tc.wantDesc)
			}
			if got := string(tools[0]["input_schema"]); got != tc.wantSchema {
				t.Errorf("tool input_schema = %s, want %s", got, tc.wantSchema)
			}
		})
	}
}

// --- TestEncodeRequest_Images ---------------------------------------------

func TestEncodeRequest_Images(t *testing.T) {
	t.Parallel()

	raw := []byte{0x1, 0x2, 0x3, 0x4}
	cases := []struct {
		name          string
		block         content.Block
		wantType      string
		wantMediaType string
		wantURL       string
		wantData      string
	}{
		{
			name:     "url image maps to a url source",
			block:    imageURLBlock("https://img.test/a.jpg"),
			wantType: "url",
			wantURL:  "https://img.test/a.jpg",
		},
		{
			name:          "inline data image maps to a base64 source",
			block:         imageDataBlock(content.MediaTypeImagePNG, raw),
			wantType:      "base64",
			wantMediaType: "image/png",
			wantData:      base64.StdEncoding.EncodeToString(raw),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := llm.Request{Model: baseModel(), Messages: content.AgenticMessages{userMsg(tc.block)}}
			data, err := anthropicapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := decodeObj(t, data)
			blocks := blocksOf(t, messagesOf(t, body)[0])
			if len(blocks) != 1 {
				t.Fatalf("block count = %d, want 1", len(blocks))
			}
			if got := asString(t, blocks[0]["type"]); got != "image" {
				t.Fatalf("block type = %q, want image", got)
			}
			source := decodeObj(t, blocks[0]["source"])
			if got := asString(t, source["type"]); got != tc.wantType {
				t.Errorf("source.type = %q, want %q", got, tc.wantType)
			}
			if tc.wantURL != "" && asString(t, source["url"]) != tc.wantURL {
				t.Errorf("source.url = %q, want %q", asString(t, source["url"]), tc.wantURL)
			}
			if tc.wantMediaType != "" && asString(t, source["media_type"]) != tc.wantMediaType {
				t.Errorf("source.media_type = %q, want %q", asString(t, source["media_type"]), tc.wantMediaType)
			}
			if tc.wantData != "" && asString(t, source["data"]) != tc.wantData {
				t.Errorf("source.data = %q, want %q", asString(t, source["data"]), tc.wantData)
			}
		})
	}
}

// --- TestEncodeRequest_EffortThinking -------------------------------------

func TestEncodeRequest_EffortThinking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		thinkingCap  bool
		effort       llm.Effort
		wantThinking bool
		wantEffort   string // "" when output_config must be absent
	}{
		{name: "thinking-capable + high effort emits adaptive + effort", thinkingCap: true, effort: llm.EffortHigh, wantThinking: true, wantEffort: "high"},
		{name: "thinking-capable + max effort maps to max", thinkingCap: true, effort: llm.EffortMax, wantThinking: true, wantEffort: "max"},
		{name: "thinking-capable + low effort maps to low", thinkingCap: true, effort: llm.EffortLow, wantThinking: true, wantEffort: "low"},
		{name: "thinking-capable + no effort emits neither", thinkingCap: true, effort: llm.EffortNone, wantThinking: false},
		{name: "not thinking-capable ignores effort", thinkingCap: false, effort: llm.EffortHigh, wantThinking: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := baseModel()
			m.Caps.Thinking = tc.thinkingCap
			m.Sampling = llm.Sampling{Effort: tc.effort}
			req := llm.Request{Model: m, Messages: content.AgenticMessages{userMsg(textBlock("hi"))}}
			data, err := anthropicapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := decodeObj(t, data)

			_, hasThinking := body["thinking"]
			if hasThinking != tc.wantThinking {
				t.Errorf("thinking present = %v, want %v", hasThinking, tc.wantThinking)
			}
			if tc.wantThinking {
				th := decodeObj(t, body["thinking"])
				if got := asString(t, th["type"]); got != "adaptive" {
					t.Errorf("thinking.type = %q, want adaptive", got)
				}
			}

			_, hasOC := body["output_config"]
			wantOC := tc.wantEffort != ""
			if hasOC != wantOC {
				t.Errorf("output_config present = %v, want %v", hasOC, wantOC)
			}
			if wantOC {
				oc := decodeObj(t, body["output_config"])
				if got := asString(t, oc["effort"]); got != tc.wantEffort {
					t.Errorf("output_config.effort = %q, want %q", got, tc.wantEffort)
				}
			}
		})
	}
}

// --- TestEncodeRequest_ThinkingOmitsSampling ------------------------------

// Current adaptive-thinking Anthropic models reject temperature/top_p sent
// alongside thinking (HTTP 400). The codec reconciles this: when thinking is
// enabled for the request (Caps.Thinking AND a real Effort), temperature and
// top_p are omitted from the body even if the caller set them; otherwise they
// pass through unchanged.
func TestEncodeRequest_ThinkingOmitsSampling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		thinkingCap  bool
		effort       llm.Effort
		wantThinking bool // thinking + output_config present
		wantSampling bool // temperature + top_p present
	}{
		{
			name:         "thinking enabled omits temperature and top_p",
			thinkingCap:  true,
			effort:       llm.EffortHigh,
			wantThinking: true,
			wantSampling: false,
		},
		{
			name:         "thinking-capable but effort none keeps temperature and top_p",
			thinkingCap:  true,
			effort:       llm.EffortNone,
			wantThinking: false,
			wantSampling: true,
		},
		{
			name:         "not thinking-capable keeps temperature and top_p even with effort",
			thinkingCap:  false,
			effort:       llm.EffortHigh,
			wantThinking: false,
			wantSampling: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := baseModel()
			m.Caps.Thinking = tc.thinkingCap
			// Both temperature and top_p are set on every case so absence is the
			// codec's reconciliation, not a missing input.
			m.Sampling = llm.Sampling{Temperature: f64ptr(0.7), TopP: f64ptr(0.9), Effort: tc.effort}
			req := llm.Request{Model: m, Messages: content.AgenticMessages{userMsg(textBlock("hi"))}}
			data, err := anthropicapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := decodeObj(t, data)

			if _, ok := body["thinking"]; ok != tc.wantThinking {
				t.Errorf("thinking present = %v, want %v", ok, tc.wantThinking)
			}
			if _, ok := body["output_config"]; ok != tc.wantThinking {
				t.Errorf("output_config present = %v, want %v", ok, tc.wantThinking)
			}
			if _, ok := body["temperature"]; ok != tc.wantSampling {
				t.Errorf("temperature present = %v, want %v", ok, tc.wantSampling)
			}
			if _, ok := body["top_p"]; ok != tc.wantSampling {
				t.Errorf("top_p present = %v, want %v", ok, tc.wantSampling)
			}
		})
	}
}

// --- TestEncodeRequest_MaxTokensAndSampling -------------------------------

func TestEncodeRequest_MaxTokensAndSampling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		sampling      llm.Sampling
		override      *llm.Sampling
		wantMaxTokens int
		wantTempKey   bool
		wantTemp      float64
		wantTopPKey   bool
		wantStopKey   bool
		wantStop      []string
	}{
		{
			name:          "unset max_tokens uses the codec default",
			sampling:      llm.Sampling{},
			wantMaxTokens: 4096,
		},
		{
			name:          "explicit max_tokens is honored",
			sampling:      llm.Sampling{MaxTokens: intptr(2000)},
			wantMaxTokens: 2000,
		},
		{
			name:          "temperature/top_p/stop_sequences map through",
			sampling:      llm.Sampling{Temperature: f64ptr(0.7), TopP: f64ptr(0.9), Stop: []string{"STOP"}},
			wantMaxTokens: 4096,
			wantTempKey:   true,
			wantTemp:      0.7,
			wantTopPKey:   true,
			wantStopKey:   true,
			wantStop:      []string{"STOP"},
		},
		{
			name:          "override wins over model sampling",
			sampling:      llm.Sampling{MaxTokens: intptr(10)},
			override:      &llm.Sampling{MaxTokens: intptr(999)},
			wantMaxTokens: 999,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := baseModel()
			m.Sampling = tc.sampling
			req := llm.Request{Model: m, Messages: content.AgenticMessages{userMsg(textBlock("hi"))}, Override: tc.override}
			data, err := anthropicapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := decodeObj(t, data)

			if got := asInt(t, body["max_tokens"]); got != tc.wantMaxTokens {
				t.Errorf("max_tokens = %d, want %d", got, tc.wantMaxTokens)
			}
			if _, ok := body["temperature"]; ok != tc.wantTempKey {
				t.Errorf("temperature present = %v, want %v", ok, tc.wantTempKey)
			}
			if tc.wantTempKey {
				var got float64
				if err := json.Unmarshal(body["temperature"], &got); err != nil || got != tc.wantTemp {
					t.Errorf("temperature = %v (err %v), want %v", got, err, tc.wantTemp)
				}
			}
			if _, ok := body["top_p"]; ok != tc.wantTopPKey {
				t.Errorf("top_p present = %v, want %v", ok, tc.wantTopPKey)
			}
			if _, ok := body["stop_sequences"]; ok != tc.wantStopKey {
				t.Errorf("stop_sequences present = %v, want %v", ok, tc.wantStopKey)
			}
			if tc.wantStopKey {
				var got []string
				if err := json.Unmarshal(body["stop_sequences"], &got); err != nil {
					t.Fatalf("unmarshal stop_sequences: %v", err)
				}
				if len(got) != len(tc.wantStop) || got[0] != tc.wantStop[0] {
					t.Errorf("stop_sequences = %v, want %v", got, tc.wantStop)
				}
			}
		})
	}
}

// --- TestEncodeRequest_Stream ---------------------------------------------

func TestEncodeRequest_Stream(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		stream     bool
		wantHasKey bool
		wantStream bool
	}{
		{name: "invoke omits stream", stream: false, wantHasKey: false},
		{name: "stream sets stream true", stream: true, wantHasKey: true, wantStream: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := llm.Request{Model: baseModel(), Messages: content.AgenticMessages{userMsg(textBlock("hi"))}}
			data, err := anthropicapi.EncodeRequest(req, tc.stream)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := decodeObj(t, data)
			raw, ok := body["stream"]
			if ok != tc.wantHasKey {
				t.Fatalf("stream present = %v, want %v", ok, tc.wantHasKey)
			}
			if !tc.wantHasKey {
				return
			}
			var got bool
			if err := json.Unmarshal(raw, &got); err != nil || got != tc.wantStream {
				t.Errorf("stream = %v (err %v), want %v", got, err, tc.wantStream)
			}
		})
	}
}

// --- TestEncodeRequest_Blocks ---------------------------------------------

// Exercises assistant blocks (text/thinking/tool_use), tool_use default input,
// and tool_result mapping (tool_use_id + is_error).
func TestEncodeRequest_Blocks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		conv     content.Conversation
		wantRole string
		assert   func(t *testing.T, blocks []map[string]json.RawMessage)
	}{
		{
			name:     "assistant text + thinking + tool_use",
			conv:     aiMsg(&content.TextBlock{Text: "sure"}, &content.ThinkingBlock{Thinking: "reason", Signature: "sig"}, &content.ToolUseBlock{ID: "toolu_1", Name: "run", Input: json.RawMessage(`{"x":1}`)}),
			wantRole: "assistant",
			assert: func(t *testing.T, blocks []map[string]json.RawMessage) {
				if len(blocks) != 3 {
					t.Fatalf("block count = %d, want 3", len(blocks))
				}
				if got := asString(t, blocks[0]["type"]); got != "text" {
					t.Errorf("blocks[0].type = %q, want text", got)
				}
				if got := asString(t, blocks[1]["type"]); got != "thinking" {
					t.Errorf("blocks[1].type = %q, want thinking", got)
				}
				if got := asString(t, blocks[1]["signature"]); got != "sig" {
					t.Errorf("blocks[1].signature = %q, want sig", got)
				}
				if got := asString(t, blocks[2]["type"]); got != "tool_use" {
					t.Errorf("blocks[2].type = %q, want tool_use", got)
				}
				if got := asString(t, blocks[2]["id"]); got != "toolu_1" {
					t.Errorf("blocks[2].id = %q, want toolu_1", got)
				}
				if got := string(blocks[2]["input"]); got != `{"x":1}` {
					t.Errorf("blocks[2].input = %s, want {\"x\":1}", got)
				}
			},
		},
		{
			name:     "tool_use with empty input defaults to empty object",
			conv:     aiMsg(&content.ToolUseBlock{ID: "toolu_2", Name: "noargs"}),
			wantRole: "assistant",
			assert: func(t *testing.T, blocks []map[string]json.RawMessage) {
				if got := string(blocks[0]["input"]); got != `{}` {
					t.Errorf("input = %s, want {}", got)
				}
			},
		},
		{
			name:     "tool_result maps to a user tool_result block with is_error",
			conv:     toolResultMsg("toolu_1", true, textBlock("boom")),
			wantRole: "user",
			assert: func(t *testing.T, blocks []map[string]json.RawMessage) {
				if len(blocks) != 1 {
					t.Fatalf("block count = %d, want 1", len(blocks))
				}
				if got := asString(t, blocks[0]["type"]); got != "tool_result" {
					t.Errorf("type = %q, want tool_result", got)
				}
				if got := asString(t, blocks[0]["tool_use_id"]); got != "toolu_1" {
					t.Errorf("tool_use_id = %q, want toolu_1", got)
				}
				var isErr bool
				if err := json.Unmarshal(blocks[0]["is_error"], &isErr); err != nil || !isErr {
					t.Errorf("is_error = %v (err %v), want true", isErr, err)
				}
				inner := asObjs(t, blocks[0]["content"])
				if len(inner) != 1 || asString(t, inner[0]["type"]) != "text" {
					t.Errorf("tool_result content = %v, want one text block", inner)
				}
			},
		},
		{
			name:     "tool_result without error omits is_error",
			conv:     toolResultMsg("toolu_9", false, textBlock("ok")),
			wantRole: "user",
			assert: func(t *testing.T, blocks []map[string]json.RawMessage) {
				if _, ok := blocks[0]["is_error"]; ok {
					t.Errorf("is_error present, want absent for a non-error result")
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := llm.Request{Model: baseModel(), Messages: content.AgenticMessages{tc.conv}}
			data, err := anthropicapi.EncodeRequest(req, false)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}
			body := decodeObj(t, data)
			msg := messagesOf(t, body)[0]
			if got := asString(t, msg["role"]); got != tc.wantRole {
				t.Errorf("role = %q, want %q", got, tc.wantRole)
			}
			tc.assert(t, blocksOf(t, msg))
		})
	}
}

// --- TestEncodeRequest_Errors ---------------------------------------------

func TestEncodeRequest_Errors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		messages  content.AgenticMessages
		wantBlock bool
	}{
		{
			name:      "audio block is unsupported",
			messages:  content.AgenticMessages{userMsg(&content.AudioBlock{MediaType: content.MediaTypeAudioMPEG, Data: []byte{1}})},
			wantBlock: true,
		},
		{
			name:      "document block is unsupported",
			messages:  content.AgenticMessages{userMsg(&content.DocumentBlock{MediaType: content.MediaTypeDocumentPDF, Data: []byte{1}})},
			wantBlock: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := anthropicapi.EncodeRequest(llm.Request{Model: baseModel(), Messages: tc.messages}, false)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var ube *anthropicapi.UnsupportedBlockError
			if tc.wantBlock && !errors.As(err, &ube) {
				t.Errorf("error = %v, want *UnsupportedBlockError", err)
			}
		})
	}
}
