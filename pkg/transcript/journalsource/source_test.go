package journalsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/ciram-co/looprig/pkg/command"
	"github.com/ciram-co/looprig/pkg/event"
	"github.com/ciram-co/looprig/pkg/journal"
	"github.com/ciram-co/looprig/pkg/transcript"
	"github.com/ciram-co/looprig/pkg/uuid"
)

// errBoom is the sentinel non-EOF read error a fakeCursor injects mid-stream.
var errBoom = errors.New("boom")

// fakeReplayer is an in-memory journal.RecordReplayer for tests — NO NATS. Open hands
// out a single fakeCursor backed by a fixed slice of records (or fails with openErr).
// opens counts Open calls so a test can assert lazy-open (Open not called until the
// first Next).
type fakeReplayer struct {
	records []journal.JournalRecord
	errAt   int   // index at which the cursor's Next returns midErr; <0 disables
	midErr  error // the non-EOF error returned at errAt
	openErr error // when non-nil, Open fails with it (cursor is never built)

	opens  int
	cursor *fakeCursor
}

func (f *fakeReplayer) Open(_ context.Context, _ journal.ReplayRequest) (journal.RecordCursor, error) {
	f.opens++
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.cursor = &fakeCursor{records: f.records, errAt: f.errAt, midErr: f.midErr}
	return f.cursor, nil
}

// fakeCursor walks records in order, returns io.EOF past the end, and (when errAt >= 0)
// injects midErr at index errAt. It counts Close calls so a test can assert Close-once.
type fakeCursor struct {
	records []journal.JournalRecord
	idx     int
	errAt   int
	midErr  error
	closed  int
}

func (c *fakeCursor) Next(_ context.Context) (journal.JournalRecord, uint64, error) {
	if c.errAt >= 0 && c.idx == c.errAt {
		return nil, 0, c.midErr
	}
	if c.idx >= len(c.records) {
		return nil, 0, io.EOF
	}
	rec := c.records[c.idx]
	c.idx++
	return rec, uint64(c.idx), nil
}

func (c *fakeCursor) Close() error {
	c.closed++
	return nil
}

// summarize renders a transcript.Record as "<kind>:<payload-type>" so a test can assert
// both the mapped variant and the carried payload identity in order.
func summarize(t *testing.T, rec transcript.Record) string {
	t.Helper()
	switch r := rec.(type) {
	case transcript.EventRecord:
		return "event:" + fmt.Sprintf("%T", r.Event)
	case transcript.CommandRecord:
		return "command:" + fmt.Sprintf("%T", r.Command)
	default:
		t.Fatalf("unexpected transcript.Record type %T", rec)
		return ""
	}
}

// drain pulls from src until it returns an error, summarizing each yielded record.
func drain(t *testing.T, src transcript.RecordSource) (got []string, termErr error) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		rec, err := src.Next(ctx)
		if err != nil {
			return got, err
		}
		got = append(got, summarize(t, rec))
	}
	t.Fatalf("drain exceeded safety bound — source never returned an error")
	return nil, nil
}

func TestOpenNext(t *testing.T) {
	t.Parallel()

	ev1 := journal.NewEventRecord(event.SessionStarted{})
	cmd1 := journal.NewCommandRecord(uuid.UUID{}, uuid.UUID{}, command.ApproveToolCall{})
	fence := journal.NewFenceRecord(uuid.UUID{}, journal.LeaseFence{Epoch: 1})
	ev2 := journal.NewEventRecord(event.SessionStopped{})

	tests := []struct {
		name       string
		records    []journal.JournalRecord
		errAt      int
		midErr     error
		want       []string
		wantErr    error // errors.Is target for the terminal error
		wantClosed int
	}{
		{
			name:       "events and command in order, fence dropped, then EOF",
			records:    []journal.JournalRecord{ev1, cmd1, fence, ev2},
			errAt:      -1,
			want:       []string{"event:event.SessionStarted", "command:command.ApproveToolCall", "event:event.SessionStopped"},
			wantErr:    io.EOF,
			wantClosed: 1,
		},
		{
			name:       "only fences yields immediate EOF",
			records:    []journal.JournalRecord{fence, fence},
			errAt:      -1,
			want:       nil,
			wantErr:    io.EOF,
			wantClosed: 1,
		},
		{
			name:       "empty stream yields EOF",
			records:    nil,
			errAt:      -1,
			want:       nil,
			wantErr:    io.EOF,
			wantClosed: 1,
		},
		{
			name:       "non-EOF read error mid-stream surfaces verbatim and closes cursor",
			records:    []journal.JournalRecord{ev1, cmd1},
			errAt:      1,
			midErr:     errBoom,
			want:       []string{"event:event.SessionStarted"},
			wantErr:    errBoom,
			wantClosed: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rr := &fakeReplayer{records: tt.records, errAt: tt.errAt, midErr: tt.midErr}
			src := Open(rr, journal.ReplayRequest{From: journal.Beginning()})

			got, termErr := drain(t, src)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("drained records = %v, want %v", got, tt.want)
			}
			if !errors.Is(termErr, tt.wantErr) {
				t.Errorf("terminal error = %v, want errors.Is %v", termErr, tt.wantErr)
			}
			// A non-EOF error must NOT be masked as io.EOF.
			if !errors.Is(tt.wantErr, io.EOF) && errors.Is(termErr, io.EOF) {
				t.Errorf("terminal error masked as io.EOF: %v", termErr)
			}
			if rr.cursor == nil {
				t.Fatalf("cursor was never opened")
			}
			if rr.cursor.closed != tt.wantClosed {
				t.Errorf("cursor Close count = %d, want %d", rr.cursor.closed, tt.wantClosed)
			}
			if rr.opens != 1 {
				t.Errorf("replayer Open count = %d, want 1", rr.opens)
			}

			// After termination, further Next stays io.EOF and does not double-close.
			if _, err := src.Next(context.Background()); !errors.Is(err, io.EOF) {
				t.Errorf("post-termination Next error = %v, want io.EOF", err)
			}
			if rr.cursor.closed != tt.wantClosed {
				t.Errorf("cursor Close count after extra Next = %d, want %d (no double close)", rr.cursor.closed, tt.wantClosed)
			}
		})
	}
}

func TestOpenLazyOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		openErr    error
		wantErr    error // errors.Is target for the first Next error; nil ⇒ expect a record
		wantRecord bool
	}{
		{
			name:       "open deferred to first Next, then yields a record",
			openErr:    nil,
			wantRecord: true,
		},
		{
			name:    "open error surfaces on first Next",
			openErr: errBoom,
			wantErr: errBoom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rr := &fakeReplayer{
				records: []journal.JournalRecord{journal.NewEventRecord(event.SessionStarted{})},
				errAt:   -1,
				openErr: tt.openErr,
			}

			src := Open(rr, journal.ReplayRequest{From: journal.Beginning()})

			// Lazy: constructing the source must NOT open the cursor.
			if rr.opens != 0 {
				t.Fatalf("Open called before first Next: opens = %d, want 0", rr.opens)
			}

			rec, err := src.Next(context.Background())
			if rr.opens != 1 {
				t.Fatalf("first Next did not open exactly once: opens = %d, want 1", rr.opens)
			}

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("first Next error = %v, want errors.Is %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("first Next unexpected error: %v", err)
			}
			if tt.wantRecord {
				if _, ok := rec.(transcript.EventRecord); !ok {
					t.Fatalf("first Next record = %T, want transcript.EventRecord", rec)
				}
			}
		})
	}
}

func TestUnexpectedRecordError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		wantSub string
	}{
		{
			name:    "bare value is errors.As-able and names the type",
			err:     &UnexpectedRecordError{RecordType: "journal.someFutureRecord"},
			wantSub: "journal.someFutureRecord",
		},
		{
			name:    "wrapped value remains errors.As-able and names the type",
			err:     fmt.Errorf("bridge: %w", &UnexpectedRecordError{RecordType: "journal.someFutureRecord"}),
			wantSub: "journal.someFutureRecord",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var target *UnexpectedRecordError
			if !errors.As(tt.err, &target) {
				t.Fatalf("errors.As(%v, *UnexpectedRecordError) = false, want true", tt.err)
			}
			msg := target.Error()
			if !strings.Contains(msg, tt.wantSub) {
				t.Errorf("Error() = %q, want it to contain %q", msg, tt.wantSub)
			}
			if !strings.Contains(msg, "unexpected journal record type") {
				t.Errorf("Error() = %q, want a sensible 'unexpected journal record type' message", msg)
			}
		})
	}
}

func TestExportUnavailableError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{
			name:    "zero value is errors.As-able with a generic message",
			err:     &ExportUnavailableError{},
			wantMsg: "transcript export unavailable: session is not journal-backed",
		},
		{
			name:    "wrapped value remains errors.As-able and keeps its reason",
			err:     fmt.Errorf("export: %w", &ExportUnavailableError{Reason: "in-memory session"}),
			wantMsg: "transcript export unavailable: in-memory session",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var target *ExportUnavailableError
			if !errors.As(tt.err, &target) {
				t.Fatalf("errors.As(%v, *ExportUnavailableError) = false, want true", tt.err)
			}
			if target.Error() != tt.wantMsg {
				t.Errorf("Error() = %q, want %q", target.Error(), tt.wantMsg)
			}
		})
	}
}
