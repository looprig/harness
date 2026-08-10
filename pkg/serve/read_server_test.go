package serve

import (
	"context"
	"encoding/json"
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
