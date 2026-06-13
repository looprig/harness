package llm_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/llm"
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

func TestModelSpec_Validate(t *testing.T) {
	t.Parallel()

	temp := func(v float64) *float64 { return &v }
	tokens := func(v int) *int { return &v }

	cases := []struct {
		name    string
		spec    llm.ModelSpec
		wantErr bool
	}{
		{
			name:    "valid zero value",
			spec:    llm.ModelSpec{},
			wantErr: false,
		},
		{
			name: "valid thinking budget with temperature 1.0",
			spec: llm.ModelSpec{
				ThinkingBudget: 1000,
				Temperature:    temp(1.0),
			},
			wantErr: false,
		},
		{
			name:    "valid reasoning effort low",
			spec:    llm.ModelSpec{ReasoningEffort: llm.ReasoningEffortLow},
			wantErr: false,
		},
		{
			name:    "valid reasoning effort medium",
			spec:    llm.ModelSpec{ReasoningEffort: llm.ReasoningEffortMedium},
			wantErr: false,
		},
		{
			name:    "valid reasoning effort high",
			spec:    llm.ModelSpec{ReasoningEffort: llm.ReasoningEffortHigh},
			wantErr: false,
		},
		{
			name:    "valid empty reasoning effort",
			spec:    llm.ModelSpec{ReasoningEffort: ""},
			wantErr: false,
		},
		{
			name: "invalid thinking budget nil temperature",
			spec: llm.ModelSpec{
				ThinkingBudget: 1000,
				Temperature:    nil,
			},
			wantErr: true,
		},
		{
			name: "invalid thinking budget temperature not 1.0",
			spec: llm.ModelSpec{
				ThinkingBudget: 1000,
				Temperature:    temp(0.7),
			},
			wantErr: true,
		},
		{
			name:    "invalid reasoning effort extreme",
			spec:    llm.ModelSpec{ReasoningEffort: "extreme"},
			wantErr: true,
		},
		{
			name: "boundary thinking budget zero temperature 0.7 no constraint",
			spec: llm.ModelSpec{
				ThinkingBudget: 0,
				Temperature:    temp(0.7),
			},
			wantErr: false,
		},
		{
			name: "boundary all optional fields nil",
			spec: llm.ModelSpec{
				MaxTokens: nil,
				TopP:      nil,
				Stop:      nil,
			},
			wantErr: false,
		},
		{
			name: "boundary all optional fields set",
			spec: llm.ModelSpec{
				Temperature: temp(0.5),
				TopP:        temp(0.9),
				MaxTokens:   tokens(256),
				Stop:        []string{"</s>"},
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.spec.Validate()
			if tc.wantErr && err == nil {
				t.Error("Validate() returned nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() returned unexpected error: %v", err)
			}
		})
	}
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

func TestModelSpecProviderFields(t *testing.T) {
	t.Parallel()
	spec := llm.ModelSpec{
		Provider: llm.ProviderLMStudio,
		BaseURL:  "http://localhost:1234",
		APIKey:   "sk-test",
		Model:    "qwen",
	}
	if spec.Provider != llm.ProviderLMStudio {
		t.Errorf("Provider = %q, want %q", spec.Provider, llm.ProviderLMStudio)
	}
	if err := spec.Validate(); err != nil {
		t.Errorf("Validate() on a benign spec = %v, want nil", err)
	}
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
