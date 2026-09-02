package hub

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// resultAppender is a controllable eventAppenderResult double for hub unit tests. Each
// call consults decisions (in order; the last entry repeats once exhausted) to decide
// whether that append reports Appended=true or Appended=false, and always assigns a
// fresh, strictly increasing sequence regardless of the decision (mirroring a real
// idempotent journal, which still reports the ORIGINAL sequence on a dedup — tests that
// care about the reported sequence set it explicitly via seqOverride). It is safe for
// concurrent use.
type resultAppender struct {
	mu          sync.Mutex
	decisions   []bool
	seqOverride map[int]uint64 // 1-based call index -> sequence to report
	appended    []event.Event  // every event this appender was asked to append, in order
	calls       int
	err         error
}

func (a *resultAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	seq, _, err := a.AppendEventResult(ctx, ev)
	return seq, err
}

func (a *resultAppender) AppendEventResult(_ context.Context, ev event.Event) (uint64, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return 0, false, a.err
	}
	a.appended = append(a.appended, ev)
	appended := true
	if len(a.decisions) > 0 {
		idx := a.calls - 1
		if idx >= len(a.decisions) {
			idx = len(a.decisions) - 1
		}
		appended = a.decisions[idx]
	}
	seq := uint64(a.calls)
	if s, ok := a.seqOverride[a.calls]; ok {
		seq = s
	}
	return seq, appended, nil
}

func (a *resultAppender) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// TestNopAppenderResultAlwaysAppended proves nopEventAppender's optional extension
// always reports Appended=true, so a bare New(sessionID) hub keeps its pre-extension
// behavior: every Enduring publish applies and delivers, never gated.
func TestNopAppenderResultAlwaysAppended(t *testing.T) {
	t.Parallel()
	seq, appended, err := nopEventAppender{}.AppendEventResult(context.Background(), event.StepDone{})
	if err != nil {
		t.Fatalf("AppendEventResult() error = %v, want nil", err)
	}
	if !appended {
		t.Error("AppendEventResult() appended = false, want true")
	}
	if seq != 0 {
		t.Errorf("AppendEventResult() seq = %d, want 0", seq)
	}
}

// TestPublishGatesBroadcastOnDeduplicatedAppend proves a Hub whose injected appender
// implements the optional eventAppenderResult extension applies and delivers a
// genuinely new append (Appended=true) but neither applies nor delivers a deduplicated
// retry (Appended=false) — and reports the SAME nil success both times, since a
// deduplicated retry already durably landed and already delivered via its original
// call.
func TestPublishGatesBroadcastOnDeduplicatedAppend(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	app := &resultAppender{decisions: []bool{true, false}}
	h := New(session, WithAppender(app))

	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}

	ev := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}

	// First publish: a genuinely new append. Applied and delivered.
	if err := h.PublishEventChecked(context.Background(), ev); err != nil {
		t.Fatalf("first PublishEventChecked() error = %v, want nil", err)
	}
	delivery := recvDelivery(t, sub)
	if _, ok := delivery.Event.(event.StepDone); !ok {
		t.Fatalf("first delivery = %T, want event.StepDone", delivery.Event)
	}
	if delivery.JournalSeq != 1 {
		t.Errorf("first delivery.JournalSeq = %d, want 1", delivery.JournalSeq)
	}

	// Second publish of the SAME event: the appender reports a deduplicated retry.
	// PublishEventChecked still succeeds (the record IS durable — just not from this
	// call), but nothing new is applied or broadcast.
	if err := h.PublishEventChecked(context.Background(), ev); err != nil {
		t.Fatalf("second (deduplicated) PublishEventChecked() error = %v, want nil", err)
	}
	expectNone(t, sub)

	if got := app.callCount(); got != 2 {
		t.Fatalf("appender calls = %d, want 2 (both the original and the deduplicated retry reach the appender)", got)
	}
}

// TestPublishAppenderWithoutResultExtensionAlwaysApplies proves an appender that
// implements only the OLD single-method eventAppender surface (no
// AppendEventResult) is treated exactly as before this extension existed: every
// successful append is applied and delivered, never gated.
func TestPublishAppenderWithoutResultExtensionAlwaysApplies(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	app := &fakeAppender{}
	h := New(session, WithAppender(app))

	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}

	ev := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}
	for i := 0; i < 2; i++ {
		if err := h.PublishEventChecked(context.Background(), ev); err != nil {
			t.Fatalf("PublishEventChecked() [%d] error = %v, want nil", i, err)
		}
		if _, ok := recv(t, sub).(event.StepDone); !ok {
			t.Fatalf("publish [%d] was not delivered", i)
		}
	}
	if got := app.callCount(); got != 2 {
		t.Errorf("appender calls = %d, want 2 (a plain eventAppender is never gated)", got)
	}
}

// TestPublishDeduplicatedAppendUncheckedNeverFaults proves the unchecked PublishEvent
// variant also treats a deduplicated retry as a silent, faultless no-op: no state
// mutation, no broadcast, no reported fault.
func TestPublishDeduplicatedAppendUncheckedNeverFaults(t *testing.T) {
	t.Parallel()
	session := mustID(t)
	loopA := mustID(t)
	rep := &recordingReporter{}
	app := &resultAppender{decisions: []bool{true, false}}
	h := New(session, WithAppender(app), WithFaultReporter(rep))

	sub, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}

	ev := event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: session, LoopID: loopA}}}
	if err := h.PublishEvent(context.Background(), ev); err != nil {
		t.Fatalf("first PublishEvent() error = %v, want nil", err)
	}
	recv(t, sub)

	if err := h.PublishEvent(context.Background(), ev); err != nil {
		t.Fatalf("second (deduplicated) PublishEvent() error = %v, want nil", err)
	}
	expectNone(t, sub)

	if faults := rep.reported(); len(faults) != 0 {
		t.Errorf("reported %d faults for a deduplicated retry, want 0", len(faults))
	}
}

// committedAppender is a controllable eventAppenderCommitted double. It mints a
// distinct "stored canonical body" per public enduring append and hands the SAME
// backing array back each time it is asked, exactly as a real journal reporting the
// bytes it wrote would — which is what makes the per-subscriber clone testable: if
// the hub forwarded this slice directly, two subscribers would share one array.
type committedAppender struct {
	mu        sync.Mutex
	supported bool
	appended  []event.Event
	stored    map[uint64][]byte
	dedupe    map[int]bool // 1-based call index -> report Appended=false
	suppress  map[int]bool // 1-based call index -> commit with no public body
	foreignID map[int]bool // 1-based call index -> commit carrying ANOTHER event's id
	calls     int
	err       error
}

func newCommittedAppender() *committedAppender {
	return &committedAppender{supported: true, stored: make(map[uint64][]byte)}
}

func (a *committedAppender) SupportsCommittedPublicBodies() bool { return a.supported }

func (a *committedAppender) AppendEvent(ctx context.Context, ev event.Event) (uint64, error) {
	commit, err := a.AppendEventCommitted(ctx, ev)
	return commit.Sequence, err
}

func (a *committedAppender) AppendEventResult(ctx context.Context, ev event.Event) (uint64, bool, error) {
	commit, err := a.AppendEventCommitted(ctx, ev)
	return commit.Sequence, commit.Appended, err
}

func (a *committedAppender) AppendEventCommitted(_ context.Context, ev event.Event) (event.AppendCommit, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return event.AppendCommit{}, a.err
	}
	a.appended = append(a.appended, ev)
	seq := uint64(a.calls)
	if a.dedupe[a.calls] {
		return event.AppendCommit{Sequence: seq}, nil
	}
	commit := event.AppendCommit{Sequence: seq, Appended: true}
	if !a.supported || a.suppress[a.calls] || ev.Class() != event.Enduring || ev.Visibility() != event.Public {
		return commit, nil
	}
	body := []byte(`{"stored":` + strconv.FormatUint(seq, 10) + `}`)
	a.stored[seq] = body
	// Mint the public id from the event's OWN header, exactly as sessionwire.Project
	// does. A double that minted an unrelated id (this one used to mint "public-N")
	// would make every delivery look mis-paired to the hub's pairing guard — and,
	// worse, would let a real mis-pairing pass unnoticed here.
	commit.EventID = ev.EventHeader().EventID.String()
	if a.foreignID[a.calls] {
		commit.EventID = "00000000-0000-0000-0000-00000000ffff"
	}
	commit.PublicBody = body
	commit.CoveredThrough = seq
	return commit, nil
}

func (a *committedAppender) storedBody(seq uint64) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stored[seq]
}

// sessionEvent builds a well-formed session-scoped enduring event for hub publishes.
func sessionEvent(t *testing.T, sid uuid.UUID) event.SessionStarted {
	t.Helper()
	return event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid},
		EventID:     mustID(t),
	}}
}

// TestCommittedPublicEventsCapabilityRequiresCommittedAppender proves the segregated
// capability is advertised through the SUBSCRIBE call site — not merely by a type
// assertion one level away — and only when the injected appender can actually report
// stored canonical bytes. A no-persistence hub, a legacy result-only appender, and a
// committed-shaped appender whose journal cannot report bytes all refuse, while
// SubscribeEvents keeps working for every one of them.
func TestCommittedPublicEventsCapabilityRequiresCommittedAppender(t *testing.T) {
	t.Parallel()
	unsupported := newCommittedAppender()
	unsupported.supported = false
	tests := []struct {
		name string
		opts []Option
		want bool
	}{
		{name: "no persistence", want: false},
		{name: "legacy result appender", opts: []Option{WithAppender(&resultAppender{})}, want: false},
		{name: "committed shape without bytes", opts: []Option{WithAppender(unsupported)}, want: false},
		{name: "committed appender", opts: []Option{WithAppender(newCommittedAppender())}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := New(mustID(t), tt.opts...)
			if got := h.CommittedPublicEventsSupported(); got != tt.want {
				t.Errorf("CommittedPublicEventsSupported() = %t, want %t", got, tt.want)
			}
			sub, err := h.SubscribeCommittedPublicEvents(allFilter())
			if tt.want {
				if err != nil {
					t.Fatalf("SubscribeCommittedPublicEvents() error = %v, want nil", err)
				}
				_ = sub.Close()
			} else {
				var unavailable *CommittedPublicEventsUnavailableError
				if !errors.As(err, &unavailable) {
					t.Fatalf("SubscribeCommittedPublicEvents() error = %T %v, want *CommittedPublicEventsUnavailableError", err, err)
				}
				if sub != nil {
					t.Errorf("SubscribeCommittedPublicEvents() subscription = %v, want nil", sub)
				}
			}
			// The compatibility surface is unconditional either way.
			compat, err := h.SubscribeEvents(allFilter())
			if err != nil {
				t.Fatalf("SubscribeEvents() error = %v", err)
			}
			_ = compat.Close()
		})
	}
}

// TestCommittedDeliveryCarriesStoredBodyAndCoverage proves the committed append
// result rides onto BOTH the compatibility stream and the segregated committed
// stream: same event, same journal sequence, the exact stored bytes, and a
// CoveredThrough equal to — never past — this append's own sequence.
func TestCommittedDeliveryCarriesStoredBodyAndCoverage(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	app := newCommittedAppender()
	h := New(sid, WithAppender(app))
	compat, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}
	committed, err := h.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents() error = %v", err)
	}

	ev := sessionEvent(t, sid)
	if err := h.PublishEvent(context.Background(), ev); err != nil {
		t.Fatalf("PublishEvent() error = %v", err)
	}

	for name, sub := range map[string]*EventSubscription{"compat": compat, "committed": committed} {
		d := recvDelivery(t, sub)
		if d.JournalSeq != 1 {
			t.Errorf("%s JournalSeq = %d, want 1", name, d.JournalSeq)
		}
		if d.EventID != ev.EventID.String() {
			t.Errorf("%s EventID = %q, want the event's own committed id %q", name, d.EventID, ev.EventID)
		}
		if !bytes.Equal(d.PublicBody, app.storedBody(1)) {
			t.Errorf("%s PublicBody = %s, want the stored body %s", name, d.PublicBody, app.storedBody(1))
		}
		if d.CoveredThrough != d.JournalSeq {
			t.Errorf("%s CoveredThrough = %d, want %d (its own committed sequence)", name, d.CoveredThrough, d.JournalSeq)
		}
		if !d.Committed() {
			t.Errorf("%s Committed() = false, want true", name)
		}
	}
}

// TestFanOutClonesPublicBodyPerSubscriber proves each subscriber receives its OWN
// backing array: one consumer mutating its delivery in place cannot corrupt a peer's
// bytes, nor the bytes the journal stored. A single-subscriber test cannot see this.
func TestFanOutClonesPublicBodyPerSubscriber(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	app := newCommittedAppender()
	h := New(sid, WithAppender(app))
	first, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents(first) error = %v", err)
	}
	second, err := h.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents(second) error = %v", err)
	}

	if err := h.PublishEvent(context.Background(), sessionEvent(t, sid)); err != nil {
		t.Fatalf("PublishEvent() error = %v", err)
	}
	want := bytes.Clone(app.storedBody(1))

	firstDelivery := recvDelivery(t, first)
	secondDelivery := recvDelivery(t, second)
	if len(firstDelivery.PublicBody) == 0 {
		t.Fatalf("first delivery carried no public body")
	}
	for i := range firstDelivery.PublicBody {
		firstDelivery.PublicBody[i] = 'X'
	}
	if !bytes.Equal(secondDelivery.PublicBody, want) {
		t.Errorf("peer PublicBody = %s after an in-place edit by another subscriber, want %s", secondDelivery.PublicBody, want)
	}
	if !bytes.Equal(app.storedBody(1), want) {
		t.Errorf("stored body = %s after a subscriber's in-place edit, want %s", app.storedBody(1), want)
	}
}

// TestEphemeralDeliveryCarriesNoCommittedFields proves an ephemeral event is never
// persisted and therefore never claims an id, bytes, or coverage on the compatibility
// stream, and never appears at all on the segregated committed stream — whose whole
// contract is that every delivery carries committed canonical bytes.
func TestEphemeralDeliveryCarriesNoCommittedFields(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	app := newCommittedAppender()
	h := New(sid, WithAppender(app))
	compat, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}
	committed, err := h.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents() error = %v", err)
	}

	ephemeral := event.TokenDelta{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: mustID(t), TurnID: mustID(t)},
		EventID:     mustID(t),
	}}
	if err := h.PublishEvent(context.Background(), ephemeral); err != nil {
		t.Fatalf("PublishEvent(ephemeral) error = %v", err)
	}
	enduring := sessionEvent(t, sid)
	if err := h.PublishEvent(context.Background(), enduring); err != nil {
		t.Fatalf("PublishEvent(enduring) error = %v", err)
	}

	d := recvDelivery(t, compat)
	if _, ok := d.Event.(event.TokenDelta); !ok {
		t.Fatalf("compat first delivery = %T, want event.TokenDelta", d.Event)
	}
	if d.JournalSeq != 0 || d.EventID != "" || d.PublicBody != nil || d.CoveredThrough != 0 {
		t.Errorf("ephemeral delivery = %+v, want zero journal sequence, id, body, and coverage", d)
	}
	if d.Committed() {
		t.Error("ephemeral Committed() = true, want false")
	}

	// The committed stream skips the ephemeral entirely and starts at the enduring
	// event; a skipped ephemeral is not a gap, because it is reconstructable from the
	// authoritative event that follows it.
	committedDelivery := recvDelivery(t, committed)
	if _, ok := committedDelivery.Event.(event.SessionStarted); !ok {
		t.Fatalf("committed first delivery = %T, want event.SessionStarted", committedDelivery.Event)
	}
	if !committedDelivery.Committed() {
		t.Errorf("committed delivery Committed() = false, want true")
	}
}

// TestDerivedSessionEventCarriesCommittedBody proves the derived SessionActive edge
// — which the hub synthesizes and appends itself, with no triggering publish of its
// own — rides its OWN committed append result, not the triggering event's.
func TestDerivedSessionEventCarriesCommittedBody(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	app := newCommittedAppender()
	h := New(sid, WithAppender(app))
	committed, err := h.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents() error = %v", err)
	}

	start := event.TurnStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: mustID(t), TurnID: mustID(t)},
		EventID:     mustID(t),
		Cause:       identity.Cause{CommandID: mustID(t)},
	}}
	if err := h.PublishEvent(context.Background(), start); err != nil {
		t.Fatalf("PublishEvent() error = %v", err)
	}

	first := recvDelivery(t, committed)
	if _, ok := first.Event.(event.TurnStarted); !ok {
		t.Fatalf("first committed delivery = %T, want event.TurnStarted", first.Event)
	}
	derived := recvDelivery(t, committed)
	if _, ok := derived.Event.(event.SessionActive); !ok {
		t.Fatalf("second committed delivery = %T, want event.SessionActive", derived.Event)
	}
	if derived.JournalSeq != 2 {
		t.Errorf("derived delivery seq = %d, want 2", derived.JournalSeq)
	}
	if derived.EventID != derived.Event.EventHeader().EventID.String() {
		t.Errorf("derived EventID = %q, want its own header id %q",
			derived.EventID, derived.Event.EventHeader().EventID)
	}
	if derived.EventID == first.EventID {
		t.Errorf("derived carried the triggering event's committed id %q", first.EventID)
	}
	if !bytes.Equal(derived.PublicBody, app.storedBody(2)) {
		t.Errorf("derived PublicBody = %s, want the stored body %s", derived.PublicBody, app.storedBody(2))
	}
	if derived.CoveredThrough != derived.JournalSeq {
		t.Errorf("derived CoveredThrough = %d, want %d", derived.CoveredThrough, derived.JournalSeq)
	}
	if bytes.Equal(derived.PublicBody, first.PublicBody) {
		t.Errorf("derived body equals the triggering event's body %s; each append must carry its own", first.PublicBody)
	}
}

// TestDeduplicatedAppendDeliversNothingOnEitherStream proves a deduplicated retry
// neither re-broadcasts nor claims coverage: the original append already delivered it.
func TestDeduplicatedAppendDeliversNothingOnEitherStream(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	app := newCommittedAppender()
	app.dedupe = map[int]bool{2: true}
	h := New(sid, WithAppender(app))
	compat, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}
	committed, err := h.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents() error = %v", err)
	}

	ev := sessionEvent(t, sid)
	if err := h.PublishEvent(context.Background(), ev); err != nil {
		t.Fatalf("first PublishEvent() error = %v", err)
	}
	if err := h.PublishEvent(context.Background(), ev); err != nil {
		t.Fatalf("retry PublishEvent() error = %v", err)
	}
	for name, sub := range map[string]*EventSubscription{"compat": compat, "committed": committed} {
		_ = recvDelivery(t, sub)
		if got := len(sub.Events()); got != 0 {
			t.Errorf("%s buffered %d deliveries after a deduplicated retry, want 0", name, got)
		}
	}
}

// TestCommittedSubscriptionFailsClosedWithoutStoredBytes proves the committed stream
// never silently skips an enduring public event. If a committed-capable hub somehow
// delivers one without stored bytes, that subscription is failed with the typed loss
// error — the same fail-loud treatment an enduring overflow gets — rather than the
// consumer being left with an invisible hole in its sequence coverage.
func TestCommittedSubscriptionFailsClosedWithoutStoredBytes(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	app := newCommittedAppender()
	app.suppress = map[int]bool{1: true}
	h := New(sid, WithAppender(app))
	committed, err := h.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents() error = %v", err)
	}
	if err := h.PublishEvent(context.Background(), sessionEvent(t, sid)); err != nil {
		t.Fatalf("PublishEvent() error = %v", err)
	}
	// PublishEvent is synchronous on this goroutine, so the subscription's terminal
	// state is already decided when it returns — no waiting, and no timeout standing
	// in for an assertion.
	var loss *SubscriptionLossError
	if !errors.As(committed.Err(), &loss) {
		t.Fatalf("committed Err() = %T %v, want *SubscriptionLossError", committed.Err(), committed.Err())
	}
	// The cause must distinguish this from congestion: a consumer that read it as
	// backpressure would resubscribe forever against a hub that cannot satisfy the
	// committed-bytes contract.
	if !errors.Is(committed.Err(), ErrCommittedBodyMissing) {
		t.Errorf("loss cause = %v, want ErrCommittedBodyMissing", committed.Err())
	}
	// The TEXT matters too, and nothing else pins it. errors.Is is what a program
	// branches on, but the message is what an operator reads, and this loss is not an
	// overflow: saying so would send them hunting a slow consumer that does not
	// exist. A future tidy-up that re-merges the two branches of
	// SubscriptionLossError.Error() must fail here rather than at a release.
	message := committed.Err().Error()
	if strings.Contains(message, "egress overflow") {
		t.Errorf("loss message = %q, must not blame egress overflow for a missing committed body", message)
	}
	if got := strings.Count(message, "hub: "); got != 1 {
		t.Errorf("loss message = %q has %d %q prefixes, want exactly 1", message, got, "hub: ")
	}
	select {
	case _, open := <-committed.Events():
		if open {
			t.Fatal("committed stream delivered an enduring event without stored bytes")
		}
	default:
		t.Fatal("committed egress channel is still open after the loss")
	}
}

// TestCommittedSubscriptionFailsOnEnduringOverflow proves the committed stream keeps
// the class-aware overflow policy: a full egress buffer fails the subscription on an
// enduring event rather than dropping committed bytes silently.
func TestCommittedSubscriptionFailsOnEnduringOverflow(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	app := newCommittedAppender()
	h := New(sid, WithAppender(app))
	committed, err := h.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents() error = %v", err)
	}
	for range defaultEgressBuffer + 1 {
		if err := h.PublishEvent(context.Background(), sessionEvent(t, sid)); err != nil {
			t.Fatalf("PublishEvent() error = %v", err)
		}
	}
	var loss *SubscriptionLossError
	if !errors.As(committed.Err(), &loss) {
		t.Fatalf("committed Err() = %T %v, want *SubscriptionLossError", committed.Err(), committed.Err())
	}
	if loss.DroppedClass != event.Enduring {
		t.Errorf("DroppedClass = %v, want Enduring", loss.DroppedClass)
	}
	// An overflow loss is congestion, not a broken invariant, and must NOT claim the
	// missing-body cause.
	if errors.Is(committed.Err(), ErrCommittedBodyMissing) {
		t.Errorf("overflow loss reported ErrCommittedBodyMissing: %v", committed.Err())
	}
}

// TestDeliveryFailsClosedOnMispairedCommit pins the pairing guard. deliver takes a
// commit, and the type system cannot say whether it is the RIGHT commit: pairing a
// different append's result compiles, and it is the one mis-delivery that announces
// nothing — the consumer simply receives two deliveries under one (sequence, EventID)
// and renders one event twice or drops the other as a duplicate.
//
// Both streams must fail, not just the committed one. A compatibility subscriber stamps
// its SSE ids from JournalSeq, so handing it another append's sequence corrupts it too;
// there is no subscriber the hub may describe a delivery wrongly to.
func TestDeliveryFailsClosedOnMispairedCommit(t *testing.T) {
	t.Parallel()
	sid := mustID(t)
	app := newCommittedAppender()
	app.foreignID = map[int]bool{1: true}
	h := New(sid, WithAppender(app))
	compat, err := h.SubscribeEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeEvents() error = %v", err)
	}
	committed, err := h.SubscribeCommittedPublicEvents(allFilter())
	if err != nil {
		t.Fatalf("SubscribeCommittedPublicEvents() error = %v", err)
	}

	if err := h.PublishEvent(context.Background(), sessionEvent(t, sid)); err != nil {
		t.Fatalf("PublishEvent() error = %v", err)
	}

	for name, sub := range map[string]*EventSubscription{"compat": compat, "committed": committed} {
		if !errors.Is(sub.Err(), ErrCommitEventMismatch) {
			t.Errorf("%s Err() = %v, want ErrCommitEventMismatch", name, sub.Err())
		}
		select {
		case _, open := <-sub.Events():
			if open {
				t.Errorf("%s received a delivery carrying another append's identity", name)
			}
		default:
			t.Errorf("%s egress channel is still open after a mis-paired commit", name)
		}
	}
}
