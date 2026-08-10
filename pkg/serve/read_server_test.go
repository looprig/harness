package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/looprig/core/uuid"
)

// stubReader is a Reader that returns canned values, so the read handlers can be
// exercised with no store, no rig, and no live session.
type stubReader struct {
	list    SessionList
	status  SessionStatus
	journal EventJournalPage
	err     error
}

func (s stubReader) ListSessions(context.Context, Page) (SessionList, error) {
	return s.list, s.err
}

func (s stubReader) ReadStatus(context.Context, uuid.UUID) (SessionStatus, error) {
	return s.status, s.err
}

func (s stubReader) ReadJournal(context.Context, uuid.UUID, JournalPage) (EventJournalPage, error) {
	return s.journal, s.err
}

// TestReadServerServesListWithoutRig proves the read plane is reachable from a
// receiver that holds NO session factory — the property the BFF depends on.
func TestReadServerServesListWithoutRig(t *testing.T) {
	t.Parallel()

	rs := &readServer{reader: stubReader{list: SessionList{Done: true}}}

	rec := httptest.NewRecorder()
	rs.handleListSessions(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got SessionList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !got.Done {
		t.Errorf("Done = false, want true")
	}
}

// TestReadServerCapabilitiesAdvertiseJournalOnly proves a read-only server does not
// claim live/control planes it cannot serve. A client that trusts the document and
// then opens an SSE stream against a read-only host would hang; honest advertisement
// is the contract that prevents it.
func TestReadServerCapabilitiesAdvertiseJournalOnly(t *testing.T) {
	t.Parallel()

	rs := &readServer{reader: stubReader{}, features: readOnlyFeatures}

	rec := httptest.NewRecorder()
	rs.handleCapabilities(rec, httptest.NewRequest(http.MethodGet, "/v1/capabilities", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got capabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.Protocol != protocolName || got.Version != protocolVersion {
		t.Errorf("protocol/version = %q/%d, want %q/%d", got.Protocol, got.Version, protocolName, protocolVersion)
	}
	if len(got.Features) != 1 || got.Features[0] != featureJournal {
		t.Errorf("features = %v, want exactly [%q]", got.Features, featureJournal)
	}
}

// TestReadHandlerRoutes proves ReadHandler serves exactly the three stateless read
// routes plus capabilities, and that every live/control route is absent. The absence
// assertions are the security-relevant half: a browse-only deployment must not expose
// a control surface at all, rather than exposing one that 403s.
func TestReadHandlerRoutes(t *testing.T) {
	t.Parallel()

	sid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	h := ReadHandler(stubReader{status: SessionStatus{SessionID: sid}})

	tests := []struct {
		name       string
		method     string
		target     string
		wantAbsent bool
	}{
		{name: "capabilities", method: http.MethodGet, target: "/v1/capabilities"},
		{name: "list", method: http.MethodGet, target: "/v1/sessions"},
		{name: "status", method: http.MethodGet, target: "/v1/sessions/" + sid.String() + "/status"},
		{name: "journal", method: http.MethodGet, target: "/v1/sessions/" + sid.String() + "/journal"},

		{name: "create absent", method: http.MethodPost, target: "/v1/sessions", wantAbsent: true},
		{name: "events absent", method: http.MethodGet, target: "/v1/sessions/" + sid.String() + "/events", wantAbsent: true},
		{name: "input absent", method: http.MethodPost, target: "/v1/sessions/" + sid.String() + "/input", wantAbsent: true},
		{name: "interrupt absent", method: http.MethodPost, target: "/v1/sessions/" + sid.String() + "/interrupt", wantAbsent: true},
		{name: "restore absent", method: http.MethodPost, target: "/v1/sessions/" + sid.String() + "/restore", wantAbsent: true},
		{name: "gate absent", method: http.MethodPost, target: "/v1/sessions/" + sid.String() + "/gates/" + sid.String(), wantAbsent: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.target, http.NoBody))

			switch {
			case tt.wantAbsent && rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed:
				t.Errorf("%s %s = %d, want 404/405 (route must not be registered)", tt.method, tt.target, rec.Code)
			case !tt.wantAbsent && rec.Code != http.StatusOK:
				t.Errorf("%s %s = %d, want %d", tt.method, tt.target, rec.Code, http.StatusOK)
			}
		})
	}
}

// TestReadHandlerRespectsOptions proves ReadHandler shares the Handler middleware
// chain, so an authenticator installed by the caller actually runs.
func TestReadHandlerRespectsOptions(t *testing.T) {
	t.Parallel()

	denied := errors.New("denied")
	h := ReadHandler(stubReader{}, WithAuth(func(*http.Request) error { return denied }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions", http.NoBody))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
