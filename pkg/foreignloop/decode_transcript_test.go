package foreignloop

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
)

type transcriptProjectionGolden struct {
	Groups           []content.AgenticMessages `json:"groups"`
	StepDoneMessages []content.AgenticMessages `json:"step_done_messages"`
	Snapshot         content.AgenticMessages   `json:"snapshot"`
}

type missingHistoryGolden struct {
	Available bool                      `json:"available"`
	Steps     []content.AgenticMessages `json:"steps"`
	Cause     string                    `json:"cause"`
}

func userMessage(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

func assistantMessage(blocks ...content.Block) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: blocks,
	}}
}

func transcriptCases() map[string][]content.AgenticMessages {
	return map[string][]content.AgenticMessages{
		"happy": {
			{userMessage("hi there")},
			{
				assistantMessage(
					&content.ThinkingBlock{Thinking: "let me think", Signature: "sig"},
					&content.TextBlock{Text: "Working"},
					&content.ToolUseBlock{ID: "toolu_9", Name: "Read", Input: json.RawMessage(`{"path":"/x"}`)},
				),
				&content.ToolResultMessage{
					Message: content.Message{
						Role:   content.RoleTool,
						Blocks: []content.Block{&content.TextBlock{Text: "contents"}},
					},
					ToolUseID: "toolu_9",
				},
			},
			{assistantMessage(&content.TextBlock{Text: "Done"})},
		},
		"empty": nil,
		"truncated": {
			{userMessage("hi")},
			{assistantMessage(&content.TextBlock{Text: "recovered"})},
		},
	}
}

func flattenTranscriptGroups(groups []content.AgenticMessages) content.AgenticMessages {
	var out content.AgenticMessages
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

func canonicalJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal canonical JSON: %v", err)
	}
	return append(data, '\n')
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "transcript", name+".golden.json"))
	if err != nil {
		t.Fatalf("read golden %q: %v", name, err)
	}
	return data
}

func TestDecodeTranscriptGoldenProjection(t *testing.T) {
	t.Parallel()
	for name, wantGroups := range transcriptCases() {
		name, wantGroups := name, wantGroups
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join("testdata", "transcript", name+".jsonl")
			gotGroups, err := decodeTranscriptTail(path, 12345)
			if err != nil {
				t.Fatalf("decodeTranscriptTail() error = %v", err)
			}
			if !reflect.DeepEqual(gotGroups, wantGroups) {
				t.Fatalf("decodeTranscriptTail() = %#v, want %#v", gotGroups, wantGroups)
			}

			var gotSteps []content.AgenticMessages
			gotSnapshot := (&Loop{}).commitTurn(path, event.TurnIndex(99), nil, func(ev event.Event) {
				step, ok := ev.(event.StepDone)
				if !ok {
					t.Fatalf("commitTurn published %T, want event.StepDone", ev)
				}
				gotSteps = append(gotSteps, step.Messages)
			})
			if !reflect.DeepEqual(gotSteps, wantGroups) {
				t.Errorf("StepDone.Messages = %#v, want %#v", gotSteps, wantGroups)
			}
			wantSnapshot := flattenTranscriptGroups(wantGroups)
			if !reflect.DeepEqual(gotSnapshot, wantSnapshot) {
				t.Errorf("committed snapshot = %#v, want %#v", gotSnapshot, wantSnapshot)
			}

			gotJSON := canonicalJSON(t, transcriptProjectionGolden{
				Groups:           gotGroups,
				StepDoneMessages: gotSteps,
				Snapshot:         gotSnapshot,
			})
			if wantJSON := readGolden(t, name); !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("canonical projection differs from %s.golden.json\ngot:\n%s\nwant:\n%s", name, gotJSON, wantJSON)
			}
		})
	}
}

func TestDecodeTranscriptMissingFileGolden(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "transcript", "missing.jsonl")
	got, err := decodeTranscriptTail(path, 0)
	if err == nil {
		t.Fatal("decodeTranscriptTail() error = nil, want unavailable error")
	}
	if got != nil {
		t.Fatalf("decodeTranscriptTail() groups = %#v, want nil", got)
	}
	var unavailable *TranscriptUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %T %v, want *TranscriptUnavailableError", err, err)
	}
	if unavailable.Path != path {
		t.Errorf("TranscriptUnavailableError.Path = %q, want %q", unavailable.Path, path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(error, os.ErrNotExist) = false: %v", err)
	}

	gotJSON := canonicalJSON(t, missingHistoryGolden{
		Available: false,
		Steps:     got,
		Cause:     "not_exist",
	})
	if wantJSON := readGolden(t, "missing"); !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("canonical missing-file behavior differs\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
	}
}
