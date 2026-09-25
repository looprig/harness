package sessionruntime

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/present"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/sessionstore"
)

func requireNoIntentRecord(t *testing.T, f *runtimeCommandFixture, runtimeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	replayer, err := f.store.OpenInternalRecordReplayer(f.sid, sessionstore.ReplayRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: f.sid, From: journal.Beginning()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cursor.Close() }()
	for {
		record, _, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if intent, ok := record.(journal.CommandRecord); ok {
			if submitted, ok := intent.Command().(command.UserInput); ok && submitted.CommandID == runtimeID {
				t.Fatal("presenter refusal wrote an intent")
			}
		}
	}
}

func TestPresenterFailureRefusesAnAttemptWithNoIntent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		kind runtimecommand.Kind
		p    *countingPresenter
		want present.ErrorKind
	}{
		{name: "input presenter error", kind: runtimecommand.KindInput, p: &countingPresenter{err: errors.New("directory down")}, want: present.ErrorPresenterFailed},
		{name: "input invalid frame", kind: runtimecommand.KindInput, p: &countingPresenter{frame: present.Frame{Prefix: []content.Block{&content.ThinkingBlock{Thinking: "x"}}}}, want: present.ErrorFrameInvalid},
		{name: "create presenter error", kind: runtimecommand.KindCreate, p: &countingPresenter{err: errors.New("directory down")}, want: present.ErrorPresenterFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRuntimeCommandFixture(t)
			WithCommandAppender(journal.NewJournalCommandAppender(f.journal))(f.session)
			WithMessagePresenter(tc.p)(f.session)
			admitted := f.admittedInput("cmd-1", mustUUID(), "hi")
			admitted.Kind = tc.kind
			admitted.AttemptID = "attempt-1"
			disposition, err := f.session.ApplyRuntimeCommand(context.Background(), admitted)
			var pe *present.Error
			if !errors.As(err, &pe) || pe.Kind != tc.want {
				t.Fatalf("err = %v, want *present.Error{%s}", err, tc.want)
			}
			if disposition.PrefixSequence == 0 {
				t.Fatal("refusal has no durable prefix")
			}
			if got := onlyDisposition(t, f); got.Disposition != runtimecommand.DispositionRefused {
				t.Fatalf("disposition = %q, want refused", got.Disposition)
			}
			f.requireNoCommand(t, "a refused presentation")
			requireNoIntentRecord(t, f, admitted.RuntimeCommandID)
			if second, err := f.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil || !second.Duplicate {
				t.Fatalf("redelivery = %+v, %v", second, err)
			}
			if tc.p.count() != 1 {
				t.Fatalf("presenter calls = %d, want 1", tc.p.count())
			}
			// A successor may no longer install this presenter. The refused
			// prefix still deduplicates before any new audit intent is written.
			f.session.presenter = nil
			if third, err := f.session.ApplyRuntimeCommand(context.Background(), admitted); err != nil || !third.Duplicate {
				t.Fatalf("redelivery after presenter removal = %+v, %v", third, err)
			}
			requireNoIntentRecord(t, f, admitted.RuntimeCommandID)
		})
	}
}

func TestPresenterFailureOnALegacyCommandWritesNothing(t *testing.T) {
	t.Parallel()
	f := newRuntimeCommandFixture(t)
	WithMessagePresenter(&countingPresenter{err: errors.New("x")})(f.session)
	disposition, err := f.session.ApplyRuntimeCommand(context.Background(), f.admittedInput("cmd-1", mustUUID(), "hi"))
	var pe *present.Error
	if !errors.As(err, &pe) || disposition != (runtimecommand.Disposition{}) {
		t.Fatalf("got %+v, %v; want zero disposition and *present.Error", disposition, err)
	}
	if got := len(readDispositions(t, f)); got != 0 {
		t.Fatalf("%d dispositions written", got)
	}
}
