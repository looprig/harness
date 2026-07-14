package sessionruntime

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/inference"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

func restoredCompactionSummary(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

func restoredCommitted(seed byte, waiters ...uuid.UUID) event.CompactionCommitted {
	measurement := foldContextMeasurement(seed + 1)
	return event.CompactionCommitted{
		Header: event.Header{
			EventID: uuid.UUID{seed},
			Coordinates: identity.Coordinates{
				SessionID: uuid.UUID{0xa1}, LoopID: uuid.UUID{0xb1},
			},
		},
		AttemptID: event.CompactAttemptID(uuid.UUID{seed}), WaiterCommandIDs: waiters,
		Reason: event.CompactionReasonManual, Basis: foldContextMeasurement(seed).Basis,
		Summary: restoredCompactionSummary("summary"), PostContext: measurement,
	}
}

func restoredRejected(seed byte, waiters ...uuid.UUID) event.CompactionRejected {
	return event.CompactionRejected{
		Header: event.Header{
			EventID: uuid.UUID{seed},
			Coordinates: identity.Coordinates{
				SessionID: uuid.UUID{0xa1}, LoopID: uuid.UUID{0xb1},
			},
		},
		AttemptID: event.CompactAttemptID(uuid.UUID{seed}), WaiterCommandIDs: waiters,
		Reason: event.CompactionReasonAutomatic, Basis: foldContextMeasurement(seed).Basis,
		RejectReason: event.CompactRejectExecutionFailed,
	}
}

func TestFoldLoopComposesCommittedContextResets(t *testing.T) {
	t.Parallel()
	first := restoredCommitted(10)
	first.Summary = restoredCompactionSummary("first summary")
	second := restoredCommitted(20)
	second.Summary = restoredCompactionSummary("second summary")
	laterUser := foldUserMsg("after second compaction")
	laterAI := aiMessage("later answer")
	tests := []struct {
		name   string
		events []event.Event
		want   content.AgenticMessages
		basis  event.ContextBasis
	}{
		{
			name: "latest committed summary replaces earlier raw conversation",
			events: []event.Event{
				event.TurnStarted{Message: foldUserMsg("raw old user")},
				foldStepGroup(aiMessage("raw old answer")), first,
			},
			want: content.AgenticMessages{first.Summary}, basis: first.PostContext.Basis,
		},
		{
			name: "later committed events fold after summary",
			events: []event.Event{
				first,
				event.TurnStarted{Header: event.Header{EventID: uuid.UUID{0x31}}, Message: laterUser},
				event.StepDone{Header: event.Header{EventID: uuid.UUID{0x32}}, Messages: content.AgenticMessages{laterAI}},
			},
			want:  content.AgenticMessages{first.Summary, laterUser, laterAI},
			basis: event.ContextBasis{Revision: first.PostContext.Basis.Revision + 2, ThroughEventID: uuid.UUID{0x32}},
		},
		{
			name: "multiple committed resets compose",
			events: []event.Event{first, event.TurnStarted{
				Header: event.Header{EventID: uuid.UUID{0x33}}, Message: foldUserMsg("discarded later user"),
			}, second},
			want: content.AgenticMessages{second.Summary}, basis: second.PostContext.Basis,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := foldLoop(tt.events)
			if got.Err != nil {
				t.Fatal(got.Err)
			}
			if !reflect.DeepEqual(got.Msgs, tt.want) {
				t.Fatalf("messages = %#v, want %#v", got.Msgs, tt.want)
			}
			if !got.HasBasis || got.Basis != tt.basis {
				t.Fatalf("basis = %+v has=%v, want %+v true", got.Basis, got.HasBasis, tt.basis)
			}
		})
	}
}

func TestPlanCompactWaiterRepairs(t *testing.T) {
	t.Parallel()
	w1, w2 := uuid.UUID{0xc1}, uuid.UUID{0xc2}
	committed := restoredCommitted(10, w1, w2)
	rejected := restoredRejected(20, w1)
	matchingResolved := event.CompactWaiterResolved{
		Header: event.Header{
			EventID:     event.CompactWaiterReplyID(committed.AttemptID, w1, true),
			Coordinates: committed.Coordinates, Cause: identity.Cause{CommandID: w1},
		},
		AttemptID: committed.AttemptID, CommittedEventID: committed.EventID,
	}
	matchingRejected := event.CompactWaiterRejected{
		Header: event.Header{
			EventID:     event.CompactWaiterReplyID(rejected.AttemptID, w1, false),
			Coordinates: rejected.Coordinates, Cause: identity.Cause{CommandID: w1},
		},
		AttemptID: rejected.AttemptID, Reason: rejected.RejectReason,
	}
	nonMemberCommand := uuid.UUID{0xc3}
	nonMemberResolved := event.CompactWaiterResolved{
		Header: event.Header{
			EventID:     event.CompactWaiterReplyID(committed.AttemptID, nonMemberCommand, true),
			Coordinates: committed.Coordinates, Cause: identity.Cause{CommandID: nonMemberCommand},
		},
		AttemptID: committed.AttemptID, CommittedEventID: committed.EventID,
	}
	nonMemberRejected := event.CompactWaiterRejected{
		Header: event.Header{
			EventID:     event.CompactWaiterReplyID(committed.AttemptID, nonMemberCommand, false),
			Coordinates: committed.Coordinates, Cause: identity.Cause{CommandID: nonMemberCommand},
		},
		AttemptID: committed.AttemptID, Reason: event.CompactRejectCanceled,
	}
	laneFull := nonMemberRejected
	laneFull.Reason = event.CompactRejectControlLaneFull
	orphanAttempt := event.CompactAttemptID(uuid.UUID{0xd1})
	orphanResolved := event.CompactWaiterResolved{
		Header: event.Header{
			EventID:     event.CompactWaiterReplyID(orphanAttempt, uuid.UUID{0xd2}, true),
			Coordinates: committed.Coordinates, Cause: identity.Cause{CommandID: uuid.UUID{0xd2}},
		},
		AttemptID: orphanAttempt, CommittedEventID: uuid.UUID{0xd3},
	}
	orphanRejected := func(commandID uuid.UUID, reason event.CompactRejectReason) event.CompactWaiterRejected {
		return event.CompactWaiterRejected{
			Header: event.Header{
				EventID:     event.CompactWaiterReplyID(orphanAttempt, commandID, false),
				Coordinates: committed.Coordinates, Cause: identity.Cause{CommandID: commandID},
			},
			AttemptID: orphanAttempt, Reason: reason,
		}
	}
	tests := []struct {
		name     string
		events   []event.Event
		want     []event.Event
		wantKind restoredCompactionErrorKind
	}{
		{
			name:   "committed terminal repairs only missing members",
			events: []event.Event{committed, matchingResolved},
			want: []event.Event{event.CompactWaiterResolved{
				Header: event.Header{
					EventID:     event.CompactWaiterReplyID(committed.AttemptID, w2, true),
					Coordinates: committed.Coordinates, Cause: identity.Cause{CommandID: w2},
				},
				AttemptID: committed.AttemptID, CommittedEventID: committed.EventID,
			}},
		},
		{name: "matching rejected outcome suppresses repair", events: []event.Event{rejected, matchingRejected}},
		{
			name:   "rejected terminal repairs missing member",
			events: []event.Event{rejected},
			want: []event.Event{event.CompactWaiterRejected{
				Header: event.Header{
					EventID:     event.CompactWaiterReplyID(rejected.AttemptID, w1, false),
					Coordinates: rejected.Coordinates, Cause: identity.Cause{CommandID: w1},
				},
				AttemptID: rejected.AttemptID, Reason: rejected.RejectReason,
			}},
		},
		{
			name: "opposite waiter outcome is corrupt",
			events: []event.Event{committed, event.CompactWaiterRejected{
				Header: event.Header{
					EventID:     event.CompactWaiterReplyID(committed.AttemptID, w1, false),
					Coordinates: committed.Coordinates, Cause: identity.Cause{CommandID: w1},
				},
				AttemptID: committed.AttemptID, Reason: event.CompactRejectExecutionFailed,
			}},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name: "mismatched committed event id is corrupt",
			events: []event.Event{committed, func() event.Event {
				value := matchingResolved
				value.CommittedEventID = uuid.UUID{0xff}
				return value
			}()},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name:     "resolved outcome for non-member is corrupt",
			events:   []event.Event{committed, nonMemberResolved},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name:     "non-lane-full rejection for non-member is corrupt",
			events:   []event.Event{committed, nonMemberRejected},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name: "lane-full rejection for overflow non-member is valid",
			events: []event.Event{func() event.Event {
				value := committed
				value.WaiterCommandIDs = []uuid.UUID{w1}
				return value
			}(), matchingResolved, laneFull},
		},
		{
			name: "lane-full rejection with foreign coordinates is corrupt",
			events: []event.Event{committed, func() event.Event {
				value := laneFull
				value.Coordinates.LoopID = uuid.UUID{0xee}
				return value
			}()},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{name: "orphan interrupted rejection is valid", events: []event.Event{
			orphanRejected(uuid.UUID{0xd4}, event.CompactRejectInterrupted),
		}},
		{name: "orphan shutting-down rejection is valid", events: []event.Event{
			orphanRejected(uuid.UUID{0xd5}, event.CompactRejectShuttingDown),
		}},
		{name: "orphan lane-full rejection is valid", events: []event.Event{
			orphanRejected(uuid.UUID{0xd6}, event.CompactRejectControlLaneFull),
		}},
		{name: "orphan lane-full and pre-start rejections on distinct commands are valid", events: []event.Event{
			orphanRejected(uuid.UUID{0xd7}, event.CompactRejectControlLaneFull),
			orphanRejected(uuid.UUID{0xd8}, event.CompactRejectInterrupted),
		}},
		{name: "multiple orphan lane-full rejections on distinct commands are valid", events: []event.Event{
			orphanRejected(uuid.UUID{0xda}, event.CompactRejectControlLaneFull),
			orphanRejected(uuid.UUID{0xdb}, event.CompactRejectControlLaneFull),
		}},
		{name: "multiple orphan interrupted rejections are one valid disposition", events: []event.Event{
			orphanRejected(uuid.UUID{0xdc}, event.CompactRejectInterrupted),
			orphanRejected(uuid.UUID{0xdd}, event.CompactRejectInterrupted),
		}},
		{name: "multiple orphan shutting-down rejections are one valid disposition", events: []event.Event{
			orphanRejected(uuid.UUID{0xde}, event.CompactRejectShuttingDown),
			orphanRejected(uuid.UUID{0xdf}, event.CompactRejectShuttingDown),
		}},
		{name: "orphan lane-full may coexist with shutting-down disposition", events: []event.Event{
			orphanRejected(uuid.UUID{0xe0}, event.CompactRejectControlLaneFull),
			orphanRejected(uuid.UUID{0xe1}, event.CompactRejectShuttingDown),
		}},
		{
			name: "orphan attempt spanning loop ownership is corrupt",
			events: []event.Event{
				orphanRejected(uuid.UUID{0xe2}, event.CompactRejectControlLaneFull),
				func() event.Event {
					value := orphanRejected(uuid.UUID{0xe3}, event.CompactRejectInterrupted)
					value.Coordinates.LoopID = uuid.UUID{0xf1}
					return value
				}(),
			},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name: "orphan attempt spanning session ownership is corrupt",
			events: []event.Event{
				orphanRejected(uuid.UUID{0xe4}, event.CompactRejectInterrupted),
				func() event.Event {
					value := orphanRejected(uuid.UUID{0xe5}, event.CompactRejectInterrupted)
					value.Coordinates.SessionID = uuid.UUID{0xf2}
					return value
				}(),
			},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name: "orphan attempt with interrupted and shutting-down dispositions is corrupt",
			events: []event.Event{
				orphanRejected(uuid.UUID{0xe6}, event.CompactRejectInterrupted),
				orphanRejected(uuid.UUID{0xe7}, event.CompactRejectShuttingDown),
			},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name:     "orphan resolved outcome is corrupt",
			events:   []event.Event{orphanResolved},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name: "orphan resolved and rejected ownership for one command is corrupt",
			events: []event.Event{
				orphanResolved,
				orphanRejected(orphanResolved.Cause.CommandID, event.CompactRejectInterrupted),
			},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name:     "orphan rejection with terminal-only reason is corrupt",
			events:   []event.Event{orphanRejected(uuid.UUID{0xd9}, event.CompactRejectExecutionFailed)},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name: "member resolved outcome with foreign coordinates is corrupt",
			events: []event.Event{committed, func() event.Event {
				value := matchingResolved
				value.Coordinates.LoopID = uuid.UUID{0xee}
				return value
			}()},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name: "member rejected outcome with contradictory reason is corrupt",
			events: []event.Event{rejected, func() event.Event {
				value := matchingRejected
				value.Reason = event.CompactRejectCanceled
				return value
			}()},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name: "resolved outcome contradicts rejected terminal type",
			events: []event.Event{rejected, event.CompactWaiterResolved{
				Header: event.Header{
					EventID:     event.CompactWaiterReplyID(rejected.AttemptID, w1, true),
					Coordinates: rejected.Coordinates, Cause: identity.Cause{CommandID: w1},
				},
				AttemptID: rejected.AttemptID, CommittedEventID: uuid.UUID{0xef},
			}},
			wantKind: restoredCompactionWaiterMismatch,
		},
		{
			name:     "duplicate terminal attempt is corrupt",
			events:   []event.Event{committed, committed},
			wantKind: restoredCompactionDuplicateTerminal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, ev := range tt.events {
				if err := event.ValidateEvent(ev); err != nil {
					t.Fatalf("test fixture %T is not structurally valid: %v", ev, err)
				}
			}
			before := append([]event.Event(nil), tt.events...)
			got, err := planCompactWaiterRepairs(tt.events)
			var repairErr *restoredCompactionError
			if errors.As(err, &repairErr) != (tt.wantKind != "") {
				t.Fatalf("error = %T %v, want kind %q", err, err, tt.wantKind)
			}
			if repairErr != nil && repairErr.Kind != tt.wantKind {
				t.Fatalf("error kind = %q, want %q", repairErr.Kind, tt.wantKind)
			}
			if err == nil && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("repairs = %#v, want %#v", got, tt.want)
			}
			if !reflect.DeepEqual(tt.events, before) {
				t.Fatal("repair planning mutated the raw replay")
			}
		})
	}
}

func TestRestoreRejectsCorruptOrphanCompactWaiterBeforeStart(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "orphan resolved outcome fails replay before restore lifecycle starts"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runRestoreRejectsCorruptOrphanCompactWaiterBeforeStart(t)
		})
	}
}

func runRestoreRejectsCorruptOrphanCompactWaiterBeforeStart(t *testing.T) {
	t.Helper()
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	h, sessionID, loopID, lease, _ := newOriginalHub(t, store, fingerprintFromDefinition(definition))
	attemptID := event.CompactAttemptID(uuid.UUID{0xe1})
	commandID := uuid.UUID{0xe2}
	orphan := event.CompactWaiterResolved{
		Header: event.Header{
			EventID: event.CompactWaiterReplyID(attemptID, commandID, true), CreatedAt: fixedClock(),
			Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID},
			Cause:       identity.Cause{CommandID: commandID},
		},
		AttemptID: attemptID, CommittedEventID: uuid.UUID{0xe3},
	}
	if err := event.ValidateEvent(orphan); err != nil {
		t.Fatalf("orphan fixture is not structurally valid: %v", err)
	}
	if err := h.PublishEvent(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}
	handOver(t, lease)

	restored, err := restoreTestSession(context.Background(), definition, sessionID, store)
	if restored != nil {
		t.Fatal("corrupt replay returned a live session")
	}
	var restoreErr *RestoreError
	var compactionErr *restoredCompactionError
	if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreReplayFailed ||
		!errors.As(err, &compactionErr) || compactionErr.Kind != restoredCompactionWaiterMismatch {
		t.Fatalf("error = %T %v, want RestoreReplayFailed wrapping waiter mismatch", err, err)
	}
	var sawStarted, sawDone, sawErrored bool
	waiterCount := 0
	for _, ev := range replayAllSessionEvents(t, store, sessionID) {
		switch ev.(type) {
		case event.RestoreStarted:
			sawStarted = true
		case event.RestoreDone:
			sawDone = true
		case event.RestoreErrored:
			sawErrored = true
		case event.CompactWaiterResolved, event.CompactWaiterRejected:
			waiterCount++
		}
	}
	if sawStarted || sawDone || !sawErrored || waiterCount != 1 {
		t.Fatalf("restore events started=%v done=%v errored=%v waiterCount=%d, want false/false/true/1", sawStarted, sawDone, sawErrored, waiterCount)
	}
}

type compactionRepairJournal struct {
	records []journal.JournalRecord
	err     error
}

func (j *compactionRepairJournal) Append(_ context.Context, record journal.JournalRecord) (uint64, error) {
	if j.err != nil {
		return 0, j.err
	}
	j.records = append(j.records, record)
	return uint64(len(j.records)), nil
}

func TestAppendCompactWaiterRepairs(t *testing.T) {
	t.Parallel()
	commandID := uuid.UUID{0xd1}
	committed := restoredCommitted(30, commandID)
	repair := event.CompactWaiterResolved{
		Header: event.Header{
			EventID:     event.CompactWaiterReplyID(committed.AttemptID, commandID, true),
			Coordinates: committed.Coordinates, Cause: identity.Cause{CommandID: commandID},
		},
		AttemptID: committed.AttemptID, CommittedEventID: committed.EventID,
	}
	wantTime := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	sentinel := errors.New("append failed")
	tests := []struct {
		name    string
		journal *compactionRepairJournal
		wantErr bool
	}{
		{name: "stamps time and appends deterministic reply", journal: &compactionRepairJournal{}},
		{name: "append failure is returned", journal: &compactionRepairJournal{err: sentinel}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			factory := event.NewFactory(func() (uuid.UUID, error) {
				t.Fatal("repair stamping must not mint a replacement event id")
				return uuid.UUID{}, nil
			}, func() time.Time { return wantTime })
			err := appendCompactWaiterRepairs(context.Background(), tt.journal, factory, []event.Event{repair})
			if errors.Is(err, sentinel) != tt.wantErr {
				t.Fatalf("error = %v, want append failure=%v", err, tt.wantErr)
			}
			if tt.wantErr {
				if len(tt.journal.records) != 0 {
					t.Fatalf("failed append retained %d records", len(tt.journal.records))
				}
				return
			}
			if len(tt.journal.records) != 1 {
				t.Fatalf("appended records = %d, want 1", len(tt.journal.records))
			}
			got := tt.journal.records[0].(journal.EventRecord).Event().(event.CompactWaiterResolved)
			if got.EventID != repair.EventID || got.CreatedAt != wantTime || got.Coordinates != repair.Coordinates || got.Cause != repair.Cause {
				t.Fatalf("appended header = %+v, want deterministic id/coords/cause and time %v", got.Header, wantTime)
			}
		})
	}
}

func replayAllSessionEvents(t *testing.T, store *sessionstore.Store, sessionID uuid.UUID) []event.Event {
	t.Helper()
	replayer, err := store.OpenEventReplayer(sessionID, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{Follow: false})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cursor.Close() }()
	var events []event.Event
	for {
		ev, _, nextErr := cursor.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			return events
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		events = append(events, ev)
	}
}

func TestRestoreRepairsMissingCompactWaiterExactlyOnce(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "missing terminal member repairs once across repeated restore"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runRestoreRepairsMissingCompactWaiterExactlyOnce(t)
		})
	}
}

func runRestoreRepairsMissingCompactWaiterExactlyOnce(t *testing.T) {
	t.Helper()
	store := newRestoreStore(t)
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	fingerprint := fingerprintFromDefinition(definition)
	h, sessionID, loopID, lease, stamper := newOriginalHub(t, store, fingerprint)
	ctx := context.Background()
	turnID, stepID := uuid.UUID{0x41}, uuid.UUID{0x42}
	turnCoordinates := identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID}
	stamper.stamp(t, ctx, h, event.TurnStarted{Header: event.Header{Coordinates: turnCoordinates}, TurnIndex: 1, Message: foldUserMsg("raw user")})
	stamper.stamp(t, ctx, h, event.StepDone{
		Header:   event.Header{Coordinates: identity.Coordinates{SessionID: sessionID, LoopID: loopID, TurnID: turnID, StepID: stepID}},
		Messages: content.AgenticMessages{aiMessage("raw answer")},
	})
	stamper.stamp(t, ctx, h, event.TurnDone{Header: event.Header{Coordinates: turnCoordinates}, TurnIndex: 1, Message: aiMessage("raw answer")})

	w1, w2 := uuid.UUID{0x51}, uuid.UUID{0x52}
	attemptID := event.CompactAttemptID(uuid.UUID{0x53})
	committedID := uuid.UUID{0x54}
	coordinates := identity.Coordinates{SessionID: sessionID, LoopID: loopID}
	postContext := event.ContextMeasurement{
		Basis: event.ContextBasis{Revision: 2, ThroughEventID: committedID},
		Model: validModel("model-x").Key(), RequestFingerprint: [32]byte{0x55},
		InputTokens: 20, InputLimit: 100, Quality: inference.CountQualityExactLocal,
	}
	committed := event.CompactionCommitted{
		Header:    event.Header{EventID: committedID, CreatedAt: fixedClock(), Coordinates: coordinates},
		AttemptID: attemptID, WaiterCommandIDs: []uuid.UUID{w1, w2}, Reason: event.CompactionReasonManual,
		Basis:   event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{0x50}},
		Summary: restoredCompactionSummary("durable summary"), PostContext: postContext,
	}
	if err := h.PublishEvent(ctx, committed); err != nil {
		t.Fatal(err)
	}
	existing := event.CompactWaiterResolved{
		Header: event.Header{
			EventID: event.CompactWaiterReplyID(attemptID, w1, true), CreatedAt: fixedClock(),
			Coordinates: coordinates, Cause: identity.Cause{CommandID: w1},
		},
		AttemptID: attemptID, CommittedEventID: committedID,
	}
	if err := h.PublishEvent(ctx, existing); err != nil {
		t.Fatal(err)
	}
	handOver(t, lease)

	restored, err := restoreTestSession(ctx, definition, sessionID, store)
	if err != nil {
		t.Fatal(err)
	}
	msgs, turnIndex := restoredSnapshot(t, restored)
	if !reflect.DeepEqual(msgs, content.AgenticMessages{committed.Summary}) || turnIndex != 1 {
		t.Fatalf("restored snapshot = %#v turn=%d, want summary-only turn 1", msgs, turnIndex)
	}
	if err := restored.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	afterFirst := replayAllSessionEvents(t, store, sessionID)
	repairID := event.CompactWaiterReplyID(attemptID, w2, true)
	startedIndex, repairIndex, doneIndex := -1, -1, -1
	rawTurn, rawStep, repairCount := false, false, 0
	for index, ev := range afterFirst {
		switch typed := ev.(type) {
		case event.TurnStarted:
			if reflect.DeepEqual(typed.Message, foldUserMsg("raw user")) {
				rawTurn = true
			}
		case event.StepDone:
			if reflect.DeepEqual(typed.Messages, content.AgenticMessages{aiMessage("raw answer")}) {
				rawStep = true
			}
		case event.RestoreStarted:
			if startedIndex < 0 {
				startedIndex = index
			}
		case event.CompactWaiterResolved:
			if typed.EventID == repairID {
				repairCount++
				repairIndex = index
				if typed.AttemptID != attemptID || typed.CommittedEventID != committedID || typed.Cause.CommandID != w2 || typed.Coordinates != coordinates {
					t.Fatalf("repair = %+v, want exact terminal membership", typed)
				}
			}
		case event.RestoreDone:
			if doneIndex < 0 {
				doneIndex = index
			}
		}
	}
	if !rawTurn || !rawStep {
		t.Fatal("restore removed raw pre-compaction journal events")
	}
	if repairCount != 1 || !(startedIndex >= 0 && startedIndex < repairIndex && repairIndex < doneIndex) {
		t.Fatalf("restore ordering started=%d repair=%d done=%d count=%d", startedIndex, repairIndex, doneIndex, repairCount)
	}

	restoredAgain, err := restoreTestSession(ctx, definition, sessionID, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoredAgain.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	repairCount = 0
	for _, ev := range replayAllSessionEvents(t, store, sessionID) {
		if resolved, ok := ev.(event.CompactWaiterResolved); ok && resolved.EventID == repairID {
			repairCount++
		}
	}
	if repairCount != 1 {
		t.Fatalf("deterministic repair count after second restore = %d, want 1", repairCount)
	}
}

type failNthLedger struct {
	storage.Ledger
	mu     sync.Mutex
	armed  bool
	calls  int
	failOn int
	err    error
}

func (l *failNthLedger) arm(failOn int, err error) {
	l.mu.Lock()
	l.armed = true
	l.calls = 0
	l.failOn = failOn
	l.err = err
	l.mu.Unlock()
}

func (l *failNthLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	l.mu.Lock()
	if l.armed {
		l.calls++
		if l.calls == l.failOn {
			l.armed = false
			err := l.err
			l.mu.Unlock()
			return err
		}
	}
	l.mu.Unlock()
	return l.Ledger.Append(ctx, name, expected, payload)
}

func TestRestoreWaiterRepairAppendFailureIsFatalAndRetryable(t *testing.T) {
	t.Parallel()
	tests := []struct{ name string }{{name: "failed repaired waiter append records error and retries once"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runRestoreWaiterRepairAppendFailureIsFatalAndRetryable(t)
		})
	}
}

func runRestoreWaiterRepairAppendFailureIsFatalAndRetryable(t *testing.T) {
	t.Helper()
	base := memstore.New()
	sentinel := errors.New("injected repaired waiter append failure")
	ledger := &failNthLedger{Ledger: base.Ledger}
	backend, err := storage.NewComposite(ledger, base.Leaser, base.KV, base.Blobs)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sessionstore.Open(backend)
	if err != nil {
		t.Fatal(err)
	}
	definition := restoreCfg(&stubLLM{}, "model-x", "be helpful")
	h, sessionID, loopID, lease, _ := newOriginalHub(t, store, fingerprintFromDefinition(definition))
	w := uuid.UUID{0x61}
	attemptID := event.CompactAttemptID(uuid.UUID{0x62})
	committedID := uuid.UUID{0x63}
	coordinates := identity.Coordinates{SessionID: sessionID, LoopID: loopID}
	committed := event.CompactionCommitted{
		Header:    event.Header{EventID: committedID, CreatedAt: fixedClock(), Coordinates: coordinates},
		AttemptID: attemptID, WaiterCommandIDs: []uuid.UUID{w}, Reason: event.CompactionReasonManual,
		Basis:   event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{0x60}},
		Summary: restoredCompactionSummary("summary"),
		PostContext: event.ContextMeasurement{
			Basis: event.ContextBasis{Revision: 2, ThroughEventID: committedID}, Model: validModel("model-x").Key(),
			RequestFingerprint: [32]byte{0x64}, InputTokens: 20, InputLimit: 100, Quality: inference.CountQualityExactLocal,
		},
	}
	if err := h.PublishEvent(context.Background(), committed); err != nil {
		t.Fatal(err)
	}
	handOver(t, lease)

	// Restore appends its new LeaseFence first and RestoreStarted second. Fail
	// exactly the third ledger append: the missing deterministic waiter repair.
	ledger.arm(3, sentinel)
	restored, err := restoreTestSession(context.Background(), definition, sessionID, store)
	if restored != nil {
		t.Fatal("failed restore returned a live controller")
	}
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreAppendFailed || !errors.Is(err, sentinel) {
		t.Fatalf("error = %T %v, want RestoreAppendFailed wrapping sentinel", err, err)
	}
	repairID := event.CompactWaiterReplyID(attemptID, w, true)
	var sawStarted, sawErrored, sawDone bool
	repairCount := 0
	for _, ev := range replayAllSessionEvents(t, store, sessionID) {
		switch typed := ev.(type) {
		case event.RestoreStarted:
			sawStarted = true
		case event.RestoreErrored:
			sawErrored = true
		case event.RestoreDone:
			sawDone = true
		case event.CompactWaiterResolved:
			if typed.EventID == repairID {
				repairCount++
			}
		}
	}
	if !sawStarted || !sawErrored || sawDone || repairCount != 0 {
		t.Fatalf("failed restore started=%v errored=%v done=%v repairCount=%d", sawStarted, sawErrored, sawDone, repairCount)
	}

	retried, err := restoreTestSession(context.Background(), definition, sessionID, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := retried.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	repairCount = 0
	for _, ev := range replayAllSessionEvents(t, store, sessionID) {
		if resolved, ok := ev.(event.CompactWaiterResolved); ok && resolved.EventID == repairID {
			repairCount++
		}
	}
	if repairCount != 1 {
		t.Fatalf("retry repair count = %d, want exactly 1", repairCount)
	}
}
