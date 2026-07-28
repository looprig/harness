package event

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

func validProcessEventMetadata(kind tool.ProcessLifecycleKind, state tool.ProcessLifecycleState, reason tool.ProcessTerminalReason) tool.ProcessLifecycleMetadata {
	created := time.Unix(300, 1).UTC()
	metadata := tool.ProcessLifecycleMetadata{
		EventID:           seededUUID(0x11),
		Kind:              kind,
		SessionID:         seededUUID(0x22),
		LoopID:            seededUUID(0x33),
		ProcessHandle:     "process_0123-ABCD",
		OriginExecutionID: seededUUID(0x44),
		State:             state,
		ProcessCreatedAt:  created,
		ProcessStartedAt:  created.Add(time.Second),
		ProcessFinishedAt: created.Add(2 * time.Second),
		Reason:            reason,
	}
	switch kind {
	case tool.ProcessLifecycleStarted, tool.ProcessLifecycleBackgrounded:
		metadata.ProcessFinishedAt = time.Time{}
	case tool.ProcessLifecycleStopRequested:
		metadata.ProcessFinishedAt = time.Time{}
	case tool.ProcessLifecycleCompleted:
		if state == tool.ProcessLifecycleExited {
			metadata.HasExitCode = true
			metadata.ExitCode = 23
		}
		if state == tool.ProcessLifecycleFailed {
			metadata.Diagnostic = "bounded failure"
		}
	case tool.ProcessLifecycleLost:
		metadata.Diagnostic = "owner restarted"
	}
	return metadata
}

func processEventHeader(metadata tool.ProcessLifecycleMetadata) Header {
	return Header{
		Coordinates: identity.Coordinates{
			SessionID: metadata.SessionID,
			LoopID:    metadata.LoopID,
		},
		EventID:   metadata.EventID,
		CreatedAt: time.Unix(900, 2).UTC(),
	}
}

func TestProcessLifecycleEventsRoundTripSealedCodec(t *testing.T) {
	t.Parallel()

	records := []Event{
		ProcessStarted{Header: processEventHeader(validProcessEventMetadata(tool.ProcessLifecycleStarted, tool.ProcessLifecycleRunning, 0)), Process: validProcessEventMetadata(tool.ProcessLifecycleStarted, tool.ProcessLifecycleRunning, 0)},
		ProcessBackgrounded{Header: processEventHeader(validProcessEventMetadata(tool.ProcessLifecycleBackgrounded, tool.ProcessLifecycleRunning, 0)), Process: validProcessEventMetadata(tool.ProcessLifecycleBackgrounded, tool.ProcessLifecycleRunning, 0)},
		ProcessCompleted{Header: processEventHeader(validProcessEventMetadata(tool.ProcessLifecycleCompleted, tool.ProcessLifecycleExited, tool.ProcessTerminalExited)), Process: validProcessEventMetadata(tool.ProcessLifecycleCompleted, tool.ProcessLifecycleExited, tool.ProcessTerminalExited)},
		ProcessStopRequested{Header: processEventHeader(validProcessEventMetadata(tool.ProcessLifecycleStopRequested, tool.ProcessLifecycleRunning, tool.ProcessTerminalInterrupted)), Process: validProcessEventMetadata(tool.ProcessLifecycleStopRequested, tool.ProcessLifecycleRunning, tool.ProcessTerminalInterrupted)},
		ProcessLost{Header: processEventHeader(validProcessEventMetadata(tool.ProcessLifecycleLost, tool.ProcessLifecycleLostOnRestore, tool.ProcessTerminalLostOnRestore)), Process: validProcessEventMetadata(tool.ProcessLifecycleLost, tool.ProcessLifecycleLostOnRestore, tool.ProcessTerminalLostOnRestore)},
	}
	for _, record := range records {
		encoded, err := MarshalEvent(record)
		if err != nil {
			t.Fatalf("MarshalEvent(%T) error = %v", record, err)
		}
		decoded, err := UnmarshalEvent(encoded)
		if err != nil {
			t.Fatalf("UnmarshalEvent(%T) error = %v", record, err)
		}
		if !reflect.DeepEqual(decoded, record) {
			t.Fatalf("round trip %T:\n got %#v\nwant %#v", record, decoded, record)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if _, ok := envelope["process"]; !ok {
			t.Fatalf("%T envelope missing process payload", record)
		}
		for _, forbidden := range []string{"command", "output", "stdin", "environment", "path", "pid"} {
			if _, ok := envelope[forbidden]; ok {
				t.Fatalf("%T envelope includes forbidden field %q", record, forbidden)
			}
		}
	}
}

func TestProcessLifecycleEventValidationMatchesHeader(t *testing.T) {
	t.Parallel()

	metadata := validProcessEventMetadata(tool.ProcessLifecycleStarted, tool.ProcessLifecycleRunning, 0)
	valid := ProcessStarted{Header: processEventHeader(metadata), Process: metadata}
	if err := ValidateEvent(valid); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*ProcessStarted)
		field  FieldName
	}{
		{"event id", func(e *ProcessStarted) { e.Header.EventID = seededUUID(0x51) }, FieldEventID},
		{"session id", func(e *ProcessStarted) { e.Header.SessionID = seededUUID(0x52) }, FieldSessionID},
		{"loop id", func(e *ProcessStarted) { e.Header.LoopID = seededUUID(0x53) }, FieldLoopID},
		{"kind", func(e *ProcessStarted) { e.Process.Kind = tool.ProcessLifecycleCompleted }, FieldProcess},
		{"metadata", func(e *ProcessStarted) { e.Process.ProcessHandle = "" }, FieldProcess},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := valid
			tt.mutate(&candidate)
			var validationErr *InvalidEventError
			if err := ValidateEvent(candidate); !errors.As(err, &validationErr) || validationErr.Field != tt.field {
				t.Fatalf("ValidateEvent() error = %T %v, want field %q", err, err, tt.field)
			}
		})
	}
}

func TestProcessLifecycleEventsPreserveProcessTimestamps(t *testing.T) {
	t.Parallel()

	metadata := validProcessEventMetadata(tool.ProcessLifecycleCompleted, tool.ProcessLifecycleExited, tool.ProcessTerminalExited)
	record := ProcessCompleted{Header: processEventHeader(metadata), Process: metadata}
	encoded, err := MarshalEvent(record)
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}
	decoded, err := UnmarshalEvent(encoded)
	if err != nil {
		t.Fatalf("UnmarshalEvent() error = %v", err)
	}
	got := decoded.(ProcessCompleted).Process
	if got.ProcessCreatedAt != metadata.ProcessCreatedAt ||
		got.ProcessStartedAt != metadata.ProcessStartedAt ||
		got.ProcessFinishedAt != metadata.ProcessFinishedAt {
		t.Fatalf("process clocks changed: got %#v want %#v", got, metadata)
	}
	if got.ProcessCreatedAt == record.Header.CreatedAt {
		t.Fatal("process creation time was replaced by event envelope creation time")
	}
}
