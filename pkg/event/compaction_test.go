package event

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
)

func validCompactionMeasurement(seed byte) ContextMeasurement {
	return ContextMeasurement{
		Basis: ContextBasis{Revision: ContextRevision(seed), ThroughEventID: uuid.UUID{seed}},
		Model: inference.ModelKey{Provider: "provider", Model: "model"}, RequestFingerprint: [32]byte{seed},
		InputTokens: content.TokenCount(seed), InputLimit: 100, Quality: inference.CountQualityExactLocal,
	}
}

func compactionSummaryFixture() *content.UserMessage {
	return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "summary"}}}}
}

func compactionHeader(commandID uuid.UUID) Header {
	h := fullHeaderLoop()
	h.Cause = identity.Cause{CommandID: commandID}
	return h
}

func TestCompactionReasonDomains(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		reason interface{ Valid() bool }
		valid  bool
	}{
		{name: "compaction unspecified", reason: CompactionReasonUnspecified},
		{name: "compaction manual", reason: CompactionReasonManual, valid: true},
		{name: "compaction automatic", reason: CompactionReasonAutomatic, valid: true},
		{name: "compaction unknown", reason: CompactionReason(3)},
		{name: "reject unspecified", reason: CompactRejectUnspecified},
		{name: "reject control lane full", reason: CompactRejectControlLaneFull, valid: true},
		{name: "reject shutting down", reason: CompactRejectShuttingDown, valid: true},
		{name: "reject interrupted", reason: CompactRejectInterrupted, valid: true},
		{name: "reject canceled", reason: CompactRejectCanceled, valid: true},
		{name: "reject stale basis", reason: CompactRejectStaleBasis, valid: true},
		{name: "reject progress publication", reason: CompactRejectProgressPublication, valid: true},
		{name: "reject unavailable", reason: CompactRejectUnavailable, valid: true},
		{name: "reject execution failed", reason: CompactRejectExecutionFailed, valid: true},
		{name: "reject invalid summary", reason: CompactRejectInvalidSummary, valid: true},
		{name: "reject context count failed", reason: CompactRejectContextCountFailed, valid: true},
		{name: "reject summary too large", reason: CompactRejectSummaryTooLarge, valid: true},
		{name: "reject internal", reason: CompactRejectInternal, valid: true},
		{name: "reject context limit unknown", reason: CompactRejectContextLimitUnknown, valid: true},
		{name: "reject unknown", reason: CompactRejectReason(14)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.reason.Valid(); got != tt.valid {
				t.Errorf("Valid() = %v, want %v", got, tt.valid)
			}
		})
	}
}

func TestCompactionReasonWireValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		reason   CompactionReason
		want     uint8
		wantJSON string
		valid    bool
	}{
		{name: "unspecified sentinel is zero", reason: CompactionReasonUnspecified, want: 0, wantJSON: "0"},
		{name: "manual is one", reason: CompactionReasonManual, want: 1, wantJSON: "1", valid: true},
		{name: "automatic is two", reason: CompactionReasonAutomatic, want: 2, wantJSON: "2", valid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := uint8(tt.reason); got != tt.want {
				t.Errorf("numeric value = %d, want %d", got, tt.want)
			}
			if got := tt.reason.Valid(); got != tt.valid {
				t.Errorf("Valid() = %v, want %v", got, tt.valid)
			}
			raw, err := json.Marshal(CompactionStarted{Reason: tt.reason})
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("json.Unmarshal fields: %v", err)
			}
			if got := string(fields["reason"]); got != tt.wantJSON {
				t.Errorf("reason JSON = %s, want numeric %s", got, tt.wantJSON)
			}
		})
	}
}

func TestCompactRejectReasonWireValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		reason   CompactRejectReason
		want     uint8
		wantJSON string
		valid    bool
	}{
		{name: "unspecified sentinel is zero", reason: CompactRejectUnspecified, want: 0, wantJSON: "0"},
		{name: "control lane full is one", reason: CompactRejectControlLaneFull, want: 1, wantJSON: "1", valid: true},
		{name: "shutting down is two", reason: CompactRejectShuttingDown, want: 2, wantJSON: "2", valid: true},
		{name: "interrupted is three", reason: CompactRejectInterrupted, want: 3, wantJSON: "3", valid: true},
		{name: "canceled is four", reason: CompactRejectCanceled, want: 4, wantJSON: "4", valid: true},
		{name: "stale basis is five", reason: CompactRejectStaleBasis, want: 5, wantJSON: "5", valid: true},
		{name: "progress publication is six", reason: CompactRejectProgressPublication, want: 6, wantJSON: "6", valid: true},
		{name: "unavailable is seven", reason: CompactRejectUnavailable, want: 7, wantJSON: "7", valid: true},
		{name: "execution failed is eight", reason: CompactRejectExecutionFailed, want: 8, wantJSON: "8", valid: true},
		{name: "invalid summary is nine", reason: CompactRejectInvalidSummary, want: 9, wantJSON: "9", valid: true},
		{name: "context count failed is ten", reason: CompactRejectContextCountFailed, want: 10, wantJSON: "10", valid: true},
		{name: "summary too large is eleven", reason: CompactRejectSummaryTooLarge, want: 11, wantJSON: "11", valid: true},
		{name: "internal is twelve", reason: CompactRejectInternal, want: 12, wantJSON: "12", valid: true},
		{name: "context limit unknown is thirteen", reason: CompactRejectContextLimitUnknown, want: 13, wantJSON: "13", valid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := uint8(tt.reason); got != tt.want {
				t.Errorf("numeric value = %d, want %d", got, tt.want)
			}
			if got := tt.reason.Valid(); got != tt.valid {
				t.Errorf("Valid() = %v, want %v", got, tt.valid)
			}
			testCases := []struct {
				name  string
				event Event
				field string
			}{
				{name: "canonical rejection field", event: CompactionRejected{RejectReason: tt.reason}, field: "reject_reason"},
				{name: "waiter rejection field", event: CompactWaiterRejected{Reason: tt.reason}, field: "reason"},
			}
			for _, tc := range testCases {
				raw, err := json.Marshal(tc.event)
				if err != nil {
					t.Fatalf("json.Marshal %s: %v", tc.name, err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatalf("json.Unmarshal %s fields: %v", tc.name, err)
				}
				if got := string(fields[tc.field]); got != tt.wantJSON {
					t.Errorf("%s JSON = %s, want numeric %s", tc.field, got, tt.wantJSON)
				}
			}
		})
	}
}

func TestCompactionReasonRawDecodeRejectsOutOfDomain(t *testing.T) {
	t.Parallel()
	base := CompactionStarted{Header: fullHeaderLoop(), AttemptID: CompactAttemptID(uuid.UUID{0x81}), Reason: CompactionReasonManual, Basis: validCompactionMeasurement(1).Basis}
	tests := []struct {
		name string
		raw  string
	}{
		{name: "zero sentinel", raw: `{"reason":0}`},
		{name: "unknown value", raw: `{"reason":3}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			decoded := base
			if err := json.Unmarshal([]byte(tt.raw), &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			var invalid *InvalidEventError
			if err := ValidateEvent(decoded); !errors.As(err, &invalid) {
				t.Fatalf("ValidateEvent error = %T %v, want *InvalidEventError", err, err)
			}
			if invalid.Field != FieldReason {
				t.Errorf("invalid field = %q, want %q", invalid.Field, FieldReason)
			}
		})
	}
}

func TestCompactRejectReasonDurableDecodeRejectsOutOfDomain(t *testing.T) {
	t.Parallel()
	valid := CompactionRejected{
		Header:           fullHeaderLoop(),
		AttemptID:        CompactAttemptID(uuid.UUID{0x82}),
		WaiterCommandIDs: []uuid.UUID{{0x83}},
		Reason:           CompactionReasonAutomatic,
		Basis:            validCompactionMeasurement(1).Basis,
		RejectReason:     CompactRejectExecutionFailed,
	}
	wire, err := MarshalEvent(valid)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	tests := []struct {
		name string
		raw  string
	}{
		{name: "zero sentinel", raw: "0"},
		{name: "unknown value", raw: "14"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatalf("json.Unmarshal envelope: %v", err)
			}
			fields["reject_reason"] = json.RawMessage(tt.raw)
			mutated, err := json.Marshal(fields)
			if err != nil {
				t.Fatalf("json.Marshal envelope: %v", err)
			}
			got, err := UnmarshalEvent(mutated)
			if got != nil {
				t.Errorf("UnmarshalEvent event = %#v, want nil", got)
			}
			var invalid *InvalidEventError
			if !errors.As(err, &invalid) {
				t.Fatalf("UnmarshalEvent error = %T %v, want *InvalidEventError", err, err)
			}
			if invalid.Field != FieldRejectReason {
				t.Errorf("invalid field = %q, want %q", invalid.Field, FieldRejectReason)
			}
		})
	}
}

func TestCompactionEventsValidate(t *testing.T) {
	t.Parallel()
	attempt := CompactAttemptID(uuid.UUID{0x91})
	commandID := uuid.UUID{0x92}
	committedID := uuid.UUID{0x93}
	waiters := []uuid.UUID{commandID, uuid.UUID{0x94}}
	started := CompactionStarted{Header: fullHeaderLoop(), AttemptID: attempt, Reason: CompactionReasonManual, Basis: validCompactionMeasurement(1).Basis}
	committed := CompactionCommitted{Header: fullHeaderLoop(), AttemptID: attempt, WaiterCommandIDs: waiters, Reason: CompactionReasonManual, Basis: validCompactionMeasurement(1).Basis, Summary: compactionSummaryFixture(), PostContext: validCompactionMeasurement(2), Duration: 1500 * time.Millisecond}
	rejected := CompactionRejected{Header: fullHeaderLoop(), AttemptID: attempt, WaiterCommandIDs: waiters, Reason: CompactionReasonAutomatic, Basis: validCompactionMeasurement(1).Basis, RejectReason: CompactRejectExecutionFailed, Duration: time.Second}
	resolvedHeader := compactionHeader(commandID)
	resolvedHeader.EventID = CompactWaiterReplyID(attempt, commandID, true)
	resolved := CompactWaiterResolved{Header: resolvedHeader, AttemptID: attempt, CommittedEventID: committedID}
	rejectedHeader := compactionHeader(commandID)
	rejectedHeader.EventID = CompactWaiterReplyID(attempt, commandID, false)
	waiterRejected := CompactWaiterRejected{Header: rejectedHeader, AttemptID: attempt, Reason: CompactRejectCanceled}
	tests := []struct {
		name    string
		event   Event
		wantErr bool
	}{
		{name: "started", event: started},
		{name: "committed", event: committed},
		{name: "rejected", event: rejected},
		{name: "waiter resolved", event: resolved},
		{name: "waiter rejected", event: waiterRejected},
		{name: "started unspecified reason", event: func() Event { value := started; value.Reason = CompactionReasonUnspecified; return value }(), wantErr: true},
		{name: "committed unspecified reason", event: func() Event { value := committed; value.Reason = CompactionReasonUnspecified; return value }(), wantErr: true},
		{name: "committed unknown reason", event: func() Event { value := committed; value.Reason = CompactionReason(3); return value }(), wantErr: true},
		{name: "committed negative duration", event: func() Event { value := committed; value.Duration = -1; return value }(), wantErr: true},
		{name: "committed nil summary", event: func() Event { value := committed; value.Summary = nil; return value }(), wantErr: true},
		{name: "committed duplicate waiter", event: func() Event {
			value := committed
			value.WaiterCommandIDs = []uuid.UUID{commandID, commandID}
			return value
		}(), wantErr: true},
		{name: "rejected unspecified reason", event: func() Event { value := rejected; value.Reason = CompactionReasonUnspecified; return value }(), wantErr: true},
		{name: "rejected invalid basis", event: func() Event { value := rejected; value.Basis = ContextBasis{}; return value }(), wantErr: true},
		{name: "rejected unknown reason", event: func() Event { value := rejected; value.RejectReason = CompactRejectReason(14); return value }(), wantErr: true},
		{name: "resolved nondeterministic event id", event: func() Event { value := resolved; value.EventID = uuid.UUID{0xff}; return value }(), wantErr: true},
		{name: "rejected waiter missing command cause", event: func() Event { value := waiterRejected; value.Cause.CommandID = uuid.UUID{}; return value }(), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEvent(tt.event)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateEvent(%T) error = %T %v, wantErr=%v", tt.event, err, err, tt.wantErr)
			}
		})
	}
}

func TestCompactionEnduringRoundTrip(t *testing.T) {
	t.Parallel()
	attempt := CompactAttemptID(uuid.UUID{0xa1})
	commandID := uuid.UUID{0xa2}
	waiters := []uuid.UUID{commandID, uuid.UUID{0xa3}}
	resolvedHeader := compactionHeader(commandID)
	resolvedHeader.EventID = CompactWaiterReplyID(attempt, commandID, true)
	rejectedHeader := compactionHeader(commandID)
	rejectedHeader.EventID = CompactWaiterReplyID(attempt, commandID, false)
	tests := []struct {
		name  string
		event Event
	}{
		{name: "committed exact duration product reason and basis", event: CompactionCommitted{Header: fullHeaderLoop(), AttemptID: attempt, WaiterCommandIDs: waiters, Reason: CompactionReasonManual, Basis: validCompactionMeasurement(1).Basis, Summary: compactionSummaryFixture(), PostContext: validCompactionMeasurement(2), Duration: 1500000001 * time.Nanosecond}},
		{name: "rejected exact duration reason and basis", event: CompactionRejected{Header: fullHeaderLoop(), AttemptID: attempt, WaiterCommandIDs: waiters, Reason: CompactionReasonAutomatic, Basis: validCompactionMeasurement(1).Basis, RejectReason: CompactRejectInternal, Duration: 2500000001 * time.Nanosecond}},
		{name: "waiter resolved", event: CompactWaiterResolved{Header: resolvedHeader, AttemptID: attempt, CommittedEventID: uuid.UUID{0xa4}}},
		{name: "waiter rejected", event: CompactWaiterRejected{Header: rejectedHeader, AttemptID: attempt, Reason: CompactRejectContextLimitUnknown}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wire, err := MarshalEvent(tt.event)
			if err != nil {
				t.Fatal(err)
			}
			got, err := UnmarshalEvent(wire)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.event) {
				t.Errorf("round trip = %#v, want %#v", got, tt.event)
			}
		})
	}
}

func TestCompactionStartedCannotPersist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		event Event
	}{
		{name: "valid started remains ephemeral", event: CompactionStarted{Header: fullHeaderLoop(), AttemptID: CompactAttemptID(uuid.UUID{1}), Reason: CompactionReasonAutomatic, Basis: validCompactionMeasurement(1).Basis}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wire, err := MarshalEvent(tt.event)
			if wire != nil {
				t.Errorf("wire = %s, want nil", wire)
			}
			var ephemeral *EphemeralNotPersistableError
			if !errors.As(err, &ephemeral) {
				t.Fatalf("error = %T %v, want *EphemeralNotPersistableError", err, err)
			}
		})
	}
}

func TestCompactWaiterReplyID(t *testing.T) {
	t.Parallel()
	attempt := CompactAttemptID(uuid.UUID{1})
	commandID := uuid.UUID{2}
	base := CompactWaiterReplyID(attempt, commandID, true)
	tests := []struct {
		name string
		got  uuid.UUID
		same bool
	}{
		{name: "same input deterministic", got: CompactWaiterReplyID(attempt, commandID, true), same: true},
		{name: "outcome separates", got: CompactWaiterReplyID(attempt, commandID, false)},
		{name: "attempt separates", got: CompactWaiterReplyID(CompactAttemptID(uuid.UUID{3}), commandID, true)},
		{name: "command separates", got: CompactWaiterReplyID(attempt, uuid.UUID{4}, true)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if (tt.got == base) != tt.same {
				t.Errorf("id equality = %v, want %v", tt.got == base, tt.same)
			}
			if tt.got.IsZero() {
				t.Error("deterministic id is zero")
			}
		})
	}
}

func FuzzCompactionEventDecode(f *testing.F) {
	attempt := CompactAttemptID(uuid.UUID{1})
	valid := CompactionRejected{Header: fullHeaderLoop(), AttemptID: attempt, WaiterCommandIDs: []uuid.UUID{{2}}, RejectReason: CompactRejectInternal, Duration: time.Nanosecond}
	if wire, err := MarshalEvent(valid); err == nil {
		f.Add(wire)
	}
	for _, seed := range []string{
		`{"type":"CompactionRejected","v":1}`,
		`{"type":"CompactionRejected","v":1,"reject_reason":14}`,
		`{"type":"CompactionCommitted","v":1,"duration":-1}`,
		`{"type":"CompactionStarted","v":1}`,
		`{"type":"CompactWaiterResolved","v":1,"attempt_id":[]}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := UnmarshalEvent(data)
		if err != nil {
			if got != nil {
				t.Fatalf("event=%#v with error=%v", got, err)
			}
			if !isTypedDecodeError(err) {
				t.Fatalf("untyped error %T: %v", err, err)
			}
			return
		}
		if got == nil {
			t.Fatal("nil event with nil error")
		}
	})
}
