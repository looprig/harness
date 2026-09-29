package sessionwire

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

var updateToolGoldens = flag.Bool("update", false, "rewrite the tool-call projection goldens (never the v0.41 ones)")

func toolGoldenHeader() event.Header {
	return event.Header{
		Coordinates: identity.Coordinates{
			SessionID: testUUID(1), LoopID: testUUID(2), TurnID: testUUID(3), StepID: testUUID(4),
		},
		AgentName: identity.AgentName("coder"),
		EventID:   testUUID(5),
		CreatedAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC),
		Cause:     identity.Cause{CommandID: testUUID(7), Agency: identity.AgencyUser},
	}
}

// toolGoldenLegacyEvents are the tool events with ONLY the members harness
// v0.41.x had. Their projection must stay byte-identical to the v0.41 goldens,
// which were captured from the v0.41.1 structs and are never regenerated: the
// new members are omitempty/omitzero, so an event that leaves them zero is
// indistinguishable on the wire from one a released harness produced.
func toolGoldenLegacyEvents() map[string]event.Event {
	header := toolGoldenHeader()
	return map[string]event.Event{
		"tool_call_started_v0.41.json": event.ToolCallStarted{
			Header: header, ToolExecutionID: testUUID(6), ToolName: "Bash", Summary: "go test ./...",
		},
		"tool_call_completed_v0.41.json": event.ToolCallCompleted{
			Header: header, ToolExecutionID: testUUID(6), IsError: true, ResultPreview: "FAIL\n… [truncated]",
		},
	}
}

func toolGoldenEvents() map[string]event.Event {
	header := toolGoldenHeader()
	return map[string]event.Event{
		"tool_call_started.json": event.ToolCallStarted{
			Header: header, ToolExecutionID: testUUID(6), ToolUseID: "toolu_01",
			ToolName: "Bash", Summary: "go test ./...",
		},
		"tool_call_completed.json": event.ToolCallCompleted{
			Header: header, ToolExecutionID: testUUID(6), ToolUseID: "toolu_01", ToolName: "Bash",
			IsError: true, ElapsedMillis: 1234, ResultPreview: "FAIL\n… [truncated]",
		},
	}
}

func projectToolGolden(t *testing.T, ev event.Event) []byte {
	t.Helper()
	got, err := Project(coresessionwire.TenantID("tenant-a"), coresessionwire.SessionID("public-session"), ev)
	if err != nil {
		t.Fatalf("Project(%T) error = %v", ev, err)
	}
	if got.Class != PublicEphemeral {
		t.Fatalf("Project(%T).Class = %q, want %q", ev, got.Class, PublicEphemeral)
	}
	publication := coresessionwire.EphemeralPublication{TenantID: got.TenantID, SessionID: got.SessionID, Body: got.Body}
	if err := publication.Validate(); err != nil {
		t.Fatalf("Core EphemeralPublication.Validate() = %v; body=%s", err, got.Body)
	}
	return append([]byte(got.Body), '\n')
}

func TestToolCallProjectionGoldens(t *testing.T) {
	for name, ev := range toolGoldenEvents() {
		path := filepath.Join("testdata", name)
		got := projectToolGolden(t, ev)
		if *updateToolGoldens {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read golden %s: %v (run with -update)", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s drifted:\n got %s\nwant %s", name, got, want)
		}
	}
}

// TestToolCallProjectionZeroNewMembersMatchesV041 is the omitempty proof: a tool
// event that leaves ToolUseID/ToolName(Completed)/ElapsedMillis zero projects to
// exactly the bytes harness v0.41.1 produced.
func TestToolCallProjectionZeroNewMembersMatchesV041(t *testing.T) {
	t.Parallel()
	for name, ev := range toolGoldenLegacyEvents() {
		want, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("read golden %s: %v", name, err)
		}
		if got := projectToolGolden(t, ev); !bytes.Equal(got, want) {
			t.Errorf("%s: zero new members changed the projection:\n got %s\nwant %s", name, got, want)
		}
	}
}

// TestToolCallProjectionCarriesJoinMembers pins the members a live viewer joins
// a live tool step to its committed StepDone row with.
func TestToolCallProjectionCarriesJoinMembers(t *testing.T) {
	t.Parallel()
	events := toolGoldenEvents()
	var started, completed map[string]any
	if err := json.Unmarshal(projectToolGolden(t, events["tool_call_started.json"]), &started); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(projectToolGolden(t, events["tool_call_completed.json"]), &completed); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"type": "ToolCallStarted", "v": float64(1), "tool_use_id": "toolu_01", "tool_name": "Bash",
		"tool_execution_id": testUUID(6).String(), "summary": "go test ./...",
	} {
		if started[field] != want {
			t.Errorf("started[%q] = %#v, want %#v", field, started[field], want)
		}
	}
	for field, want := range map[string]any{
		"type": "ToolCallCompleted", "v": float64(1), "tool_use_id": "toolu_01", "tool_name": "Bash",
		"tool_execution_id": testUUID(6).String(), "is_error": true, "elapsed_ms": float64(1234),
	} {
		if completed[field] != want {
			t.Errorf("completed[%q] = %#v, want %#v", field, completed[field], want)
		}
	}
}

// TestToolCallBodiesDecodeAcrossVersions: a v0.41 body (no new members) decodes
// into the current structs with the new members zero, and a current body
// round-trips every member.
func TestToolCallBodiesDecodeAcrossVersions(t *testing.T) {
	t.Parallel()
	legacy, err := os.ReadFile(filepath.Join("testdata", "tool_call_completed_v0.41.json"))
	if err != nil {
		t.Fatal(err)
	}
	var old event.ToolCallCompleted
	if err := json.Unmarshal(legacy, &old); err != nil {
		t.Fatalf("decode v0.41 ToolCallCompleted: %v", err)
	}
	if old.ToolUseID != "" || old.ToolName != "" || old.ElapsedMillis != 0 {
		t.Errorf("v0.41 body decoded new members = %q/%q/%d, want zero", old.ToolUseID, old.ToolName, old.ElapsedMillis)
	}
	if old.ToolExecutionID != testUUID(6) || !old.IsError || old.ResultPreview == "" {
		t.Errorf("v0.41 body lost existing members: %+v", old)
	}
	legacyStarted, err := os.ReadFile(filepath.Join("testdata", "tool_call_started_v0.41.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oldStarted event.ToolCallStarted
	if err := json.Unmarshal(legacyStarted, &oldStarted); err != nil {
		t.Fatalf("decode v0.41 ToolCallStarted: %v", err)
	}
	if oldStarted.ToolUseID != "" || oldStarted.ToolName != "Bash" {
		t.Errorf("v0.41 Started decoded = %+v", oldStarted)
	}

	events := toolGoldenEvents()
	var startedBack event.ToolCallStarted
	if err := json.Unmarshal(projectToolGolden(t, events["tool_call_started.json"]), &startedBack); err != nil {
		t.Fatal(err)
	}
	if want := events["tool_call_started.json"].(event.ToolCallStarted); startedBack.ToolUseID != want.ToolUseID ||
		startedBack.ToolName != want.ToolName || startedBack.Summary != want.Summary || startedBack.ToolExecutionID != want.ToolExecutionID {
		t.Errorf("Started round trip = %+v, want %+v", startedBack, want)
	}
	var completedBack event.ToolCallCompleted
	if err := json.Unmarshal(projectToolGolden(t, events["tool_call_completed.json"]), &completedBack); err != nil {
		t.Fatal(err)
	}
	if want := events["tool_call_completed.json"].(event.ToolCallCompleted); completedBack.ToolUseID != want.ToolUseID ||
		completedBack.ToolName != want.ToolName || completedBack.ElapsedMillis != want.ElapsedMillis ||
		completedBack.IsError != want.IsError || completedBack.ResultPreview != want.ResultPreview {
		t.Errorf("Completed round trip = %+v, want %+v", completedBack, want)
	}
}
