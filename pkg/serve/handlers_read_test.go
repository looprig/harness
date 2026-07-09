package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// fakeReader is a test double for the read plane: each method returns the configured
// DTO / error and records the arguments it was called with, so a handler test can
// assert the parsed Page/JournalPage flowed through unchanged.
type fakeReader struct {
	list    SessionList
	listErr error
	gotPage Page

	status    SessionStatus
	statusErr error
	gotStatus uuid.UUID

	journal      EventJournalPage
	journalErr   error
	gotJournalID uuid.UUID
	gotJournal   JournalPage
}

func (f *fakeReader) ListSessions(_ context.Context, page Page) (SessionList, error) {
	f.gotPage = page
	return f.list, f.listErr
}

func (f *fakeReader) ReadStatus(_ context.Context, id uuid.UUID) (SessionStatus, error) {
	f.gotStatus = id
	return f.status, f.statusErr
}

func (f *fakeReader) ReadJournal(_ context.Context, id uuid.UUID, page JournalPage) (EventJournalPage, error) {
	f.gotJournalID = id
	f.gotJournal = page
	return f.journal, f.journalErr
}

// readRequest builds a GET request to path, stamping the {sid} path value the mux
// would set (empty sid leaves it unset).
func readRequest(path, sid string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	if sid != "" {
		req.SetPathValue("sid", sid)
	}
	return req
}

func TestServerHandleListSessions(t *testing.T) {
	t.Parallel()

	sample := SessionList{
		Sessions: []SessionSummary{{SessionID: parseTestUUID(t, "11111111-1111-1111-1111-111111111111"), State: "idle"}},
		Skip:     0,
		Limit:    100,
		NextSkip: 0,
		Done:     true,
	}

	tests := []struct {
		name       string
		query      string
		listErr    error
		wantStatus int
		wantSkip   int
		wantLimit  int
	}{
		{name: "absent params use defaults", query: "", wantStatus: http.StatusOK, wantSkip: 0, wantLimit: 100},
		{name: "explicit skip", query: "?skip=5", wantStatus: http.StatusOK, wantSkip: 5, wantLimit: 100},
		{name: "explicit limit", query: "?limit=25", wantStatus: http.StatusOK, wantSkip: 0, wantLimit: 25},
		{name: "skip and limit", query: "?skip=10&limit=50", wantStatus: http.StatusOK, wantSkip: 10, wantLimit: 50},
		{name: "limit at cap ok", query: "?limit=1000", wantStatus: http.StatusOK, wantSkip: 0, wantLimit: 1000},
		{name: "limit over cap is 400", query: "?limit=1001", wantStatus: http.StatusBadRequest},
		{name: "negative skip is 400", query: "?skip=-1", wantStatus: http.StatusBadRequest},
		{name: "non-numeric limit is 400", query: "?limit=abc", wantStatus: http.StatusBadRequest},
		{name: "store read error is 500", query: "", listErr: StoreReadError{Op: "list", Cause: errBoom}, wantStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := &fakeReader{list: sample, listErr: tt.listErr}
			srv := newServer[*fakeSession](&fakeRunner{}, reader, newConfig())

			req := readRequest("/v1/sessions"+tt.query, "")
			rec := httptest.NewRecorder()
			srv.handleListSessions(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				assertErrorEnvelope(t, rec)
				return
			}
			if reader.gotPage.Skip != tt.wantSkip || reader.gotPage.Limit != tt.wantLimit {
				t.Errorf("page = %+v, want {Skip:%d Limit:%d}", reader.gotPage, tt.wantSkip, tt.wantLimit)
			}
			var got SessionList
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode list body: %v", err)
			}
			if got.Skip != sample.Skip || got.Limit != sample.Limit || got.Done != sample.Done {
				t.Errorf("list envelope = %+v, want skip/limit/done from %+v", got, sample)
			}
			if len(got.Sessions) != 1 || got.Sessions[0].State != "idle" {
				t.Errorf("sessions = %+v, want one idle summary", got.Sessions)
			}
		})
	}
}

func TestServerHandleStatus(t *testing.T) {
	t.Parallel()

	const sidStr = "22222222-2222-2222-2222-222222222222"
	sid := parseTestUUID(t, sidStr)
	turnID := parseTestUUID(t, "33333333-3333-3333-3333-333333333333")

	statusFor := func(state string) SessionStatus {
		return SessionStatus{
			SessionID:      sid,
			State:          state,
			LastJournalSeq: 7,
			ActiveTurnID:   turnID,
			LastTurn:       &StatusEvent{JournalSeq: 7, Event: event.SessionStarted{}},
			LastStep:       &StatusEvent{JournalSeq: 6, Event: event.SessionStarted{}},
		}
	}

	tests := []struct {
		name       string
		sid        string
		status     SessionStatus
		statusErr  error
		wantStatus int
		wantState  string
	}{
		{name: "running happy", sid: sidStr, status: statusFor("running"), wantStatus: http.StatusOK, wantState: "running"},
		{name: "waiting happy", sid: sidStr, status: statusFor("waiting_on_gate"), wantStatus: http.StatusOK, wantState: "waiting_on_gate"},
		{name: "idle happy", sid: sidStr, status: statusFor("idle"), wantStatus: http.StatusOK, wantState: "idle"},
		{name: "stopped happy", sid: sidStr, status: statusFor("stopped"), wantStatus: http.StatusOK, wantState: "stopped"},
		{name: "malformed sid is 400", sid: "not-a-uuid", wantStatus: http.StatusBadRequest},
		{name: "not found is 404", sid: sidStr, statusErr: SessionNotFoundError{SessionID: sid}, wantStatus: http.StatusNotFound},
		{name: "store read error is 500", sid: sidStr, statusErr: StoreReadError{Op: "get", Cause: errBoom}, wantStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := &fakeReader{status: tt.status, statusErr: tt.statusErr}
			// NO live session in the registry: a read must succeed without one.
			srv := newServer[*fakeSession](&fakeRunner{}, reader, newConfig())

			req := readRequest("/v1/sessions/"+tt.sid+"/status", tt.sid)
			rec := httptest.NewRecorder()
			srv.handleStatus(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				assertErrorEnvelope(t, rec)
				return
			}

			// Decode into a shape that captures the raw event so we can assert the
			// custom StatusEvent MarshalJSON emitted the codec {journal_seq, event}
			// form (event = event.MarshalEvent output, type-tagged), not a struct dump.
			var got struct {
				SessionID      uuid.UUID `json:"session_id"`
				State          string    `json:"state"`
				LastJournalSeq uint64    `json:"last_journal_seq"`
				ActiveTurnID   uuid.UUID `json:"active_turn_id"`
				LastTurn       struct {
					JournalSeq uint64          `json:"journal_seq"`
					Event      json.RawMessage `json:"event"`
				} `json:"last_turn"`
				LastStep struct {
					JournalSeq uint64          `json:"journal_seq"`
					Event      json.RawMessage `json:"event"`
				} `json:"last_step"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode status body: %v", err)
			}
			if got.State != tt.wantState {
				t.Errorf("state = %q, want %q", got.State, tt.wantState)
			}
			if got.SessionID != sid || got.LastJournalSeq != 7 || got.ActiveTurnID != turnID {
				t.Errorf("projected fields wrong: %+v", got)
			}
			if got.LastTurn.JournalSeq != 7 {
				t.Errorf("last_turn.journal_seq = %d, want 7", got.LastTurn.JournalSeq)
			}
			assertEventTypeTag(t, got.LastTurn.Event, "SessionStarted")
			assertEventTypeTag(t, got.LastStep.Event, "SessionStarted")
		})
	}
}

func TestServerHandleJournal(t *testing.T) {
	t.Parallel()

	const sidStr = "44444444-4444-4444-4444-444444444444"
	sid := parseTestUUID(t, sidStr)

	sample := EventJournalPage{
		Events:         []StatusEvent{{JournalSeq: 3, Event: event.SessionStarted{}}},
		NextJournalSeq: 4,
		Done:           false,
	}

	tests := []struct {
		name       string
		sid        string
		query      string
		journalErr error
		wantStatus int
		wantFrom   uint64
		wantLimit  int
	}{
		{name: "absent params use defaults", sid: sidStr, query: "", wantStatus: http.StatusOK, wantFrom: 0, wantLimit: 100},
		{name: "explicit from and limit", sid: sidStr, query: "?from_journal_seq=10&limit=20", wantStatus: http.StatusOK, wantFrom: 10, wantLimit: 20},
		{name: "malformed sid is 400", sid: "not-a-uuid", query: "", wantStatus: http.StatusBadRequest},
		{name: "negative from is 400", sid: sidStr, query: "?from_journal_seq=-1", wantStatus: http.StatusBadRequest},
		{name: "non-numeric from is 400", sid: sidStr, query: "?from_journal_seq=x", wantStatus: http.StatusBadRequest},
		{name: "limit over cap is 400", sid: sidStr, query: "?limit=1001", wantStatus: http.StatusBadRequest},
		{name: "store read error is 500", sid: sidStr, query: "", journalErr: StoreReadError{Op: "replay", Cause: errBoom}, wantStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reader := &fakeReader{journal: sample, journalErr: tt.journalErr}
			// NO live session in the registry: a journal read must succeed without one.
			srv := newServer[*fakeSession](&fakeRunner{}, reader, newConfig())

			req := readRequest("/v1/sessions/"+tt.sid+"/journal"+tt.query, tt.sid)
			rec := httptest.NewRecorder()
			srv.handleJournal(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				assertErrorEnvelope(t, rec)
				return
			}
			if reader.gotJournalID != sid {
				t.Errorf("journal id = %v, want %v", reader.gotJournalID, sid)
			}
			if reader.gotJournal.From != tt.wantFrom || reader.gotJournal.Limit != tt.wantLimit {
				t.Errorf("journal page = %+v, want {From:%d Limit:%d}", reader.gotJournal, tt.wantFrom, tt.wantLimit)
			}
			var got struct {
				Events []struct {
					JournalSeq uint64          `json:"journal_seq"`
					Event      json.RawMessage `json:"event"`
				} `json:"events"`
				NextJournalSeq uint64 `json:"next_journal_seq"`
				Done           bool   `json:"done"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode journal body: %v", err)
			}
			if got.NextJournalSeq != 4 || got.Done {
				t.Errorf("cursor = {next:%d done:%v}, want {4 false}", got.NextJournalSeq, got.Done)
			}
			if len(got.Events) != 1 || got.Events[0].JournalSeq != 3 {
				t.Fatalf("events = %+v, want one at seq 3", got.Events)
			}
			assertEventTypeTag(t, got.Events[0].Event, "SessionStarted")
		})
	}
}

// assertEventTypeTag verifies raw is the durable event envelope (event.MarshalEvent
// output) carrying the expected "type" discriminator — proving StatusEvent serialized
// via the codec, not a Go-struct dump of the interface.
func assertEventTypeTag(t *testing.T, raw json.RawMessage, wantType string) {
	t.Helper()
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("decode event envelope: %v (raw %s)", err, raw)
	}
	if probe.Type != wantType {
		t.Errorf("event type = %q, want %q (raw %s)", probe.Type, wantType, raw)
	}
}
