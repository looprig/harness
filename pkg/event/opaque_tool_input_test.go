package event

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

const duplicateKeyToolInput = `{"results":[],"results":[{"id":"example"}]}`

func TestStepDoneReplaysDuplicateKeysInToolInput(t *testing.T) {
	t.Parallel()
	newID := func() uuid.UUID {
		id, err := uuid.New()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	e := StepDone{
		Header: Header{Coordinates: identity.Coordinates{SessionID: newID(), LoopID: newID(), TurnID: newID(), StepID: newID()}, EventID: newID(), CreatedAt: time.Now()},
		Messages: content.AgenticMessages{&content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{
			&content.ToolUseBlock{ID: "call-1", Name: "example", Input: json.RawMessage(duplicateKeyToolInput)},
		}}}},
	}
	raw, err := MarshalEvent(e)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	decoded, err := UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	got := decoded.(StepDone).Messages[0].(*content.AIMessage).Blocks[0].(*content.ToolUseBlock).Input
	if string(got) != duplicateKeyToolInput {
		t.Fatalf("tool input = %s, want original bytes %s", got, duplicateKeyToolInput)
	}
	for name, bad := range map[string]string{
		"duplicate envelope key": strings.Replace(string(raw), `"type":"StepDone"`, `"type":"StepDone","type":"StepDone"`, 1),
		"duplicate block key":    strings.Replace(string(raw), `"Input":`, `"Input":{},"Input":`, 1),
	} {
		if bad == string(raw) {
			t.Fatalf("%s: fixture did not change", name)
		}
		if _, err := UnmarshalEvent([]byte(bad)); err == nil {
			t.Fatalf("%s: UnmarshalEvent accepted it", name)
		}
	}
}

func TestGatePreparedResumeReplaysDuplicateKeysInToolInput(t *testing.T) {
	t.Parallel()
	message := &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{
		&content.ToolUseBlock{ID: "call-1", Name: "Ask", Input: json.RawMessage(duplicateKeyToolInput)},
	}}}
	raw, err := MarshalEvent(GatePrepared{Header: gateWireHeader(), Gate: resumeGate(), Resume: &ToolStepResume{Message: message, ToolUseID: "call-1"}})
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	decoded, err := UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	got := decoded.(GatePrepared).Resume.Message.Blocks[0].(*content.ToolUseBlock).Input
	if string(got) != duplicateKeyToolInput {
		t.Fatalf("tool input = %s, want original bytes %s", got, duplicateKeyToolInput)
	}
	bad := strings.Replace(string(raw), `"Input":`, `"Input":{},"Input":`, 1)
	if bad == string(raw) {
		t.Fatal("fixture did not change")
	}
	if _, err := UnmarshalEvent([]byte(bad)); err == nil {
		t.Fatal("UnmarshalEvent accepted a duplicate block key inside resume")
	}
}

func TestRejectDuplicateJSONKeysTreatsOnlyToolUseInputAsOpaque(t *testing.T) {
	t.Parallel()
	dup := `{"results":[],"results":[]}`
	accepted := map[string]string{
		"messages path":           `{"messages":[{"blocks":[{"type":"tool_use","Input":` + dup + `}]}]}`,
		"message path":            `{"message":{"blocks":[{"type":"tool_use","Input":` + dup + `}]}}`,
		"retained path":           `{"retained":[{"blocks":[{"Input":` + dup + `,"type":"tool_use"}]}]}`,
		"summary path":            `{"summary":{"blocks":[{"type":"tool_use","Input":` + dup + `}]}}`,
		"resume path":             `{"resume":{"message":{"blocks":[{"type":"tool_use","Input":` + dup + `}]}}}`,
		"nested under content":    `{"messages":[{"blocks":[{"type":"tool_result","content":[{"type":"tool_use","Input":` + dup + `}]}]}]}`,
		"input before type":       `{"messages":[{"blocks":[{"Input":` + dup + `,"type":"tool_use"}]}]}`,
		"lower-case input member": `{"messages":[{"blocks":[{"type":"tool_use","input":` + dup + `}]}]}`,
	}
	for name, raw := range accepted {
		if err := rejectDuplicateJSONKeys([]byte(raw)); err != nil {
			t.Errorf("%s: rejectDuplicateJSONKeys = %v, want nil", name, err)
		}
	}
	refused := map[string]string{
		"duplicate envelope key":        `{"type":"StepDone","TYPE":"StepDone"}`,
		"duplicate nested envelope key": `{"cause":{"command_id":"one","COMMAND_ID":"two"}}`,
		"top-level input":               `{"input":` + dup + `}`,
		"input off a block path":        `{"resume":{"input":` + dup + `}}`,
		"duplicate outside input":       `{"resume":{"results":[],"results":[]}}`,
		"text block input":              `{"messages":[{"blocks":[{"type":"text","Input":` + dup + `}]}]}`,
		"retained text block input":     `{"retained":[{"blocks":[{"Input":` + dup + `,"type":"text"}]}]}`,
		"untyped block input":           `{"messages":[{"blocks":[{"Input":` + dup + `}]}]}`,
		"duplicate Input member":        `{"messages":[{"blocks":[{"type":"tool_use","Input":{},"input":{}}]}]}`,
		"nested duplicate Input member": `{"messages":[{"blocks":[{"type":"tool_result","content":[{"type":"tool_use","Input":{},"input":{}}]}]}]}`,
		"duplicate block type":          `{"messages":[{"blocks":[{"type":"tool_use","Type":"tool_use","Input":{}}]}]}`,
	}
	for name, raw := range refused {
		if err := rejectDuplicateJSONKeys([]byte(raw)); err == nil {
			t.Errorf("%s: rejectDuplicateJSONKeys accepted %s", name, raw)
		}
	}
}
