//go:build integration

package journal_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/content"
	"github.com/inventivepotter/urvi/internal/uuid"
	"github.com/nats-io/nats.go"
)

// seedUUID builds a deterministic non-zero uuid from a single seed byte so the
// integration records carry stable, readable ids. (Mirrors the white-box test
// helper fixedUUID, redefined here because this file is package journal_test.)
func seedUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

// recordKind tags how a stored record's Data must be decoded for the round-trip
// assertion: the three codecs are distinct, so the table names which one applies.
type recordKind uint8

const (
	kindEvent recordKind = iota
	kindCommand
	kindFence
)

// appendCase is one table row: the record to append plus the subject, Msg-Id, and
// codec-keyed expected value the stored record must round-trip back to.
type appendCase struct {
	name        string
	rec         journal.JournalRecord
	kind        recordKind
	wantSubject string
	wantMsgID   string
	// wantEvent/wantCommand/wantFence is the value the stored Data must decode
	// deep-equal to; exactly one is set per row, keyed by kind.
	wantEvent   event.Event
	wantCommand command.Command
	wantFence   journal.LeaseFence
}

// TestSessionJournalAppend exercises the happy path of the single-writer
// serializer against a real embedded JetStream server: a fence, several events,
// and a command are appended in order, and each is asserted to land at a strictly
// monotonic sequence on the expected subject with the expected Nats-Msg-Id, and to
// decode back deep-equal to what was written.
func TestSessionJournalAppend(t *testing.T) {
	sid := seedUUID(0x10)
	lid := seedUUID(0x11)
	tid := seedUUID(0x12)
	stepID := seedUUID(0x13)

	fence := journal.LeaseFence{Epoch: 1}

	sessionStarted := event.SessionStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid},
			EventID:     seedUUID(0x20),
		},
	}
	loopStarted := event.LoopStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid},
			EventID:     seedUUID(0x21),
		},
	}
	stepDone := event.StepDone{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: stepID},
			EventID:     seedUUID(0x22),
		},
		Messages: content.AgenticMessages{
			&content.AIMessage{Message: content.Message{
				Role:   content.RoleAssistant,
				Blocks: []content.Block{&content.TextBlock{Text: "done"}},
			}},
		},
	}
	userInput := command.UserInput{
		Header: command.Header{CommandID: seedUUID(0x30)},
		Blocks: []content.Block{&content.TextBlock{Text: "hello"}},
	}

	cases := []appendCase{
		{
			name:        "fence epoch 1",
			rec:         journal.NewFenceRecord(sid, fence),
			kind:        kindFence,
			wantSubject: journal.FenceSubject(sid),
			wantMsgID:   "1",
			wantFence:   fence,
		},
		{
			name:        "session started",
			rec:         journal.NewEventRecord(sessionStarted),
			kind:        kindEvent,
			wantSubject: journal.SessionEventSubject(sid),
			wantMsgID:   sessionStarted.EventID.String(),
			wantEvent:   sessionStarted,
		},
		{
			name:        "loop started",
			rec:         journal.NewEventRecord(loopStarted),
			kind:        kindEvent,
			wantSubject: journal.LoopEventSubject(sid, lid),
			wantMsgID:   loopStarted.EventID.String(),
			wantEvent:   loopStarted,
		},
		{
			name:        "step done with messages",
			rec:         journal.NewEventRecord(stepDone),
			kind:        kindEvent,
			wantSubject: journal.LoopEventSubject(sid, lid),
			wantMsgID:   stepDone.EventID.String(),
			wantEvent:   stepDone,
		},
		{
			name:        "user input command",
			rec:         journal.NewCommandRecord(sid, lid, userInput),
			kind:        kindCommand,
			wantSubject: journal.LoopCommandSubject(sid, lid),
			wantMsgID:   userInput.CommandID.String(),
			wantCommand: userInput,
		},
	}

	_, js := newEmbeddedJS(t)
	j, err := journal.NewSessionJournal(js, sid)
	if err != nil {
		t.Fatalf("NewSessionJournal: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lastSeq uint64
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq, err := j.Append(ctx, tc.rec)
			if err != nil {
				t.Fatalf("Append(%s): %v", tc.name, err)
			}
			// Sequences are strictly monotonic, 1-based and gap-free.
			wantSeq := uint64(i + 1)
			if seq != wantSeq {
				t.Fatalf("Append(%s) seq = %d, want %d (strictly monotonic)", tc.name, seq, wantSeq)
			}
			lastSeq = seq

			// Read the record back BY SEQUENCE and assert subject + Msg-Id header.
			raw, err := js.GetMsg(journal.StreamName(sid), seq)
			if err != nil {
				t.Fatalf("GetMsg(seq %d): %v", seq, err)
			}
			if raw.Subject != tc.wantSubject {
				t.Errorf("stored subject = %q, want %q", raw.Subject, tc.wantSubject)
			}
			if got := raw.Header.Get(nats.MsgIdHdr); got != tc.wantMsgID {
				t.Errorf("stored %s = %q, want %q", nats.MsgIdHdr, got, tc.wantMsgID)
			}

			// Decode the stored Data via the kind's codec and assert deep-equal.
			assertRoundTrip(t, tc, raw.Data)
		})
	}

	// The stream tip equals the last returned sequence: the journal and the stream
	// agree on the durable length.
	info, err := js.StreamInfo(journal.StreamName(sid))
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if info.State.LastSeq != lastSeq {
		t.Errorf("StreamInfo LastSeq = %d, want %d (last returned seq)", info.State.LastSeq, lastSeq)
	}
	if info.State.Msgs != uint64(len(cases)) {
		t.Errorf("StreamInfo Msgs = %d, want %d", info.State.Msgs, len(cases))
	}
}

// TestNewSessionJournalRejectsMismatchedStream asserts the constructor fails closed
// when an existing stream under the per-session name diverges from the durability
// contract. A stream pre-created with WorkQueue retention (or wrong subjects) is NOT
// silently bound — ensureStream must verify the existing config and return a typed
// *StreamSetupError (phase "verify"), protecting the keep-everything guarantee
// against a fail-open rebind onto a divergent stream.
func TestNewSessionJournalRejectsMismatchedStream(t *testing.T) {
	tests := []struct {
		name   string
		preCfg func(sid uuid.UUID) *nats.StreamConfig
	}{
		{
			name: "wrong retention (WorkQueue)",
			preCfg: func(sid uuid.UUID) *nats.StreamConfig {
				return &nats.StreamConfig{
					Name:      journal.StreamName(sid),
					Subjects:  []string{"urvi.session." + sid.String() + ".>"},
					Retention: nats.WorkQueuePolicy,
				}
			},
		},
		{
			name: "wrong subjects",
			preCfg: func(sid uuid.UUID) *nats.StreamConfig {
				return &nats.StreamConfig{
					Name:      journal.StreamName(sid),
					Subjects:  []string{"some.other.subject.>"},
					Retention: nats.LimitsPolicy,
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			sid := seedUUID(0x40)
			_, js := newEmbeddedJS(t)

			// Pre-create a divergent stream under the per-session name.
			if _, err := js.AddStream(tt.preCfg(sid)); err != nil {
				t.Fatalf("pre-create stream: %v", err)
			}

			j, err := journal.NewSessionJournal(js, sid)
			if err == nil {
				t.Fatalf("NewSessionJournal bound a divergent stream (j=%v); want a verify error", j)
			}
			if j != nil {
				t.Errorf("NewSessionJournal returned non-nil journal %v alongside error", j)
			}
			var setupErr *journal.StreamSetupError
			if !errors.As(err, &setupErr) {
				t.Fatalf("error %v is not *StreamSetupError", err)
			}
			if setupErr.Phase != journal.PhaseVerify {
				t.Errorf("StreamSetupError.Phase = %q, want %q", setupErr.Phase, journal.PhaseVerify)
			}
			if setupErr.Stream != journal.StreamName(sid) {
				t.Errorf("StreamSetupError.Stream = %q, want %q", setupErr.Stream, journal.StreamName(sid))
			}
		})
	}
}

// assertRoundTrip decodes data via the codec named by tc.kind and asserts the
// decoded value is deep-equal to the value tc carried into the append.
func assertRoundTrip(t *testing.T, tc appendCase, data []byte) {
	t.Helper()
	switch tc.kind {
	case kindEvent:
		got, err := event.UnmarshalEvent(data)
		if err != nil {
			t.Fatalf("UnmarshalEvent: %v", err)
		}
		if !reflect.DeepEqual(got, tc.wantEvent) {
			t.Errorf("decoded event = %#v, want %#v", got, tc.wantEvent)
		}
	case kindCommand:
		got, err := command.UnmarshalCommand(data)
		if err != nil {
			t.Fatalf("UnmarshalCommand: %v", err)
		}
		if !reflect.DeepEqual(got, tc.wantCommand) {
			t.Errorf("decoded command = %#v, want %#v", got, tc.wantCommand)
		}
	case kindFence:
		got, err := journal.UnmarshalLeaseFence(data)
		if err != nil {
			t.Fatalf("UnmarshalLeaseFence: %v", err)
		}
		if !reflect.DeepEqual(got, tc.wantFence) {
			t.Errorf("decoded fence = %#v, want %#v", got, tc.wantFence)
		}
	default:
		t.Fatalf("unknown record kind %d", tc.kind)
	}
}
