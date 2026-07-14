package loop

import (
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
)

func validCompactionInput() CompactionInput {
	return CompactionInput{
		Basis:              event.ContextBasis{Revision: 3, ThroughEventID: uuid.UUID{1}},
		Model:              inference.ModelKey{Provider: "provider", Model: "model"},
		RequestFingerprint: [32]byte{1},
		Transcript: content.AgenticMessages{&content.UserMessage{Message: content.Message{
			Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "work"}},
		}}},
		MaxSummaryTokens: 128,
	}
}

func TestCompactionInputValidate(t *testing.T) {
	t.Parallel()
	typedNil := (*content.UserMessage)(nil)
	tests := []struct {
		name      string
		mutate    func(*CompactionInput)
		wantField CompactionInputField
	}{
		{name: "valid"},
		{name: "zero revision", mutate: func(v *CompactionInput) { v.Basis.Revision = 0 }, wantField: CompactionInputFieldBasis},
		{name: "zero through event", mutate: func(v *CompactionInput) { v.Basis.ThroughEventID = uuid.UUID{} }, wantField: CompactionInputFieldBasis},
		{name: "invalid model", mutate: func(v *CompactionInput) { v.Model.Provider = "" }, wantField: CompactionInputFieldModel},
		{name: "zero request fingerprint", mutate: func(v *CompactionInput) { v.RequestFingerprint = [32]byte{} }, wantField: CompactionInputFieldRequestFingerprint},
		{name: "empty transcript", mutate: func(v *CompactionInput) { v.Transcript = nil }, wantField: CompactionInputFieldTranscript},
		{name: "nil transcript message", mutate: func(v *CompactionInput) { v.Transcript = content.AgenticMessages{nil} }, wantField: CompactionInputFieldTranscript},
		{name: "typed nil transcript message", mutate: func(v *CompactionInput) { v.Transcript = content.AgenticMessages{typedNil} }, wantField: CompactionInputFieldTranscript},
		{name: "wrong concrete role", mutate: func(v *CompactionInput) { v.Transcript[0].(*content.UserMessage).Role = content.RoleAssistant }, wantField: CompactionInputFieldTranscript},
		{name: "zero summary budget", mutate: func(v *CompactionInput) { v.MaxSummaryTokens = 0 }, wantField: CompactionInputFieldMaxSummaryTokens},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := validCompactionInput()
			if tt.mutate != nil {
				tt.mutate(&value)
			}
			err := value.Validate()
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			var inputErr *CompactionInputError
			if !errors.As(err, &inputErr) || inputErr.Field != tt.wantField {
				t.Fatalf("Validate() error = %T %v, want field %q", err, err, tt.wantField)
			}
		})
	}
}

func TestCompactionOutputValidate(t *testing.T) {
	t.Parallel()
	input := validCompactionInput()
	valid := CompactionOutput{
		Basis:              input.Basis,
		Model:              input.Model,
		RequestFingerprint: input.RequestFingerprint,
		Summary: &content.UserMessage{Message: content.Message{
			Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "<conversation_summary/>"}},
		}},
	}
	tests := []struct {
		name    string
		mutate  func(*CompactionOutput)
		wantErr bool
	}{
		{name: "valid"},
		{name: "zero basis", mutate: func(v *CompactionOutput) { v.Basis = event.ContextBasis{} }, wantErr: true},
		{name: "invalid model", mutate: func(v *CompactionOutput) { v.Model.Model = "" }, wantErr: true},
		{name: "zero fingerprint", mutate: func(v *CompactionOutput) { v.RequestFingerprint = [32]byte{} }, wantErr: true},
		{name: "nil summary", mutate: func(v *CompactionOutput) { v.Summary = nil }, wantErr: true},
		{name: "wrong summary role", mutate: func(v *CompactionOutput) { v.Summary.Role = content.RoleAssistant }, wantErr: true},
		{name: "empty summary blocks", mutate: func(v *CompactionOutput) { v.Summary.Blocks = nil }, wantErr: true},
		{name: "multiple summary blocks", mutate: func(v *CompactionOutput) {
			v.Summary.Blocks = append(v.Summary.Blocks, &content.TextBlock{Text: "extra"})
		}, wantErr: true},
		{name: "non-text summary", mutate: func(v *CompactionOutput) { v.Summary.Blocks = []content.Block{&content.ThinkingBlock{Thinking: "x"}} }, wantErr: true},
		{name: "empty text summary", mutate: func(v *CompactionOutput) { v.Summary.Blocks[0].(*content.TextBlock).Text = "" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := valid
			summary := *valid.Summary
			summary.Blocks = append([]content.Block(nil), valid.Summary.Blocks...)
			text := *valid.Summary.Blocks[0].(*content.TextBlock)
			summary.Blocks[0] = &text
			value.Summary = &summary
			if tt.mutate != nil {
				tt.mutate(&value)
			}
			if err := value.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCompactionErrorDomains(t *testing.T) {
	t.Parallel()
	cause := errors.New("bounded cause")
	tests := []struct {
		name   string
		reason InvalidSummaryReason
		valid  bool
	}{
		{name: "wire", reason: InvalidSummaryWire, valid: true},
		{name: "identity", reason: InvalidSummaryIdentity, valid: true},
		{name: "output shape", reason: InvalidSummaryOutputShape, valid: true},
		{name: "byte limit", reason: InvalidSummaryByteLimit, valid: true},
		{name: "token usage", reason: InvalidSummaryTokenUsage, valid: true},
		{name: "token limit", reason: InvalidSummaryTokenLimit, valid: true},
		{name: "xml syntax", reason: InvalidSummaryXMLSyntax, valid: true},
		{name: "xml root", reason: InvalidSummaryXMLRoot, valid: true},
		{name: "xml structure", reason: InvalidSummaryXMLStructure, valid: true},
		{name: "xml content", reason: InvalidSummaryXMLContent, valid: true},
		{name: "zero"},
		{name: "future", reason: "future"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.reason.Valid(); got != tt.valid {
				t.Fatalf("Valid() = %v, want %v", got, tt.valid)
			}
			if !tt.valid {
				return
			}
			err := &InvalidSummaryError{Reason: tt.reason, Cause: cause}
			if !errors.Is(err, cause) || err.Error() == "" {
				t.Fatalf("InvalidSummaryError = %v, want bounded unwrap", err)
			}
		})
	}
	summaryTests := []struct {
		name        string
		measurement event.ContextMeasurement
	}{
		{name: "bounded summary too large error", measurement: event.ContextMeasurement{InputTokens: 9, InputLimit: 8}},
	}
	for _, tt := range summaryTests {
		t.Run(tt.name, func(t *testing.T) {
			if (&SummaryTooLargeError{Measurement: tt.measurement}).Error() == "" {
				t.Fatal("SummaryTooLargeError returned empty message")
			}
		})
	}
}
