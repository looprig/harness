package journal

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// countingAround is an AroundAppend that records how many appends it decorated and
// proves the delegated call really ran inside it.
type countingAround struct {
	calls int
	inner int
}

func (c *countingAround) around(ctx context.Context, _ JournalRecord, next func(context.Context) error) error {
	c.calls++
	err := next(ctx)
	c.inner++
	return err
}

// TestDecoratePreservesOptionalJournalContracts is the omission guard for every journal
// decorator in the workspace. Decorate must advertise EXACTLY the optional contracts its
// delegate advertises: advertising fewer silently demotes the journal (the hub stops
// seeing Appended=false and re-broadcasts deduplicated retries; the committed-bytes
// capability vanishes rather than degrading), and advertising more would promise a seam
// the delegate cannot serve.
//
// The reflection check at the end makes this maintenance-free for the failure mode that
// actually happens — a seam added to the journal and forgotten in Decorate's type switch.
// It requires the decorated value to carry every Append* method its delegate has, so a
// fourth seam implemented by one of these doubles but not forwarded fails here without
// anyone remembering to assert it.
//
// Its reach is PROBABILISTIC, not structural, and that limit is worth stating rather
// than assuming closed: the sweep sees exactly the three doubles this table names
// (declared in appender_test.go). A seam added to the real *sessionstore.sessionJournal
// AND exercised only through some new double elsewhere would fire nothing here. It caught the committed seam because that
// seam was added to the existing committed double, which is the customary path — but
// "customary" is the whole guarantee.
func TestDecoratePreservesOptionalJournalContracts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		inner          SessionJournal
		wantIdempotent bool
		wantCommitted  bool
	}{
		{name: "plain", inner: &recordingJournal{}},
		{name: "idempotent", inner: newIdempotentRecordingJournal(), wantIdempotent: true},
		{name: "committed", inner: newCommittedRecordingJournal(), wantIdempotent: true, wantCommitted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			counter := &countingAround{}
			decorated := Decorate(tt.inner, counter.around)

			idem, gotIdempotent := decorated.(IdempotentJournal)
			if gotIdempotent != tt.wantIdempotent {
				t.Fatalf("decorated %T implements IdempotentJournal = %t, want %t",
					decorated, gotIdempotent, tt.wantIdempotent)
			}
			committed, gotCommitted := decorated.(CommittedPublicJournal)
			if gotCommitted != tt.wantCommitted {
				t.Fatalf("decorated %T implements CommittedPublicJournal = %t, want %t",
					decorated, gotCommitted, tt.wantCommitted)
			}

			record := NewEventRecord(event.SessionStarted{Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: fixedUUID(0x51)},
				EventID:     fixedUUID(0x52),
			}})
			if _, err := decorated.Append(context.Background(), record); err != nil {
				t.Fatalf("Append() error = %v", err)
			}
			wantCalls := 1
			if gotIdempotent {
				// The SAME record: an idempotent delegate must still report the retry as
				// deduplicated THROUGH the decorator. Losing this is the defect that
				// makes the hub re-apply and re-broadcast an already-delivered event.
				result, err := idem.AppendIdempotent(context.Background(), record)
				if err != nil {
					t.Fatalf("AppendIdempotent() error = %v", err)
				}
				if result.Appended {
					t.Error("AppendIdempotent() Appended = true for a retry through the decorator, want false")
				}
				wantCalls++
			}
			if gotCommitted {
				fresh := NewEventRecord(event.SessionStarted{Header: event.Header{
					Coordinates: identity.Coordinates{SessionID: fixedUUID(0x51)},
					EventID:     fixedUUID(0x53),
				}})
				result, err := committed.AppendCommitted(context.Background(), fresh)
				if err != nil {
					t.Fatalf("AppendCommitted() error = %v", err)
				}
				if !result.Appended || len(result.Public.Body) == 0 || result.Public.EventID == "" {
					t.Errorf("AppendCommitted() = %+v, want a committed public body through the decorator", result)
				}
				wantCalls++
			}
			if counter.calls != wantCalls || counter.inner != wantCalls {
				t.Errorf("decorator observed %d appends (%d completed), want %d on every seam",
					counter.calls, counter.inner, wantCalls)
			}

			// Auto-catch for a seam added to the delegate and forgotten in Decorate.
			//
			// Restricted to Append* because that is what a decorator can forward, and
			// these doubles are shared with the rest of this package's tests: an
			// author adding an ordinary accessor to one of them would otherwise get a
			// red test claiming the production decorator ate a method it never could
			// carry. A guard that cries wolf gets weakened or deleted, and this one is
			// the only thing standing between a fourth append seam and the silent
			// amputation that shipped twice already. Every seam is Append* by
			// construction (SessionJournal.Append, IdempotentJournal.AppendIdempotent,
			// CommittedPublicJournal.AppendCommitted), so the narrower property is the
			// right trade for a guard whose value is entirely in surviving to fire.
			delegateType := reflect.TypeOf(tt.inner)
			decoratedType := reflect.TypeOf(decorated)
			for i := range delegateType.NumMethod() {
				name := delegateType.Method(i).Name
				if !strings.HasPrefix(name, "Append") {
					continue
				}
				if _, exists := decoratedType.MethodByName(name); !exists {
					t.Errorf("Decorate dropped %s: %v does not carry it", name, decoratedType)
				}
			}
		})
	}
}

// TestDecorateDegenerateInputs pins the two guards a composition root relies on: a nil
// journal stays nil (the checked appender constructors report it, rather than a
// decorator manufacturing a non-nil wrapper around nothing), and a nil decorator returns
// the delegate untouched so no capability is lost to a no-op wrapper.
func TestDecorateDegenerateInputs(t *testing.T) {
	t.Parallel()
	if got := Decorate(nil, func(ctx context.Context, _ JournalRecord, next func(context.Context) error) error {
		return next(ctx)
	}); got != nil {
		t.Errorf("Decorate(nil, around) = %v, want nil", got)
	}
	inner := newCommittedRecordingJournal()
	if got := Decorate(inner, nil); got != SessionJournal(inner) {
		t.Errorf("Decorate(inner, nil) = %v, want the delegate itself", got)
	}
}
