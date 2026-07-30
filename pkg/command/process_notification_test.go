package command_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

// validProcessNotification builds a fully-populated, valid ProcessNotification
// whose Header.CommandID and Notification.CommandID carry the SAME stable id
// (Task 4/24C's contract).
func validProcessNotification(id, sess, loop uuid.UUID) command.ProcessNotification {
	return command.ProcessNotification{
		Header: command.Header{CommandID: id, Agency: identity.AgencyMachine},
		Notification: tool.ProcessCompletionNotification{
			CommandID:     id,
			SessionID:     sess,
			LoopID:        loop,
			ProcessHandle: "proc-handle-01",
			State:         tool.ProcessLifecycleExited,
			Reason:        tool.ProcessTerminalExited,
		},
	}
}

// TestProcessNotificationRejectsOutputAndHostData proves the wrapped
// tool.ProcessCompletionNotification DTO is metadata-only end to end through
// the command codec: the wire form never carries a forbidden key on a
// legitimate encode, and an attempt to SMUGGLE command text, output, stdin, a
// host path, or an OS PID onto the wire — at either the envelope level or
// inside "notification" — is rejected fail-closed by UnmarshalCommand with a
// typed *CommandDecodeError, mirroring pkg/event's identical guarantee for
// Task 4's process lifecycle events (TestProcessLifecycleEventsRejectUnknownWireFields).
func TestProcessNotificationRejectsOutputAndHostData(t *testing.T) {
	t.Parallel()

	id, sess, loop := newID(t), newID(t), newID(t)
	notification := validProcessNotification(id, sess, loop)

	data, err := command.MarshalCommand(notification)
	if err != nil {
		t.Fatalf("MarshalCommand() error = %v", err)
	}
	for _, forbidden := range []string{"command", "output", "stdin", "environment", "path", "pid", "host_path", "os_pid"} {
		if bytesContainsKey(t, data, forbidden) {
			t.Errorf("marshaled ProcessNotification unexpectedly carries forbidden key %q\nwire: %s", forbidden, data)
		}
	}

	unknownValues := map[string]json.RawMessage{
		"command":     json.RawMessage(`"printf secret"`),
		"output":      json.RawMessage(`"secret output"`),
		"stdin":       json.RawMessage(`"secret input"`),
		"environment": json.RawMessage(`{"TOKEN":"secret"}`),
		"path":        json.RawMessage(`"/host/private"`),
		"pid":         json.RawMessage(`31337`),
		"unexpected":  json.RawMessage(`true`),
	}
	for field, value := range unknownValues {
		field, value := field, value
		for _, location := range []string{"envelope", "notification"} {
			location := location
			t.Run(location+"/"+field, func(t *testing.T) {
				t.Parallel()

				wire := processNotificationWireWithUnknownField(t, notification, location, field, value)
				decoded, err := command.UnmarshalCommand(wire)
				if decoded != nil {
					t.Fatalf("UnmarshalCommand() command = %#v, want nil", decoded)
				}
				var decodeErr *command.CommandDecodeError
				if !errors.As(err, &decodeErr) {
					t.Fatalf("UnmarshalCommand() error = %T %v, want *CommandDecodeError", err, err)
				}
			})
		}
	}
}

// bytesContainsKey reports whether the top-level JSON object encoded in data
// carries the given key (a strict structural check, not a substring search).
func bytesContainsKey(t *testing.T, data []byte, key string) bool {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if _, ok := envelope[key]; ok {
		return true
	}
	if raw, ok := envelope["notification"]; ok {
		var notification map[string]json.RawMessage
		if err := json.Unmarshal(raw, &notification); err != nil {
			t.Fatalf("decode notification: %v", err)
		}
		if _, ok := notification[key]; ok {
			return true
		}
	}
	return false
}

// processNotificationWireWithUnknownField re-encodes a valid ProcessNotification
// with one extra, hostile key injected at either the envelope level or inside
// the nested "notification" object, mirroring pkg/event's
// processEventWireWithUnknownField helper.
func processNotificationWireWithUnknownField(
	t *testing.T,
	cmd command.ProcessNotification,
	location string,
	field string,
	value json.RawMessage,
) []byte {
	t.Helper()

	encoded, err := command.MarshalCommand(cmd)
	if err != nil {
		t.Fatalf("MarshalCommand() error = %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	switch location {
	case "envelope":
		envelope[field] = value
	case "notification":
		var notification map[string]json.RawMessage
		if err := json.Unmarshal(envelope["notification"], &notification); err != nil {
			t.Fatalf("decode notification: %v", err)
		}
		notification[field] = value
		envelope["notification"], err = json.Marshal(notification)
		if err != nil {
			t.Fatalf("encode notification: %v", err)
		}
	default:
		t.Fatalf("unknown location %q", location)
	}
	wire, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode hostile wire: %v", err)
	}
	return wire
}

// TestProcessNotificationStableCommandID proves Header.CommandID and
// Notification.CommandID always carry the SAME Tools-allocated stable id
// end to end: ValidateCommand rejects any disagreement, and a legitimate
// round trip through Marshal/UnmarshalCommand preserves the exact id
// supplied — Harness never mints or replaces it.
func TestProcessNotificationStableCommandID(t *testing.T) {
	t.Parallel()

	id, sess, loop := newID(t), newID(t), newID(t)
	notification := validProcessNotification(id, sess, loop)

	if err := command.ValidateCommand(notification); err != nil {
		t.Fatalf("ValidateCommand(matching ids) error = %v, want nil", err)
	}

	data, err := command.MarshalCommand(notification)
	if err != nil {
		t.Fatalf("MarshalCommand() error = %v", err)
	}
	decoded, err := command.UnmarshalCommand(data)
	if err != nil {
		t.Fatalf("UnmarshalCommand() error = %v\nwire: %s", err, data)
	}
	restored, ok := decoded.(command.ProcessNotification)
	if !ok {
		t.Fatalf("UnmarshalCommand() returned %T, want command.ProcessNotification", decoded)
	}
	if restored.Header.CommandID != id {
		t.Errorf("restored Header.CommandID = %v, want %v", restored.Header.CommandID, id)
	}
	if restored.Notification.CommandID != id {
		t.Errorf("restored Notification.CommandID = %v, want %v", restored.Notification.CommandID, id)
	}

	// A mismatched pair (Tools somehow supplied two different stable ids) is
	// rejected fail-secure rather than one silently winning.
	mismatched := notification
	mismatched.Notification.CommandID = newID(t)
	err = command.ValidateCommand(mismatched)
	var ve *command.CommandValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("ValidateCommand(mismatched ids) error = %T %v, want *CommandValidationError", err, err)
	}
	if ve.Field != command.FieldNotification {
		t.Errorf("CommandValidationError.Field = %q, want %q", ve.Field, command.FieldNotification)
	}
}
