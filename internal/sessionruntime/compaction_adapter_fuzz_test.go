package sessionruntime

import (
	"encoding/json"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	model "github.com/looprig/inference/model"
)

func FuzzCompactionJSON(f *testing.F) {
	eventID, err := uuid.New()
	if err != nil {
		f.Fatal(err)
	}
	var fingerprint [32]byte
	fingerprint[0] = 1
	input := loop.CompactionInput{
		Basis: event.ContextBasis{Revision: 1, ThroughEventID: eventID},
		Model: model.ModelKey{Provider: "provider", Model: "model"}, RequestFingerprint: fingerprint,
		Transcript:       content.AgenticMessages{&content.UserMessage{Message: content.Message{Role: content.RoleUser}}},
		MaxSummaryTokens: 16,
	}
	inputSeed, err := marshalCompactionInput(input)
	if err != nil {
		f.Fatal(err)
	}
	outputSeed := validCompactionOutputJSONForFuzz(f, input)
	f.Add([]byte(`{}`), false)
	f.Add([]byte(`null`), true)
	f.Add([]byte(inputSeed), false)
	f.Add([]byte(outputSeed), true)
	f.Fuzz(func(t *testing.T, raw []byte, output bool) {
		if output {
			value, validationErr := validateCompactionResult(hustle.Result{Output: raw, Usage: &content.Usage{OutputTokens: 1}}, input, len(raw)+1)
			if validationErr == nil {
				if value == nil || value.Validate() != nil {
					t.Fatal("accepted output is not a valid typed compaction output")
				}
			}
			return
		}
		value, decodeErr := unmarshalCompactionInput(raw)
		if decodeErr == nil && value.Validate() != nil {
			t.Fatal("accepted input is not a valid typed compaction input")
		}
	})
}

func validCompactionOutputJSONForFuzz(f *testing.F, input loop.CompactionInput) json.RawMessage {
	f.Helper()
	wire := struct {
		Version            loop.CompactionWireVersion `json:"version"`
		Basis              event.ContextBasis         `json:"basis"`
		Model              compactionModelWire        `json:"model"`
		RequestFingerprint string                     `json:"request_fingerprint"`
		Summary            string                     `json:"summary"`
	}{
		Version: loop.CompactionWireV1, Basis: input.Basis,
		Model:              compactionModelWire{Provider: input.Model.Provider, Model: input.Model.Model},
		RequestFingerprint: "0100000000000000000000000000000000000000000000000000000000000000",
		Summary:            validCompactionXML,
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		f.Fatal(err)
	}
	return raw
}
