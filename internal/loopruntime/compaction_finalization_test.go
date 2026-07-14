package loopruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
)

type compactionFinalizationPublisher struct {
	mu       sync.Mutex
	events   []event.Event
	failType event.EventName
	failErr  error
	hook     func(event.Event)
}

func (p *compactionFinalizationPublisher) PublishEvent(_ context.Context, ev event.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

func (p *compactionFinalizationPublisher) PublishEventChecked(ctx context.Context, ev event.Event) error {
	p.mu.Lock()
	failType, failErr, hook := p.failType, p.failErr, p.hook
	p.mu.Unlock()
	if hook != nil {
		hook(ev)
	}
	if failErr != nil && compactionFinalizationEventName(ev) == failType {
		return failErr
	}
	return p.PublishEvent(ctx, ev)
}

func (p *compactionFinalizationPublisher) setFailure(name event.EventName, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failType, p.failErr = name, err
}

func (p *compactionFinalizationPublisher) snapshot() []event.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]event.Event(nil), p.events...)
}

type compactionFinalizationJournalError struct{}

func (*compactionFinalizationJournalError) Error() string { return "test: journal unavailable" }

func TestCompactionFinalizerOwnsCanonicalTerminalAndWaiterProjection(t *testing.T) {
	tests := []struct {
		name          string
		proposal      compactionFinalizationProposal
		wantTerminal  event.EventName
		wantReplyType event.EventName
	}{
		{
			name:          "prepared success commits then resolves every waiter",
			proposal:      compactionFinalizationProposal{Success: validPreparedFinalizationSuccess(9)},
			wantTerminal:  "CompactionCommitted",
			wantReplyType: "CompactWaiterResolved",
		},
		{
			name:          "rejection rejects every waiter",
			proposal:      compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed},
			wantTerminal:  "CompactionRejected",
			wantReplyType: "CompactWaiterRejected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publisher := &compactionFinalizationPublisher{}
			attempt := validFinalizationAttempt()
			finalizer := newCompactionFinalizer(compactionFinalizerConfig{
				Publisher: publisher,
				Factory:   finalizationFactory(),
				SessionID: uuid.UUID{21},
				LoopID:    uuid.UUID{22},
				Now:       func() time.Time { return attempt.StartedAt.Add(5 * time.Second) },
			})

			terminal, err := finalizer.Finalize(context.Background(), attempt, tt.proposal)
			if err != nil {
				t.Fatalf("Finalize() error = %v", err)
			}
			if got := compactionFinalizationEventName(terminal); got != tt.wantTerminal {
				t.Fatalf("terminal name = %q, want %q", got, tt.wantTerminal)
			}
			events := publisher.snapshot()
			if len(events) != 1+len(attempt.WaiterCommandIDs) {
				t.Fatalf("published events = %d, want %d", len(events), 1+len(attempt.WaiterCommandIDs))
			}
			if compactionFinalizationEventName(events[0]) != tt.wantTerminal {
				t.Fatalf("first event = %q, want terminal %q", compactionFinalizationEventName(events[0]), tt.wantTerminal)
			}
			for i, waiterID := range attempt.WaiterCommandIDs {
				reply := events[i+1]
				if compactionFinalizationEventName(reply) != tt.wantReplyType {
					t.Fatalf("reply %d name = %q, want %q", i, compactionFinalizationEventName(reply), tt.wantReplyType)
				}
				if reply.EventHeader().Cause.CommandID != waiterID {
					t.Fatalf("reply %d command = %v, want %v", i, reply.EventHeader().Cause.CommandID, waiterID)
				}
				if err := event.ValidateEvent(reply); err != nil {
					t.Fatalf("ValidateEvent(reply %d) error = %v", i, err)
				}
			}
		})
	}
}

func TestCompactionFinalizerIsIdempotentAcrossCallbackPanics(t *testing.T) {
	tests := []struct {
		name        string
		panicBefore bool
		wantName    event.EventName
	}{
		{name: "panic before durable append permits rejection fallback", panicBefore: true, wantName: "CompactionRejected"},
		{name: "panic after durable append returns existing commit", wantName: "CompactionCommitted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publisher := &compactionFinalizationPublisher{}
			attempt := validFinalizationAttempt()
			finalizer := newCompactionFinalizer(compactionFinalizerConfig{
				Publisher: publisher, Factory: finalizationFactory(), SessionID: uuid.UUID{31}, LoopID: uuid.UUID{32},
				Now: func() time.Time { return attempt.StartedAt.Add(time.Second) },
			})
			func() {
				defer func() { _ = recover() }()
				if tt.panicBefore {
					panic("before finalizer ownership")
				}
				_, err := finalizer.Finalize(context.Background(), attempt, compactionFinalizationProposal{Success: validPreparedFinalizationSuccess(7)})
				if err != nil {
					t.Fatalf("initial Finalize() error = %v", err)
				}
				panic("after finalizer ownership")
			}()

			terminal, err := finalizer.Finalize(context.Background(), attempt, compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed})
			if err != nil {
				t.Fatalf("fallback Finalize() error = %v", err)
			}
			if got := compactionFinalizationEventName(terminal); got != tt.wantName {
				t.Fatalf("terminal name = %q, want %q", got, tt.wantName)
			}
			if got, want := len(publisher.snapshot()), 1+len(attempt.WaiterCommandIDs); got != want {
				t.Fatalf("published events = %d, want exactly %d", got, want)
			}
		})
	}
}

func TestCompactionFinalizerMeasuresBeforeTerminalAppend(t *testing.T) {
	tests := []struct {
		name     string
		proposal compactionFinalizationProposal
	}{
		{name: "commit", proposal: compactionFinalizationProposal{Success: validPreparedFinalizationSuccess(8)}},
		{name: "reject", proposal: compactionFinalizationProposal{RejectReason: event.CompactRejectInvalidSummary}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt := validFinalizationAttempt()
			now := attempt.StartedAt.Add(4 * time.Second)
			publisher := &compactionFinalizationPublisher{hook: func(event.Event) { now = attempt.StartedAt.Add(time.Minute) }}
			finalizer := newCompactionFinalizer(compactionFinalizerConfig{
				Publisher: publisher, Factory: finalizationFactory(), SessionID: uuid.UUID{41}, LoopID: uuid.UUID{42},
				Now: func() time.Time { return now },
			})

			terminal, err := finalizer.Finalize(context.Background(), attempt, tt.proposal)
			if err != nil {
				t.Fatalf("Finalize() error = %v", err)
			}
			var got time.Duration
			switch value := terminal.(type) {
			case event.CompactionCommitted:
				got = value.Duration
			case event.CompactionRejected:
				got = value.Duration
			default:
				t.Fatalf("terminal type = %T", terminal)
			}
			if got != 4*time.Second {
				t.Fatalf("duration = %v, want 4s before append latency", got)
			}
		})
	}
}

func TestCompactionFinalizerDoesNotRecordFailedCanonicalAppend(t *testing.T) {
	tests := []struct {
		name     string
		proposal compactionFinalizationProposal
		failType event.EventName
	}{
		{name: "commit append failure", proposal: compactionFinalizationProposal{Success: validPreparedFinalizationSuccess(6)}, failType: "CompactionCommitted"},
		{name: "rejection append failure", proposal: compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed}, failType: "CompactionRejected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publisher := &compactionFinalizationPublisher{}
			journalErr := &compactionFinalizationJournalError{}
			publisher.setFailure(tt.failType, journalErr)
			attempt := validFinalizationAttempt()
			finalizer := newCompactionFinalizer(compactionFinalizerConfig{
				Publisher: publisher, Factory: finalizationFactory(), SessionID: uuid.UUID{51}, LoopID: uuid.UUID{52},
				Now: func() time.Time { return attempt.StartedAt.Add(time.Second) },
			})

			_, err := finalizer.Finalize(context.Background(), attempt, tt.proposal)
			var finalizationErr *CompactionFinalizationError
			if !errors.As(err, &finalizationErr) || finalizationErr.Kind != CompactionFinalizationTerminalAppend || !errors.Is(err, journalErr) {
				t.Fatalf("Finalize() error = %T %v, want typed terminal append wrapping journal error", err, err)
			}
			if got := publisher.snapshot(); len(got) != 0 {
				t.Fatalf("published events after failed terminal append = %v, want none", got)
			}

			publisher.setFailure("", nil)
			terminal, err := finalizer.Finalize(context.Background(), attempt, compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed})
			if err != nil {
				t.Fatalf("fallback Finalize() error = %v", err)
			}
			if compactionFinalizationEventName(terminal) != "CompactionRejected" {
				t.Fatalf("fallback terminal = %q, want CompactionRejected", compactionFinalizationEventName(terminal))
			}
		})
	}
}

func TestCompactionFinalizerCanonicalizesCommittedPostContextBasis(t *testing.T) {
	tests := []struct {
		name        string
		basis       event.ContextBasis
		wantErrKind CompactionFinalizationErrorKind
	}{
		{name: "post context advances from attempted basis to committed event", basis: event.ContextBasis{Revision: 4, ThroughEventID: uuid.UUID{5}}},
		{name: "revision overflow fails before append", basis: event.ContextBasis{Revision: ^event.ContextRevision(0), ThroughEventID: uuid.UUID{5}}, wantErrKind: CompactionFinalizationTerminalMint},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publisher := &compactionFinalizationPublisher{}
			attempt := validFinalizationAttempt()
			attempt.Basis = tt.basis
			finalizer := newCompactionFinalizer(compactionFinalizerConfig{
				Publisher: publisher, Factory: finalizationFactory(), SessionID: uuid.UUID{71}, LoopID: uuid.UUID{72},
				Now: func() time.Time { return attempt.StartedAt.Add(time.Second) },
			})
			success := &compactionPreparedSuccess{
				Model: inference.ModelKey{Provider: "test", Model: "compactor"}, RequestFingerprint: [32]byte{1},
				Summary: validFinalizationSummary(), PostContext: validFinalizationMeasurement(14),
			}
			success.PostContext.Basis = event.ContextBasis{}

			terminal, err := finalizer.Finalize(context.Background(), attempt, compactionFinalizationProposal{Success: success})
			if tt.wantErrKind != "" {
				var typed *CompactionFinalizationError
				if !errors.As(err, &typed) || typed.Kind != tt.wantErrKind {
					t.Fatalf("Finalize() error = %T %v, want kind %q", err, err, tt.wantErrKind)
				}
				if len(publisher.snapshot()) != 0 {
					t.Fatal("revision overflow published a terminal")
				}
				return
			}
			if err != nil {
				t.Fatalf("Finalize() error = %v", err)
			}
			committed, ok := terminal.(event.CompactionCommitted)
			if !ok {
				t.Fatalf("terminal = %T, want CompactionCommitted", terminal)
			}
			want := event.ContextBasis{Revision: tt.basis.Revision + 1, ThroughEventID: committed.EventID}
			if committed.PostContext.Basis != want {
				t.Fatalf("PostContext basis = %+v, want %+v", committed.PostContext.Basis, want)
			}
		})
	}
}

func TestCompactionFinalizerSeparatesPublishedCachedAndReturnedOwnership(t *testing.T) {
	tests := []struct {
		name     string
		proposal compactionFinalizationProposal
		mutate   func(*testing.T, event.Event)
	}{
		{
			name:     "committed summary and waiters",
			proposal: compactionFinalizationProposal{Success: validPreparedFinalizationSuccess(9)},
			mutate: func(t *testing.T, terminal event.Event) {
				t.Helper()
				committed, ok := terminal.(event.CompactionCommitted)
				if !ok {
					t.Fatalf("terminal = %T, want CompactionCommitted", terminal)
				}
				committed.WaiterCommandIDs[0] = uuid.UUID{0xee}
				text, ok := committed.Summary.Blocks[0].(*content.TextBlock)
				if !ok || text == nil {
					t.Fatalf("summary block = %T, want non-nil TextBlock", committed.Summary.Blocks[0])
				}
				text.Text = "mutated"
			},
		},
		{
			name:     "rejected waiters",
			proposal: compactionFinalizationProposal{RejectReason: event.CompactRejectExecutionFailed},
			mutate: func(t *testing.T, terminal event.Event) {
				t.Helper()
				rejected, ok := terminal.(event.CompactionRejected)
				if !ok {
					t.Fatalf("terminal = %T, want CompactionRejected", terminal)
				}
				rejected.WaiterCommandIDs[0] = uuid.UUID{0xee}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publisher := &compactionFinalizationPublisher{}
			attempt := validFinalizationAttempt()
			finalizer := newCompactionFinalizer(compactionFinalizerConfig{
				Publisher: publisher, Factory: finalizationFactory(), SessionID: uuid.UUID{61}, LoopID: uuid.UUID{62},
				Now: func() time.Time { return attempt.StartedAt.Add(time.Second) },
			})

			first, err := finalizer.Finalize(context.Background(), attempt, tt.proposal)
			if err != nil {
				t.Fatalf("first Finalize() error = %v", err)
			}
			delivered := publisher.snapshot()[0]
			canonical := mustMarshalFinalizationEvent(t, delivered)

			tt.mutate(t, first)
			if got := mustMarshalFinalizationEvent(t, publisher.snapshot()[0]); !bytes.Equal(got, canonical) {
				t.Fatalf("published terminal changed through first-return alias\n got: %s\nwant: %s", got, canonical)
			}
			tt.mutate(t, delivered)
			retry, err := finalizer.Finalize(context.Background(), attempt, compactionFinalizationProposal{RejectReason: event.CompactRejectInternal})
			if err != nil {
				t.Fatalf("retry Finalize() error = %v", err)
			}
			if got := mustMarshalFinalizationEvent(t, retry); !bytes.Equal(got, canonical) {
				t.Fatalf("retry terminal changed through caller/publisher alias\n got: %s\nwant: %s", got, canonical)
			}

			tt.mutate(t, retry)
			secondRetry, err := finalizer.Finalize(context.Background(), attempt, compactionFinalizationProposal{RejectReason: event.CompactRejectInternal})
			if err != nil {
				t.Fatalf("second retry Finalize() error = %v", err)
			}
			if got := mustMarshalFinalizationEvent(t, secondRetry); !bytes.Equal(got, canonical) {
				t.Fatalf("later retry terminal changed through earlier retry alias\n got: %s\nwant: %s", got, canonical)
			}
		})
	}
}

func mustMarshalFinalizationEvent(t *testing.T, value event.Event) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal(%T) error = %v", value, err)
	}
	return encoded
}

func validFinalizationAttempt() compactionAttempt {
	return compactionAttempt{
		AttemptID:        event.CompactAttemptID(uuid.UUID{1}),
		WaiterCommandIDs: []uuid.UUID{{2}, {3}},
		Reason:           event.CompactionReasonManual,
		Basis:            event.ContextBasis{Revision: 4, ThroughEventID: uuid.UUID{5}},
		StartedAt:        time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC),
	}
}

func validFinalizationSummary() *content.UserMessage {
	return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{
		&content.TextBlock{Text: "<conversation_summary><goal>ship</goal><constraints></constraints><decisions></decisions><state>working</state><open_items></open_items></conversation_summary>"},
	}}}
}

func validFinalizationMeasurement(seed byte) event.ContextMeasurement {
	return event.ContextMeasurement{
		Basis:              event.ContextBasis{Revision: event.ContextRevision(seed), ThroughEventID: uuid.UUID{seed}},
		Model:              inference.ModelKey{Provider: "test", Model: "compactor"},
		RequestFingerprint: [32]byte{seed},
		InputTokens:        content.TokenCount(seed),
		InputLimit:         100,
		Quality:            inference.CountQualityExactLocal,
	}
}

func validPreparedFinalizationSuccess(seed byte) *compactionPreparedSuccess {
	measurement := validFinalizationMeasurement(seed)
	return &compactionPreparedSuccess{
		Model: measurement.Model, RequestFingerprint: [32]byte{0xf1},
		Summary: validFinalizationSummary(), PostContext: measurement,
	}
}

func finalizationFactory() *event.Factory {
	next := byte(80)
	return event.NewFactory(func() (uuid.UUID, error) {
		next++
		return uuid.UUID{next}, nil
	}, func() time.Time { return time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC) })
}

func compactionFinalizationEventName(ev event.Event) event.EventName {
	switch ev.(type) {
	case event.CompactionCommitted:
		return "CompactionCommitted"
	case event.CompactionRejected:
		return "CompactionRejected"
	case event.CompactWaiterResolved:
		return "CompactWaiterResolved"
	case event.CompactWaiterRejected:
		return "CompactWaiterRejected"
	default:
		return ""
	}
}
