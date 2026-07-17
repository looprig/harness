package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

func privacyUUID(seed byte) uuid.UUID {
	var id uuid.UUID
	for index := range id {
		id[index] = seed
	}
	return id
}

func privacyHustleEvents(t *testing.T) []event.Event {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName("privacy.audit"),
		hustle.WithParticipation(hustle.ParticipationBlocking),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1, OutputBytes: 1}),
		hustle.WithCurrentLoopModel(),
		hustle.WithSystemPrompt("private-prompt-marker", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	header := event.Header{
		Coordinates:     identity.Coordinates{SessionID: privacyUUID(1)},
		EventID:         privacyUUID(2),
		EventVisibility: event.Internal,
	}
	run := event.HustleRunDescriptor{
		Definition: definition.Descriptor(),
		RunID:      hustle.RunID(privacyUUID(3)),
	}
	completedRun := run
	completedRun.Runtime = event.ModelRuntime{Key: model.ModelKey{Provider: "provider", Model: "model"}}
	return []event.Event{
		event.HustleStarted{Header: header, Run: run},
		event.HustleCompleted{Header: header, Run: completedRun},
		event.HustleFailed{Header: header, Run: run, Stage: hustle.StageQueue, ReasonCode: hustle.ReasonCanceled},
	}
}

func privacyCompactionEvents() (event.CompactionStarted, event.CompactionCommitted) {
	basis := event.ContextBasis{Revision: 1, ThroughEventID: privacyUUID(4)}
	header := event.Header{
		Coordinates: identity.Coordinates{SessionID: privacyUUID(1), LoopID: privacyUUID(5)},
		EventID:     privacyUUID(6),
	}
	attemptID := event.CompactAttemptID(privacyUUID(7))
	started := event.CompactionStarted{
		Header:    header,
		AttemptID: attemptID,
		Reason:    event.CompactionReasonManual,
		Basis:     basis,
	}
	committed := event.CompactionCommitted{
		Header:           header,
		AttemptID:        attemptID,
		WaiterCommandIDs: []uuid.UUID{privacyUUID(8)},
		Reason:           event.CompactionReasonManual,
		Basis:            basis,
		Summary: &content.UserMessage{Message: content.Message{
			Role: content.RoleUser,
			Blocks: []content.Block{
				&content.TextBlock{Text: "summary"},
			},
		}},
		PostContext: event.ContextMeasurement{
			Basis:              basis,
			Model:              model.ModelKey{Provider: "provider", Model: "model"},
			RequestFingerprint: [32]byte{1},
			InputTokens:        1,
			InputLimit:         100,
			Quality:            contextcount.CountQualityExactLocal,
		},
	}
	return started, committed
}

func TestStatusEventMarshalJSONRejectsNonPublicVisibility(t *testing.T) {
	t.Parallel()
	internal := privacyHustleEvents(t)
	_, committed := privacyCompactionEvents()
	tests := []struct {
		name       string
		ev         event.Event
		wantErr    bool
		visibility event.EventVisibility
	}{
		{name: "nil event remains omittable"},
		{name: "normal public event", ev: event.SessionStarted{Header: fixSessionHeader}},
		{name: "public compaction committed", ev: committed},
		{name: "internal hustle started", ev: internal[0], wantErr: true, visibility: event.Internal},
		{name: "internal hustle completed", ev: internal[1], wantErr: true, visibility: event.Internal},
		{name: "internal hustle failed", ev: internal[2], wantErr: true, visibility: event.Internal},
		{name: "unknown visibility", ev: event.SessionStarted{Header: event.Header{EventVisibility: event.EventVisibility(99)}}, wantErr: true, visibility: event.EventVisibility(99)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := json.Marshal(StatusEvent{JournalSeq: 1, Event: tt.ev})
			if (err != nil) != tt.wantErr {
				t.Fatalf("json.Marshal(StatusEvent) error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var visibilityErr *NonPublicEventError
			if !errors.As(err, &visibilityErr) {
				t.Fatalf("error = %T %v, want *NonPublicEventError", err, err)
			}
			if visibilityErr.Visibility != tt.visibility {
				t.Errorf("Visibility = %d, want %d", visibilityErr.Visibility, tt.visibility)
			}
		})
	}
}

func TestReadHandlersRejectNonPublicEventsBeforeSuccess(t *testing.T) {
	t.Parallel()
	const sidText = "44444444-4444-4444-4444-444444444444"
	sid := parseTestUUID(t, sidText)
	internal := privacyHustleEvents(t)
	unknown := event.SessionStarted{Header: event.Header{EventVisibility: event.EventVisibility(99)}}
	tests := []struct {
		name        string
		route       string
		ev          event.Event
		wantMessage string
	}{
		{name: "status last turn rejects hustle started", route: "last_turn", ev: internal[0], wantMessage: msgStatusFailed},
		{name: "status last turn rejects hustle completed", route: "last_turn", ev: internal[1], wantMessage: msgStatusFailed},
		{name: "status last step rejects hustle failed", route: "last_step", ev: internal[2], wantMessage: msgStatusFailed},
		{name: "status rejects unknown visibility", route: "last_turn", ev: unknown, wantMessage: msgStatusFailed},
		{name: "journal rejects hustle started", route: "journal", ev: internal[0], wantMessage: msgJournalFailed},
		{name: "journal rejects hustle completed", route: "journal", ev: internal[1], wantMessage: msgJournalFailed},
		{name: "journal rejects hustle failed", route: "journal", ev: internal[2], wantMessage: msgJournalFailed},
		{name: "journal rejects unknown visibility", route: "journal", ev: unknown, wantMessage: msgJournalFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeReader{}
			srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, reader, newConfig())
			rec := httptest.NewRecorder()
			switch tt.route {
			case "last_turn":
				reader.status = SessionStatus{SessionID: sid, LastTurn: &StatusEvent{JournalSeq: 1, Event: tt.ev}}
				srv.handleStatus(rec, readRequest("/v1/sessions/"+sidText+"/status", sidText))
			case "last_step":
				reader.status = SessionStatus{SessionID: sid, LastStep: &StatusEvent{JournalSeq: 1, Event: tt.ev}}
				srv.handleStatus(rec, readRequest("/v1/sessions/"+sidText+"/status", sidText))
			case "journal":
				reader.journal = EventJournalPage{Events: []StatusEvent{{JournalSeq: 1, Event: tt.ev}}}
				srv.handleJournal(rec, readRequest("/v1/sessions/"+sidText+"/journal", sidText))
			default:
				t.Fatalf("unknown route fixture %q", tt.route)
			}
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (body %s)", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, tt.wantMessage) {
				t.Errorf("body = %q, want generic message %q", body, tt.wantMessage)
			}
			for _, forbidden := range []string{"Hustle", "privacy.audit", "private-prompt-marker", `"run"`, `"visibility"`} {
				if strings.Contains(body, forbidden) {
					t.Errorf("body leaked %q: %s", forbidden, body)
				}
			}
			assertErrorEnvelope(t, rec)
		})
	}
}

func TestEncodeDeliveryRejectsNonPublicVisibility(t *testing.T) {
	t.Parallel()
	internal := privacyHustleEvents(t)
	started, committed := privacyCompactionEvents()
	tests := []struct {
		name string
		ev   event.Event
		want bool
	}{
		{name: "nil event skipped"},
		{name: "internal hustle started skipped", ev: internal[0]},
		{name: "internal hustle completed skipped", ev: internal[1]},
		{name: "internal hustle failed skipped", ev: internal[2]},
		{name: "unknown visibility skipped", ev: event.SessionStarted{Header: event.Header{EventVisibility: event.EventVisibility(99)}}},
		{name: "public compaction started encoded", ev: started, want: true},
		{name: "public compaction committed encoded", ev: committed, want: true},
		{name: "normal public event encoded", ev: event.TurnDone{Header: fixTurnHeader}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, got := encodeDelivery(event.Delivery{Event: tt.ev, JournalSeq: 1})
			if got != tt.want {
				t.Errorf("encodeDelivery() ok = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandleEventsSkipsNonPublicLiveDeliveries(t *testing.T) {
	t.Parallel()
	sid := parseTestUUID(t, eventsSIDStr)
	internal := privacyHustleEvents(t)
	started, committed := privacyCompactionEvents()
	unknown := event.SessionStarted{Header: event.Header{EventVisibility: event.EventVisibility(99)}}
	public := []event.Delivery{
		{Event: started},
		{Event: committed, JournalSeq: 9},
	}
	tests := []struct {
		name    string
		skipped []event.Event
	}{
		{name: "live session cannot inject private audit", skipped: []event.Event{internal[0], internal[1], internal[2], unknown}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sub := &fakeSubscription{ch: make(chan event.Delivery, len(tt.skipped)+len(public))}
			sess := &fakeSession{sub: sub}
			srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, quietConfig())
			srv.registry.put(sid, sess)
			rec := newFlushRecorder()
			done := runEvents(srv, rec, eventsRequest(t, context.Background(), eventsSIDStr))
			for _, ev := range tt.skipped {
				sub.ch <- event.Delivery{Event: ev, JournalSeq: 8}
			}
			for _, delivery := range public {
				sub.ch <- delivery
			}
			for range public {
				select {
				case <-rec.flushes:
				case <-time.After(streamDeadline):
					t.Fatalf("public frame was not flushed within %v", streamDeadline)
				}
			}
			close(sub.ch)
			select {
			case <-done:
			case <-time.After(streamDeadline):
				t.Fatalf("handler did not return within %v", streamDeadline)
			}
			var want strings.Builder
			for _, delivery := range public {
				frame, ok := encodeDelivery(delivery)
				if !ok {
					t.Fatalf("public %T unexpectedly skipped", delivery.Event)
				}
				_, _ = want.Write(frame)
			}
			if got := rec.snapshot(); got != want.String() {
				t.Errorf("stream = %q, want only public frames %q", got, want.String())
			}
		})
	}
}
