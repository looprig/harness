package sessionstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
)

func replayHustleDefinition(t *testing.T) hustle.DefinitionDescriptor {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName("private.audit"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1, OutputBytes: 1}),
		hustle.WithCurrentLoopModel(),
		hustle.WithSystemPrompt("secret", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return definition.Descriptor()
}

func replayHustleStarted(t *testing.T, sid uuid.UUID) event.HustleStarted {
	t.Helper()
	return event.HustleStarted{
		Header: event.Header{
			Coordinates:     identity.Coordinates{SessionID: sid},
			EventID:         newTestUUID(t),
			EventVisibility: event.Internal,
		},
		Run: event.HustleRunDescriptor{
			Definition: replayHustleDefinition(t),
			RunID:      hustle.RunID(newTestUUID(t)),
		},
	}
}

func TestEventReplayVisibilityAndPrivilegedSeam(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	sid := newTestUUID(t)
	lease, err := store.AcquireLease(context.Background(), sid)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	j, err := store.OpenJournal(context.Background(), sid, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	publicBefore := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: newTestUUID(t)}}
	completedStart := replayHustleStarted(t, sid)
	completedRun := completedStart.Run
	completedRun.Runtime = event.ModelRuntime{Key: inference.ModelKey{Provider: "provider", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}}
	completedHeader := completedStart.Header
	completedHeader.EventID = newTestUUID(t)
	completed := event.HustleCompleted{Header: completedHeader, Run: completedRun}
	failedStart := replayHustleStarted(t, sid)
	failedHeader := failedStart.Header
	failedHeader.EventID = newTestUUID(t)
	failed := event.HustleFailed{Header: failedHeader, Run: failedStart.Run, Stage: hustle.StageQueue, ReasonCode: hustle.ReasonCanceled}
	publicAfter := event.SessionStopped{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: newTestUUID(t)}}
	for _, ev := range []event.Event{publicBefore, completedStart, completed, failedStart, failed, publicAfter} {
		if _, err := j.Append(context.Background(), journal.NewEventRecord(ev)); err != nil {
			t.Fatalf("Append(%T) error = %v", ev, err)
		}
	}

	tests := []struct {
		name     string
		open     func() (journal.EventReplayer, error)
		wantType []string
		wantSeq  []uint64
	}{
		{
			name: "public skips internal and preserves sequence gap",
			open: func() (journal.EventReplayer, error) {
				return store.OpenEventReplayer(sid, ReplayRequest{FromSeq: 1})
			},
			wantType: []string{"SessionStarted", "SessionStopped"},
			wantSeq:  []uint64{2, 7},
		},
		{
			name: "privileged retains internal",
			open: func() (journal.EventReplayer, error) {
				return store.OpenInternalEventReplayer(sid, ReplayRequest{FromSeq: 1})
			},
			wantType: []string{"SessionStarted", "HustleStarted", "HustleCompleted", "HustleStarted", "HustleFailed", "SessionStopped"},
			wantSeq:  []uint64{2, 3, 4, 5, 6, 7},
		},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			replayer, openErr := testCase.open()
			if openErr != nil {
				t.Fatalf("open() error = %v", openErr)
			}
			events, seqs := drainEvents(t, replayer, journal.ReplayRequest{})
			if len(events) != len(testCase.wantType) {
				t.Fatalf("events = %d, want %d", len(events), len(testCase.wantType))
			}
			for index, ev := range events {
				name := eventTypeName(ev)
				if name != testCase.wantType[index] || seqs[index] != testCase.wantSeq[index] {
					t.Fatalf("event[%d] = %s@%d, want %s@%d", index, name, seqs[index], testCase.wantType[index], testCase.wantSeq[index])
				}
			}
		})
	}

	recordReplayer, err := store.OpenRecordReplayer(sid, ReplayRequest{FromSeq: 1})
	if err != nil {
		t.Fatalf("OpenRecordReplayer() error = %v", err)
	}
	records, _ := drainRecords(t, recordReplayer, journal.ReplayRequest{})
	if len(records) != 7 {
		t.Fatalf("raw records = %d, want fence plus all six events", len(records))
	}
}

func eventTypeName(ev event.Event) string {
	switch ev.(type) {
	case event.SessionStarted:
		return "SessionStarted"
	case event.HustleStarted:
		return "HustleStarted"
	case event.HustleCompleted:
		return "HustleCompleted"
	case event.HustleFailed:
		return "HustleFailed"
	case event.SessionStopped:
		return "SessionStopped"
	default:
		return "unknown"
	}
}

func TestEventReplayLoopNarrowingPreservesVisibility(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	sid, loopA, loopB := newTestUUID(t), newTestUUID(t), newTestUUID(t)
	lease, err := store.AcquireLease(context.Background(), sid)
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	j, err := store.OpenJournal(context.Background(), sid, lease)
	if err != nil {
		t.Fatalf("OpenJournal() error = %v", err)
	}
	events := []event.Event{
		event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: newTestUUID(t)}},
		event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopA}, EventID: newTestUUID(t)}},
		replayHustleStarted(t, sid),
		event.LoopIdle{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loopB}, EventID: newTestUUID(t)}},
		event.SessionStopped{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: newTestUUID(t)}},
	}
	for _, ev := range events {
		if _, err := j.Append(context.Background(), journal.NewEventRecord(ev)); err != nil {
			t.Fatalf("Append(%T) error = %v", ev, err)
		}
	}
	tests := []struct {
		name    string
		open    func() (journal.EventReplayer, error)
		wantSeq []uint64
	}{
		{name: "public loop narrow", open: func() (journal.EventReplayer, error) { return store.OpenEventReplayer(sid, ReplayRequest{FromSeq: 1}) }, wantSeq: []uint64{2, 3, 6}},
		{name: "privileged loop narrow", open: func() (journal.EventReplayer, error) {
			return store.OpenInternalEventReplayer(sid, ReplayRequest{FromSeq: 1})
		}, wantSeq: []uint64{2, 3, 4, 6}},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			replayer, openErr := testCase.open()
			if openErr != nil {
				t.Fatalf("open() error = %v", openErr)
			}
			_, seqs := drainEvents(t, replayer, journal.ReplayRequest{LoopID: loopA})
			if len(seqs) != len(testCase.wantSeq) {
				t.Fatalf("seqs = %v, want %v", seqs, testCase.wantSeq)
			}
			for index := range seqs {
				if seqs[index] != testCase.wantSeq[index] {
					t.Fatalf("seqs = %v, want %v", seqs, testCase.wantSeq)
				}
			}
		})
	}
}

func TestEventReplayRejectsUnknownVisibility(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	sid := newTestUUID(t)
	body := []byte(`{"type":"SessionStarted","v":1,"session_id":"11111111-1111-1111-1111-111111111111","event_id":"12121212-1212-1212-1212-121212121212","visibility":99}`)
	frame, err := encodeEnvelope(envelope{V: envelopeVersion, Kind: string(kindEvent), ID: "unknown-visibility", Body: body})
	if err != nil {
		t.Fatalf("encodeEnvelope() error = %v", err)
	}
	if err := store.backend.Ledger.Append(context.Background(), ledgerName(sid), 0, frame); err != nil {
		t.Fatalf("Ledger.Append() error = %v", err)
	}
	tests := []struct {
		name string
		open func() (journal.EventReplayer, error)
	}{
		{name: "public", open: func() (journal.EventReplayer, error) { return store.OpenEventReplayer(sid, ReplayRequest{FromSeq: 1}) }},
		{name: "privileged", open: func() (journal.EventReplayer, error) {
			return store.OpenInternalEventReplayer(sid, ReplayRequest{FromSeq: 1})
		}},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			replayer, openErr := testCase.open()
			if openErr != nil {
				t.Fatalf("open() error = %v", openErr)
			}
			cursor, openErr := replayer.Open(context.Background(), journal.ReplayRequest{})
			if openErr != nil {
				t.Fatalf("Open() error = %v", openErr)
			}
			defer func() { _ = cursor.Close() }()
			_, _, nextErr := cursor.Next(context.Background())
			var replayErr *ReplayDecodeError
			var invalid *event.InvalidEventError
			if !errors.As(nextErr, &replayErr) || !errors.As(nextErr, &invalid) || invalid.Field != event.FieldVisibility {
				t.Fatalf("Next() error = %T %v, want replay-wrapped visibility InvalidEventError", nextErr, nextErr)
			}
		})
	}
}

func TestJournalRejectsInvalidEventVisibility(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		visibility event.EventVisibility
		wantErr    bool
		wantTip    uint64
	}{
		{name: "public appends", visibility: event.Public, wantTip: 2},
		{name: "internal appends", visibility: event.Internal, wantTip: 2},
		{name: "unknown cannot poison journal", visibility: event.EventVisibility(99), wantErr: true, wantTip: 1},
	}
	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			store, err := Open(memstore.New())
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			sid := newTestUUID(t)
			lease, err := store.AcquireLease(context.Background(), sid)
			if err != nil {
				t.Fatalf("AcquireLease() error = %v", err)
			}
			journalStore, err := store.OpenJournal(context.Background(), sid, lease)
			if err != nil {
				t.Fatalf("OpenJournal() error = %v", err)
			}
			ev := event.SessionStarted{Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sid}, EventID: newTestUUID(t), EventVisibility: testCase.visibility,
			}}
			_, appendErr := journalStore.Append(context.Background(), journal.NewEventRecord(ev))
			if (appendErr != nil) != testCase.wantErr {
				t.Fatalf("Append() error = %v, wantErr %v", appendErr, testCase.wantErr)
			}
			if testCase.wantErr {
				var invalid *event.InvalidEventError
				if !errors.As(appendErr, &invalid) || invalid.Field != event.FieldVisibility {
					t.Fatalf("Append() error = %T %v, want visibility InvalidEventError", appendErr, appendErr)
				}
			}
			tip, err := store.backend.Ledger.Tip(context.Background(), ledgerName(sid))
			if err != nil || tip != testCase.wantTip {
				t.Fatalf("Tip() = (%d,%v), want (%d,nil)", tip, err, testCase.wantTip)
			}
		})
	}
}
