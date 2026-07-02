package llm_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
)

// fakeLLM satisfies llm.LLM for interface compliance testing.
type fakeLLM struct{}

func (f *fakeLLM) Invoke(_ context.Context, _ llm.Request) (*llm.Response, error) {
	return nil, nil
}

func (f *fakeLLM) Stream(_ context.Context, _ llm.Request) (*llm.StreamReader[content.Chunk], error) {
	return nil, nil
}

// compile-time interface check
var _ llm.LLM = (*fakeLLM)(nil)

func TestReasoningEffort_Constants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value llm.ReasoningEffort
		want  string
	}{
		{name: "Low", value: llm.ReasoningEffortLow, want: "low"},
		{name: "Medium", value: llm.ReasoningEffortMedium, want: "medium"},
		{name: "High", value: llm.ReasoningEffortHigh, want: "high"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if string(tc.value) != tc.want {
				t.Errorf("ReasoningEffort%s = %q, want %q", tc.name, tc.value, tc.want)
			}
		})
	}

	// Distinctness check: all three constants must be mutually unique.
	// Done outside parallel subtests to avoid a shared-map data race.
	t.Run("all_distinct", func(t *testing.T) {
		t.Parallel()
		all := []llm.ReasoningEffort{llm.ReasoningEffortLow, llm.ReasoningEffortMedium, llm.ReasoningEffortHigh}
		seen := make(map[llm.ReasoningEffort]bool, len(all))
		for _, v := range all {
			if seen[v] {
				t.Errorf("duplicate ReasoningEffort value: %q", v)
			}
			seen[v] = true
		}
	})
}

func TestLLM_InterfaceCompliance(t *testing.T) {
	t.Parallel()
	// compile-time assertion is at the top of the file via var _ llm.LLM = (*fakeLLM)(nil).
	// This runtime test confirms the type is instantiable and usable as the interface.
	var iface llm.LLM = &fakeLLM{}
	ctx := context.Background()

	resp, err := iface.Invoke(ctx, llm.Request{})
	if err != nil {
		t.Fatalf("fakeLLM.Invoke returned unexpected error: %v", err)
	}
	if resp != nil {
		t.Errorf("fakeLLM.Invoke returned non-nil response, want nil")
	}

	sr, err := iface.Stream(ctx, llm.Request{})
	if err != nil {
		t.Fatalf("fakeLLM.Stream returned unexpected error: %v", err)
	}
	if sr != nil {
		t.Errorf("fakeLLM.Stream returned non-nil StreamReader, want nil")
	}
}

// TestRequest_Fields verifies a Request carries a secret-free Model, a per-agent
// System prompt, messages, tools, and an optional per-call Sampling override.
func TestRequest_Fields(t *testing.T) {
	t.Parallel()

	override := &llm.Sampling{Temperature: f64ptr(0.2)}
	req := llm.Request{
		Model:    llm.ChutesKimiK2(),
		System:   "you are helpful",
		Messages: content.AgenticMessages{},
		Tools:    []llm.Tool{{Name: "search"}},
		Override: override,
	}

	if req.Model.Provider != llm.ProviderChutes {
		t.Errorf("Request.Model.Provider = %q, want chutes", req.Model.Provider)
	}
	if req.System != "you are helpful" {
		t.Errorf("Request.System = %q, want %q", req.System, "you are helpful")
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "search" {
		t.Errorf("Request.Tools = %+v, want one tool named search", req.Tools)
	}
	if req.Override == nil || req.Override.Temperature == nil || *req.Override.Temperature != 0.2 {
		t.Errorf("Request.Override = %+v, want Temperature 0.2", req.Override)
	}

	// A nil Override is the documented "use Model.Sampling" default.
	def := llm.Request{Model: llm.ChutesKimiK2()}
	if def.Override != nil {
		t.Errorf("zero-value Request.Override = %+v, want nil", def.Override)
	}
}

func TestTool_Schema(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		schema json.RawMessage
	}{
		{
			name:   "object schema",
			schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		},
		{
			name:   "empty object",
			schema: json.RawMessage(`{}`),
		},
		{
			name:   "array schema",
			schema: json.RawMessage(`{"type":"array","items":{"type":"number"}}`),
		},
		{
			name:   "nil schema",
			schema: nil,
		},
		{
			name:   "string literal",
			schema: json.RawMessage(`"hello"`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tool := llm.Tool{
				Name:        "search",
				Description: "searches the web",
				Schema:      tc.schema,
			}
			if string(tool.Schema) != string(tc.schema) {
				t.Errorf("Tool.Schema = %q, want %q", tool.Schema, tc.schema)
			}
		})
	}
}

func TestProviderConstants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		provider llm.Provider
		want     string
	}{
		{name: "lmstudio", provider: llm.ProviderLMStudio, want: "lmstudio"},
		{name: "phala", provider: llm.ProviderPhala, want: "phala"},
		{name: "chutes", provider: llm.ProviderChutes, want: "chutes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if string(tc.provider) != tc.want {
				t.Errorf("Provider = %q, want %q", tc.provider, tc.want)
			}
		})
	}

	// Distinctness check: a copy-paste collision between two provider
	// constants would make auto.New dispatch to the wrong backend.
	// Done outside parallel subtests to avoid a shared-map data race.
	t.Run("all_distinct", func(t *testing.T) {
		t.Parallel()
		all := []llm.Provider{llm.ProviderLMStudio, llm.ProviderPhala, llm.ProviderChutes}
		seen := make(map[llm.Provider]bool, len(all))
		for _, p := range all {
			if seen[p] {
				t.Errorf("duplicate Provider value: %q", p)
			}
			seen[p] = true
		}
	})
}

func TestUsage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		inputTokens  int
		outputTokens int
	}{
		{name: "zero", inputTokens: 0, outputTokens: 0},
		{name: "positive", inputTokens: 100, outputTokens: 50},
		{name: "large", inputTokens: 1_000_000, outputTokens: 999_999},
		{name: "input only", inputTokens: 42, outputTokens: 0},
		{name: "output only", inputTokens: 0, outputTokens: 7},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := llm.Usage{
				InputTokens:  tc.inputTokens,
				OutputTokens: tc.outputTokens,
			}
			if u.InputTokens != tc.inputTokens {
				t.Errorf("Usage.InputTokens = %d, want %d", u.InputTokens, tc.inputTokens)
			}
			if u.OutputTokens != tc.outputTokens {
				t.Errorf("Usage.OutputTokens = %d, want %d", u.OutputTokens, tc.outputTokens)
			}
		})
	}
}
