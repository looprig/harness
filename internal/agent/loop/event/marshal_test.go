package event

import (
	"encoding/json"
	"testing"

	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/tool"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// seededUUID builds a deterministic non-zero uuid from a single seed byte so the
// marshalled output is stable across runs.
func seededUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

// topLevelKeys parses a JSON object into the set of its top-level key names. It is
// the shape assertion used by the journal-stability tests: we care that the
// produced keys are the stable snake_case names, not the (interface-laden,
// non-round-trippable) values behind them.
func topLevelKeys(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to key map: %v", err)
	}
	return m
}

func hasKey(m map[string]json.RawMessage, key string) bool {
	_, ok := m[key]
	return ok
}

// TestEventBodyJSONKeysAreStableSnakeCase marshals fully-populated representative
// events and asserts the journal output carries the stable snake_case keys from
// the design's Naming Rules. It asserts the marshalled SHAPE (top-level key names)
// rather than a full round-trip: content.Block is a sealed interface with no
// generic UnmarshalJSON, so a whole-event round-trip through encoding/json is
// infeasible for events that carry blocks/messages. The keys are what the journal
// reader depends on, so the shape is the contract under test.
func TestEventBodyJSONKeysAreStableSnakeCase(t *testing.T) {
	t.Parallel()

	hdr := Header{
		Coordinates: identity.Coordinates{
			SessionID: seededUUID(0x11),
			LoopID:    seededUUID(0x22),
			TurnID:    seededUUID(0x33),
			StepID:    seededUUID(0x44),
		},
		EventID: seededUUID(0x55),
		Cause: identity.Cause{
			CommandID: seededUUID(0x66),
			Agency:    identity.AgencyUser,
		},
	}

	tests := []struct {
		name     string
		event    Event
		wantKeys []string
		// absentKeys must NOT appear (e.g. machine default agency is omitzero).
		absentKeys []string
	}{
		{
			name: "ToolCallStarted carries tool_execution_id, tool_name, summary",
			event: ToolCallStarted{
				Header:          hdr,
				ToolExecutionID: seededUUID(0x77),
				ToolName:        "Bash",
				Summary:         "ls -la",
			},
			wantKeys: []string{
				"session_id", "loop_id", "turn_id", "step_id",
				"event_id", "cause", "tool_execution_id", "tool_name", "summary",
			},
		},
		{
			name: "TurnStarted carries turn_index and message",
			event: TurnStarted{
				Header:    hdr,
				TurnIndex: 7,
				Message: &content.UserMessage{Message: content.Message{
					Role:   content.RoleUser,
					Blocks: []content.Block{&content.TextBlock{Text: "hi"}},
				}},
			},
			wantKeys: []string{"session_id", "loop_id", "turn_id", "event_id", "cause", "turn_index", "message"},
		},
		{
			name: "ToolCallCompleted carries is_error and result_preview",
			event: ToolCallCompleted{
				Header:          hdr,
				ToolExecutionID: seededUUID(0x77),
				IsError:         true,
				ResultPreview:   "boom",
			},
			wantKeys: []string{"tool_execution_id", "is_error", "result_preview"},
		},
		{
			// Request is a no-codec sealed interface tagged json:"-"; even a NON-nil
			// request must never reach the journal (it would marshal to lossy,
			// un-keyed PascalCase). Only the addressable tool_execution_id survives.
			name: "PermissionRequested journals tool_execution_id but never request",
			event: PermissionRequested{
				Header:          hdr,
				ToolExecutionID: seededUUID(0x77),
				Request:         tool.BashRequest{Command: "rm -rf /"},
			},
			wantKeys:   []string{"tool_execution_id"},
			absentKeys: []string{"request", "Request"},
		},
		{
			name: "UserInputRequested carries question and choices",
			event: UserInputRequested{
				Header:          hdr,
				ToolExecutionID: seededUUID(0x77),
				Question:        "pick one",
				Choices:         []string{"a", "b"},
			},
			wantKeys: []string{"tool_execution_id", "question", "choices"},
		},
		{
			name: "TurnRejected carries reason",
			event: TurnRejected{
				Header: hdr,
				Reason: RejectQueueFull,
			},
			wantKeys: []string{"event_id", "cause", "reason"},
		},
		{
			// A non-zero reason is used so the omitzero scalar tag emits the key; a
			// zero reason (CancelClientRetracted) is intentionally dropped by omitzero,
			// matching the design's "omitzero for scalars" rule.
			name: "InputCancelled carries reason and message",
			event: InputCancelled{
				Header:    hdr,
				TurnIndex: 1,
				Reason:    CancelTurnInterrupted,
				Message: &content.UserMessage{Message: content.Message{
					Role:   content.RoleUser,
					Blocks: []content.Block{&content.TextBlock{Text: "retract"}},
				}},
			},
			wantKeys: []string{"turn_index", "reason", "message"},
		},
		{
			name: "TurnInterrupted carries turn_index, no machine-default agency in cause",
			event: TurnInterrupted{
				Header:    Header{EventID: seededUUID(0x55)},
				TurnIndex: 2,
			},
			wantKeys:   []string{"event_id", "turn_index"},
			absentKeys: []string{"cause"}, // zero Cause is omitzero
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tt.event)
			if err != nil {
				t.Fatalf("json.Marshal(%T) error = %v", tt.event, err)
			}
			keys := topLevelKeys(t, data)
			for _, want := range tt.wantKeys {
				if !hasKey(keys, want) {
					t.Errorf("%T journal output missing key %q; got keys %v\nraw: %s", tt.event, want, keysOf(keys), data)
				}
			}
			for _, absent := range tt.absentKeys {
				if hasKey(keys, absent) {
					t.Errorf("%T journal output unexpectedly has key %q\nraw: %s", tt.event, absent, data)
				}
			}
		})
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestTurnFailedErrNotMarshalled proves the un-marshalable TurnFailed.Err is tagged
// json:"-": the typed error cause cannot round-trip through encoding/json, so it
// must never appear in the journal as garbage. The turn_index still serializes.
func TestTurnFailedErrNotMarshalled(t *testing.T) {
	t.Parallel()
	ev := TurnFailed{
		Header:    Header{EventID: seededUUID(0x55)},
		TurnIndex: 3,
		Err:       &content.BlockDecodeError{Cause: errSentinel{}},
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal(TurnFailed) error = %v", err)
	}
	keys := topLevelKeys(t, data)
	if hasKey(keys, "err") || hasKey(keys, "Err") {
		t.Errorf("TurnFailed journal output must not carry the error field; got %s", data)
	}
	if !hasKey(keys, "turn_index") {
		t.Errorf("TurnFailed journal output missing turn_index; got %s", data)
	}
}

// TestRestoreErroredErrNotMarshalled proves RestoreErrored.Err is tagged json:"-"
// (mirroring TurnFailed.Err): the typed restore-failure cause cannot round-trip
// through encoding/json, so it must never appear in the journal as garbage. The
// event still marshals (here, only the embedded Header fields).
func TestRestoreErroredErrNotMarshalled(t *testing.T) {
	t.Parallel()
	ev := RestoreErrored{
		Header: Header{EventID: seededUUID(0x66)},
		Err:    errSentinel{},
	}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal(RestoreErrored) error = %v", err)
	}
	keys := topLevelKeys(t, data)
	if hasKey(keys, "err") || hasKey(keys, "Err") {
		t.Errorf("RestoreErrored journal output must not carry the error field; got %s", data)
	}
	if !hasKey(keys, "event_id") {
		t.Errorf("RestoreErrored journal output missing event_id; got %s", data)
	}
}

// errSentinel is a tiny error used only to populate TurnFailed.Err.
type errSentinel struct{}

func (errSentinel) Error() string { return "sentinel" }
