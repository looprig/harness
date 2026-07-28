package journal

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
)

type hookedRecordingJournal struct {
	mu       sync.Mutex
	records  []JournalRecord
	seq      uint64
	err      error
	onAppend func(context.Context, JournalRecord)
}

func (j *hookedRecordingJournal) Append(ctx context.Context, record JournalRecord) (uint64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.records = append(j.records, record)
	if j.onAppend != nil {
		j.onAppend(ctx, record)
	}
	return j.seq, j.err
}

func TestWithHooksObservesEachRecordFamilyAndDelegatesOnce(t *testing.T) {
	t.Parallel()

	sessionID := fixedUUID(0xa1)
	eventID := fixedUUID(0xa2)
	commandID := fixedUUID(0xa3)
	gateID := fixedUUID(0xa4)
	tests := []struct {
		name       string
		record     JournalRecord
		wantFamily hook.RecordFamily
		wantID     string
	}{
		{
			name: "event",
			record: NewEventRecord(event.SessionStarted{Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sessionID},
				EventID:     eventID,
			}}),
			wantFamily: hook.RecordEvent,
			wantID:     eventID.String(),
		},
		{
			name: "event pointer",
			record: eventRecordPointer(NewEventRecord(event.SessionStarted{Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sessionID},
				EventID:     eventID,
			}})),
			wantFamily: hook.RecordEvent,
			wantID:     eventID.String(),
		},
		{
			name:       "command",
			record:     NewCommandRecord(sessionID, fixedUUID(0xa5), command.Interrupt{Header: command.Header{CommandID: commandID}}),
			wantFamily: hook.RecordCommand,
			wantID:     commandID.String(),
		},
		{
			name: "gate prepared",
			record: NewGatePreparedRecord(event.GatePrepared{Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sessionID},
				EventID:     gateID,
			}}, gate.OpenPayload{}),
			wantFamily: hook.RecordGatePrepared,
			wantID:     gateID.String(),
		},
		{
			name:       "fence",
			record:     NewFenceRecord(sessionID, LeaseFence{Epoch: 42}),
			wantFamily: hook.RecordFence,
			wantID:     "42",
		},
	}

	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var began hook.Call
			var finished hook.Result
			runner, err := hook.Compile(hook.Set{Around: []hook.Around{{
				Operation: hook.OperationJournalAppend,
				Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
					began = call
					return ctx, func(result hook.Result) { finished = result }
				},
			}}})
			if err != nil {
				t.Fatalf("hook.Compile: %v", err)
			}
			delegate := &hookedRecordingJournal{seq: 77}
			journal := WithHooks(delegate, runner, sessionID)

			seq, err := journal.Append(context.Background(), testCase.record)
			if err != nil || seq != 77 {
				t.Fatalf("Append = (%d, %v), want (77, nil)", seq, err)
			}
			if len(delegate.records) != 1 || !reflect.DeepEqual(delegate.records[0], testCase.record) {
				t.Fatalf("delegate records = %#v, want exactly original record", delegate.records)
			}
			if began.Operation != hook.OperationJournalAppend ||
				began.Coordinates != (identity.Coordinates{SessionID: sessionID}) ||
				began.JournalAppend == nil ||
				began.JournalAppend.Family != testCase.wantFamily ||
				began.JournalAppend.RecordID != testCase.wantID {
				t.Fatalf("begin call = %#v, want session/family/id %v/%q", began, testCase.wantFamily, testCase.wantID)
			}
			if finished.Outcome != hook.OutcomeCompleted || finished.Err != nil ||
				finished.EndedAt.Before(finished.StartedAt) {
				t.Fatalf("finish = %#v, want completed terminal", finished)
			}
		})
	}
}

func eventRecordPointer(record EventRecord) *EventRecord {
	return &record
}

func TestWithHooksPreservesAppendTerminalAndClassifiesOutcome(t *testing.T) {
	t.Parallel()

	appendErr := errors.New("append failed")
	tests := []struct {
		name        string
		ctx         context.Context
		err         error
		begin       func(context.Context) context.Context
		wantOutcome hook.Outcome
	}{
		{name: "success", ctx: context.Background(), wantOutcome: hook.OutcomeCompleted},
		{name: "failure", ctx: context.Background(), err: appendErr, wantOutcome: hook.OutcomeFailed},
		{name: "canceled wins over failure", ctx: canceledContext(), err: appendErr, wantOutcome: hook.OutcomeCanceled},
		{
			name: "original cancellation survives observer context replacement",
			ctx:  canceledContext(),
			err:  appendErr,
			begin: func(context.Context) context.Context {
				return context.Background()
			},
			wantOutcome: hook.OutcomeCanceled,
		},
		{
			name: "observer derived cancellation wins over failure",
			ctx:  context.Background(),
			err:  appendErr,
			begin: func(context.Context) context.Context {
				return canceledContext()
			},
			wantOutcome: hook.OutcomeCanceled,
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var got hook.Result
			runner, err := hook.Compile(hook.Set{Around: []hook.Around{{
				Operation: hook.OperationJournalAppend,
				Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
					if testCase.begin != nil {
						ctx = testCase.begin(ctx)
					}
					return ctx, func(result hook.Result) { got = result }
				},
			}}})
			if err != nil {
				t.Fatalf("hook.Compile: %v", err)
			}
			delegate := &hookedRecordingJournal{seq: 91, err: testCase.err}
			record := NewFenceRecord(fixedUUID(0xb1), LeaseFence{Epoch: 3})

			seq, err := WithHooks(delegate, runner, fixedUUID(0xb1)).Append(testCase.ctx, record)
			if seq != 91 || !errors.Is(err, testCase.err) {
				t.Fatalf("Append = (%d, %v), want (91, %v)", seq, err, testCase.err)
			}
			if got.Outcome != testCase.wantOutcome || !errors.Is(got.Err, testCase.err) {
				t.Fatalf("result = %#v, want outcome %v and err %v", got, testCase.wantOutcome, testCase.err)
			}
		})
	}
}

func TestWithHooksSuccessfulAppendIsCompletedDespiteCancellation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		context  func() (context.Context, context.CancelFunc)
		delegate func(context.CancelFunc) func(context.Context, JournalRecord)
	}{
		{
			name: "already canceled",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
		},
		{
			name: "delegate races cancellation with successful return",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			delegate: func(cancel context.CancelFunc) func(context.Context, JournalRecord) {
				return func(context.Context, JournalRecord) { cancel() }
			},
		},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := testCase.context()
			defer cancel()
			var result hook.Result
			runner, err := hook.Compile(hook.Set{Around: []hook.Around{{
				Operation: hook.OperationJournalAppend,
				Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
					return ctx, func(got hook.Result) { result = got }
				},
			}}})
			if err != nil {
				t.Fatalf("hook.Compile: %v", err)
			}
			delegate := &hookedRecordingJournal{seq: 37}
			if testCase.delegate != nil {
				delegate.onAppend = testCase.delegate(cancel)
			}

			seq, err := WithHooks(delegate, runner, fixedUUID(0xb2)).Append(
				ctx,
				NewFenceRecord(fixedUUID(0xb2), LeaseFence{Epoch: 4}),
			)
			if err != nil || seq != 37 {
				t.Fatalf("Append = (%d, %v), want (37, nil)", seq, err)
			}
			if result.Outcome != hook.OutcomeCompleted {
				t.Fatalf("Outcome = %v, want Completed for successful delegate", result.Outcome)
			}
		})
	}
}

func TestWithHooksInvalidRecordPreservesDelegateResult(t *testing.T) {
	t.Parallel()

	delegateErr := &RecordKindError{}
	var nilEvent *EventRecord
	var nilCommand *CommandRecord
	var nilGate *GatePreparedRecord
	var nilFence *FenceRecord
	tests := []struct {
		name   string
		record JournalRecord
	}{
		{name: "nil interface", record: nil},
		{name: "typed nil event", record: nilEvent},
		{name: "typed nil command", record: nilCommand},
		{name: "typed nil gate", record: nilGate},
		{name: "typed nil fence", record: nilFence},
		{name: "zero event", record: EventRecord{}},
		{name: "zero command", record: CommandRecord{}},
	}
	for _, testCase := range tests {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var began int
			runner, err := hook.Compile(hook.Set{Around: []hook.Around{{
				Operation: hook.OperationJournalAppend,
				Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
					began++
					return ctx, nil
				},
			}}})
			if err != nil {
				t.Fatalf("hook.Compile: %v", err)
			}
			delegate := &hookedRecordingJournal{seq: 83, err: delegateErr}

			seq, err := WithHooks(delegate, runner, fixedUUID(0xb3)).Append(context.Background(), testCase.record)
			if seq != 83 || err != delegateErr {
				t.Fatalf("Append = (%d, %v), want exact (83, %v)", seq, err, delegateErr)
			}
			if began != 0 {
				t.Fatalf("hook began %d times for unsafe metadata, want 0", began)
			}
		})
	}
}

func TestWithHooksObserverPanicsDoNotChangeAppend(t *testing.T) {
	t.Parallel()

	runner, err := hook.Compile(hook.Set{Around: []hook.Around{
		{
			Operation: hook.OperationJournalAppend,
			Begin: func(context.Context, hook.Call) (context.Context, hook.FinishFunc) {
				panic("begin")
			},
		},
		{
			Operation: hook.OperationJournalAppend,
			Begin: func(ctx context.Context, _ hook.Call) (context.Context, hook.FinishFunc) {
				return ctx, func(hook.Result) { panic("finish") }
			},
		},
	}})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	delegate := &hookedRecordingJournal{seq: 12}

	seq, err := WithHooks(delegate, runner, fixedUUID(0xc1)).Append(
		context.Background(),
		NewFenceRecord(fixedUUID(0xc1), LeaseFence{Epoch: 9}),
	)
	if err != nil || seq != 12 || len(delegate.records) != 1 {
		t.Fatalf("Append = (%d, %v), records=%d; observer changed behavior", seq, err, len(delegate.records))
	}
}

func TestWithHooksPreservesNilJournalForCheckedComposition(t *testing.T) {
	t.Parallel()

	runner, err := hook.Compile(hook.Set{})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	if got := WithHooks(nil, runner, fixedUUID(0xd1)); got != nil {
		t.Fatalf("WithHooks(nil) = %T, want nil so checked appenders reject bad wiring", got)
	}
}

func TestWithHooksReturnsDelegateWhenJournalAppendIsUnregistered(t *testing.T) {
	t.Parallel()

	runner, err := hook.Compile(hook.Set{})
	if err != nil {
		t.Fatalf("hook.Compile: %v", err)
	}
	delegate := &hookedRecordingJournal{}
	if got := WithHooks(delegate, runner, fixedUUID(0xd2)); got != delegate {
		t.Fatalf("WithHooks(empty runner) = %T, want original delegate", got)
	}
	if middleware := HookMiddleware(runner, fixedUUID(0xd2)); middleware != nil {
		t.Fatal("HookMiddleware(empty runner) is non-nil")
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

var _ SessionJournal = (*hookedRecordingJournal)(nil)
