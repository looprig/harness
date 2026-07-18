package journal

import (
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

// fixedUUID builds a deterministic non-zero uuid from a single seed byte so the table
// tests round-trip stable, readable ids.
func fixedUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

// TestEventRecordIDAndPayload proves an EventRecord carries the event's EventID as its
// idempotency id and returns the wrapped event unchanged for the serializer to marshal.
func TestEventRecordIDAndPayload(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11)
	lid := fixedUUID(0x12)
	evID := fixedUUID(0x13)

	tests := []struct {
		name   string
		ev     event.Event
		wantID string
	}{
		{
			name:   "session-scoped event",
			ev:     event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: evID}},
			wantID: evID.String(),
		},
		{
			name:   "loop-scoped event",
			ev:     event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid}, EventID: evID}},
			wantID: evID.String(),
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := NewEventRecord(tt.ev)
			if got := rec.IdempotencyID(); got != tt.wantID {
				t.Errorf("IdempotencyID() = %q, want %q", got, tt.wantID)
			}
			// The wrapped event is recoverable for the serializer's MarshalEvent.
			if !reflect.DeepEqual(rec.Event(), tt.ev) {
				t.Errorf("Event() did not return the wrapped event")
			}
			var _ JournalRecord = rec // EventRecord satisfies the sealed sum.
		})
	}
}

// TestCommandRecordIDAndTarget proves a CommandRecord carries the command's CommandID as
// its idempotency id, exposes the writer-supplied dispatch target (session + loop), and
// returns the wrapped command unchanged.
func TestCommandRecordIDAndTarget(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x21)
	lid := fixedUUID(0x22)
	cmdID := fixedUUID(0x23)

	cmd := command.Interrupt{Header: command.Header{CommandID: cmdID}}
	rec := NewCommandRecord(sid, lid, cmd)
	if got := rec.IdempotencyID(); got != cmdID.String() {
		t.Errorf("IdempotencyID() = %q, want %q", got, cmdID.String())
	}
	if rec.SessionID() != sid {
		t.Errorf("SessionID() = %v, want %v", rec.SessionID(), sid)
	}
	if rec.LoopID() != lid {
		t.Errorf("LoopID() = %v, want %v", rec.LoopID(), lid)
	}
	if rec.Command() != cmd {
		t.Errorf("Command() did not return the wrapped command")
	}
	var _ JournalRecord = rec // CommandRecord satisfies the sealed sum.
}

func TestValidateCommandRecordRoute(t *testing.T) {
	t.Parallel()
	target, other := fixedUUID(0x61), fixedUUID(0x62)
	cmd := command.UserInput{Header: command.Header{CommandID: fixedUUID(0x63), Agency: identity.AgencyMachine}, NoFold: true, TargetLoopID: target}
	for _, tt := range []struct {
		name    string
		loopID  uuid.UUID
		wantErr bool
	}{
		{name: "matching live route", loopID: target},
		{name: "zero replay route unavailable", loopID: uuid.UUID{}},
		{name: "mismatched route", loopID: other, wantErr: true},
	} {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			record := NewCommandRecord(fixedUUID(0x64), tt.loopID, cmd)
			err := ValidateCommandRecordRoute(record)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ValidateCommandRecordRoute: %v", err)
				}
				return
			}
			var mismatch *CommandRouteMismatchError
			if !errors.As(err, &mismatch) || mismatch.RecordLoopID != other || mismatch.TargetLoopID != target {
				t.Fatalf("error = %T %+v, want typed route mismatch", err, err)
			}
		})
	}
}

// TestJournalRecordSumIsSealed asserts each record variant satisfies the sealed
// JournalRecord marker (so a serializer's switch over the sum stays exhaustive) and
// exposes a non-empty idempotency id.
func TestJournalRecordSumIsSealed(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x41)
	lid := fixedUUID(0x42)
	recs := []JournalRecord{
		NewEventRecord(event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}}}),
		NewCommandRecord(sid, lid, command.Interrupt{}),
		NewFenceRecord(sid, LeaseFence{Epoch: 7}),
		NewGatePreparedRecord(
			event.GatePrepared{
				Header: event.Header{
					Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: fixedUUID(0x33), StepID: fixedUUID(0x44)},
					EventID:     fixedUUID(0x55),
				},
				Gate: gate.Gate{ID: gate.ID(fixedUUID(0x70)), Kind: gate.KindPermission},
			},
			gate.OpenPayload{GateID: gate.ID(fixedUUID(0x70)), Payload: gate.PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}}},
		),
	}
	for i, r := range recs {
		if r == nil {
			t.Fatalf("record %d is nil", i)
		}
		if r.IdempotencyID() == "" {
			t.Errorf("record %d IdempotencyID() is empty", i)
		}
	}
}

// TestGatePreparedRecordIDAndPayload proves a GatePreparedRecord carries the
// prepared event's EventID as its idempotency id and returns the wrapped prepared
// projection and private payload unchanged.
func TestGatePreparedRecordIDAndPayload(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11)
	lid := fixedUUID(0x12)
	evID := fixedUUID(0x55)
	prepared := event.GatePrepared{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: fixedUUID(0x33), StepID: fixedUUID(0x44)},
			EventID:     evID,
		},
		Gate: gate.Gate{ID: gate.ID(fixedUUID(0x70)), Kind: gate.KindPermission},
	}
	payload := gate.OpenPayload{
		GateID:  gate.ID(fixedUUID(0x70)),
		Payload: gate.PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}},
	}
	rec := NewGatePreparedRecord(prepared, payload)
	if got := rec.IdempotencyID(); got != evID.String() {
		t.Errorf("IdempotencyID() = %q, want %q", got, evID.String())
	}
	if !reflect.DeepEqual(rec.Prepared(), prepared) {
		t.Errorf("Prepared() = %#v, want %#v", rec.Prepared(), prepared)
	}
	if !reflect.DeepEqual(rec.Payload(), payload) {
		t.Errorf("Payload() = %#v, want %#v", rec.Payload(), payload)
	}
	var _ JournalRecord = rec
}

// TestGatePreparedRecordCodecRoundTrip proves MarshalGatePreparedRecord and
// UnmarshalGatePreparedRecord are inverses: the reconstructed record carries the
// same prepared projection and typed payload.
func TestGatePreparedRecordCodecRoundTrip(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11)
	lid := fixedUUID(0x12)
	prepared := event.GatePrepared{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: fixedUUID(0x33), StepID: fixedUUID(0x44)},
			EventID:     fixedUUID(0x55),
		},
		Gate: gate.Gate{
			ID:       gate.ID(fixedUUID(0x70)),
			Kind:     gate.KindPermission,
			Resolver: gate.ResolverLoop,
			Blocks:   gate.BlocksToolCall,
			Effect:   gate.EffectResume,
			Prompt:   gate.Prompt{Title: "Approve", Body: "echo ok"},
		},
	}
	tests := []struct {
		name    string
		payload gate.OpenPayload
	}{
		{
			name: "permission payload",
			payload: gate.OpenPayload{
				GateID:  gate.ID(fixedUUID(0x70)),
				Payload: gate.PermissionPayload{Request: tool.BashRequest{Command: "echo ok"}},
			},
		},
		{
			name: "ask-user payload",
			payload: gate.OpenPayload{
				GateID:  gate.ID(fixedUUID(0x70)),
				Payload: gate.AskUserPayload{Question: "continue?", Choices: []string{"yes", "no"}},
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := NewGatePreparedRecord(prepared, tt.payload)
			data, err := MarshalGatePreparedRecord(rec)
			if err != nil {
				t.Fatalf("MarshalGatePreparedRecord() error = %v", err)
			}
			got, err := UnmarshalGatePreparedRecord(data)
			if err != nil {
				t.Fatalf("UnmarshalGatePreparedRecord() error = %v", err)
			}
			if !reflect.DeepEqual(got.Prepared(), prepared) {
				t.Errorf("Prepared() = %#v, want %#v", got.Prepared(), prepared)
			}
			if !reflect.DeepEqual(got.Payload(), tt.payload) {
				t.Errorf("Payload() = %#v, want %#v", got.Payload(), tt.payload)
			}
		})
	}
}

// TestGatePreparedRecordCodecMalformedFailsClosed proves the codec fails closed
// with a typed *GatePreparedDecodeError on malformed input.
func TestGatePreparedRecordCodecMalformedFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
	}{
		{name: "not json", data: `not json`},
		{name: "truncated", data: `{"prepared":`},
		{name: "wrong wrapper shape", data: `[]`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := UnmarshalGatePreparedRecord([]byte(tt.data))
			var decode *GatePreparedDecodeError
			if !errors.As(err, &decode) {
				t.Fatalf("UnmarshalGatePreparedRecord(%q) error = %v, want *GatePreparedDecodeError", tt.data, err)
			}
		})
	}
}
