package llm

import "testing"

func TestAPIFormatValid(t *testing.T) {
	tests := []struct {
		name string
		f    APIFormat
		want bool
	}{
		{name: "openai", f: APIFormatOpenAI, want: true},
		{name: "anthropic", f: APIFormatAnthropic, want: true},
		{name: "bedrock converse", f: APIFormatBedrockConverse, want: true},
		{name: "gemini", f: APIFormatGemini, want: true},
		{name: "empty is invalid", f: "", want: false},
		{name: "unknown is invalid", f: "bogus", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.f.Valid(); got != tt.want {
				t.Errorf("APIFormat(%q).Valid() = %v, want %v", tt.f, got, tt.want)
			}
		})
	}
}
