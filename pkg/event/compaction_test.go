package event

import (
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

func TestCompactionEventsValidate(t *testing.T) {
	t.Parallel()
	attempt := CompactAttemptID(uuid.UUID{0x91})
	commandID := uuid.UUID{0x92}
	committedID := uuid.UUID{0x93}
	waiters := []uuid.UUID{commandID, uuid.UUID{0x94}}
	started := CompactionStarted{Header: fullHeaderLoop(), AttemptID: attempt, Reason: CompactionReasonManual, Basis: validCompactionMeasurement(1).Basis}
	committed := CompactionCommitted{Header: fullHeaderLoop(), AttemptID: attempt, WaiterCommandIDs: waiters, Basis: validCompactionMeasurement(1).Basis, Summary: compactionSummaryFixture(), PostContext: validCompactionMeasurement(2), Duration: 1500 * time.Millisecond}
	rejected := CompactionRejected{Header: fullHeaderLoop(), AttemptID: attempt, WaiterCommandIDs: waiters, RejectReason: CompactRejectExecutionFailed, Duration: time.Second}
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
		{name: "committed negative duration", event: func() Event { value := committed; value.Duration = -1; return value }(), wantErr: true},
		{name: "committed nil summary", event: func() Event { value := committed; value.Summary = nil; return value }(), wantErr: true},
		{name: "committed duplicate waiter", event: func() Event {
			value := committed
			value.WaiterCommandIDs = []uuid.UUID{commandID, commandID}
			return value
		}(), wantErr: true},
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
		{name: "committed exact duration and product", event: CompactionCommitted{Header: fullHeaderLoop(), AttemptID: attempt, WaiterCommandIDs: waiters, Basis: validCompactionMeasurement(1).Basis, Summary: compactionSummaryFixture(), PostContext: validCompactionMeasurement(2), Duration: 1500000001 * time.Nanosecond}},
		{name: "rejected exact duration", event: CompactionRejected{Header: fullHeaderLoop(), AttemptID: attempt, WaiterCommandIDs: waiters, RejectReason: CompactRejectInternal, Duration: 2500000001 * time.Nanosecond}},
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
