package serve

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// update regenerates the golden wire fixtures instead of comparing against them:
//
//	go test ./pkg/serve -run Fixture -update
//
// This is the standard Go golden-file idiom. A fixture under testdata/fixtures IS
// the reviewed wire contract — a diff there is a breaking-change signal, so
// regenerate deliberately and review the diff (see testdata/README.md).
var update = flag.Bool("update", false, "regenerate golden wire fixtures")

// fixturesDir is the directory the golden wire fixtures live in.
const fixturesDir = "testdata/fixtures"

// Deterministic identifiers and instant fed to the handlers so the emitted bytes
// carry stable values. They are ALSO run through the normalizer below (belt and
// suspenders): every uuid collapses to the zero uuid and every RFC3339 timestamp to
// fixedTimestamp, so a fixture documents the wire SHAPE, not a volatile identity.
const (
	fixSessionID = "11111111-1111-1111-1111-111111111111"
	fixCommandID = "22222222-2222-2222-2222-222222222222"
	fixTurnID    = "33333333-3333-3333-3333-333333333333"
	fixGateID    = "88888888-8888-8888-8888-888888888888"
	fixEventID   = "55555555-5555-5555-5555-555555555555"
)

// fixedInstant is the single timestamp every timestamp-bearing fixture is fed. It
// is a whole-second UTC instant so encoding/json renders it as a plain RFC3339
// "2026-07-08T12:00:00Z" (no sub-second digits), which the normalizer pins.
var fixedInstant = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

// Normalization: FIXED VALUES are fed from the fakes/inputs (the primary strategy),
// and these regexps are a deterministic backstop so any volatile value that ever
// leaks into a body is pinned before compare/write. uuidRE matches a canonical
// 8-4-4-4-12 uuid; tsRE matches an RFC3339 timestamp (optional sub-second, Z or
// numeric offset). Both replacements are idempotent — the zero uuid and the fixed
// timestamp each re-match their own regexp and map to themselves — so `-update`
// round-trips cleanly.
var (
	uuidRE = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	tsRE   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
)

const (
	zeroUUID       = "00000000-0000-0000-0000-000000000000"
	fixedTimestamp = "2026-07-08T12:00:00Z"
)

// normalize pins every volatile value in an emitted body to a stable placeholder:
// uuids to the zero uuid, RFC3339 timestamps to fixedTimestamp. It is applied to
// both the freshly-emitted bytes (before compare) and the bytes written under
// -update, so the golden fixture and the live comparison are normalized identically.
func normalize(b []byte) []byte {
	b = tsRE.ReplaceAll(b, []byte(fixedTimestamp))
	b = uuidRE.ReplaceAll(b, []byte(zeroUUID))
	return b
}

// fixedHeader stamps a deterministic producer identity (session id + event id +
// creation instant) onto an event so an SSE frame fixture exercises the normalizer's
// uuid and timestamp paths rather than emitting an empty header.
func fixedHeader(t *testing.T) event.Header {
	t.Helper()
	return event.Header{
		Coordinates: identity.Coordinates{SessionID: parseTestUUID(t, fixSessionID)},
		EventID:     parseTestUUID(t, fixEventID),
		CreatedAt:   fixedInstant,
	}
}

// TestFixtures drives each handler / encoder with deterministic inputs, normalizes
// the emitted bytes, and asserts byte-equality against the committed golden fixture
// (or rewrites the fixture under -update). One table row per response type keeps the
// wire contract enumerated in a single place.
func TestFixtures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		file    string
		produce func(t *testing.T) []byte
	}{
		{name: "capabilities", file: "capabilities.json", produce: produceCapabilities},
		{name: "create idle", file: "create_idle.json", produce: produceCreateIdle},
		{name: "create with command", file: "create_with_command.json", produce: produceCreateWithCommand},
		{name: "session list", file: "session_list.json", produce: produceSessionList},
		{name: "restore", file: "restore.json", produce: produceRestore},
		{name: "input", file: "input.json", produce: produceInput},
		{name: "interrupt", file: "interrupt.json", produce: produceInterrupt},
		{name: "gate accepted", file: "gate_accepted.json", produce: produceGateAccepted},
		{name: "status running", file: "status_running.json", produce: produceStatusRunning},
		{name: "journal page", file: "journal_page.json", produce: produceJournalPage},
		{name: "error 400", file: "error_400.json", produce: produceError(http.StatusBadRequest, codeInvalidBody, msgInvalidBody, false)},
		{name: "error 404", file: "error_404.json", produce: produceError(http.StatusNotFound, codeNotFound, msgNotFound, false)},
		{name: "error 409", file: "error_409.json", produce: produceError(http.StatusConflict, codeIdempotencyConflict, msgIdempotencyConflict, false)},
		{name: "error 500", file: "error_500.json", produce: produceError(http.StatusInternalServerError, codeInternal, msgCreateFailed, false)},
		{name: "error 503", file: "error_503.json", produce: produceError(http.StatusServiceUnavailable, codeGateCapacity, msgGateCapacity, true)},
		{name: "sse enduring frame", file: "enduring_frame.sse", produce: produceEnduringFrame},
		{name: "sse ephemeral token_delta frame", file: "ephemeral_token_delta.sse", produce: produceEphemeralFrame},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := normalize(tt.produce(t))
			path := filepath.Join(fixturesDir, tt.file)

			if *update {
				if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", fixturesDir, err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write fixture %s: %v", path, err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture %s: %v (run with -update to generate)", path, err)
			}
			if string(got) != string(want) {
				t.Errorf("wire body for %s drifted from the golden fixture.\n got: %s\nwant: %s\n(if this change is intended, regenerate with -update and review the diff)", tt.file, got, want)
			}
		})
	}
}

// --- producers: each drives one handler/encoder with deterministic inputs and
// returns the RAW emitted bytes (normalization is applied by the caller). ---

func produceCapabilities(t *testing.T) []byte {
	t.Helper()
	srv := newServer[*fakeSession, fakeSessionOption](nil, nil, newConfig())
	rec := httptest.NewRecorder()
	srv.handleCapabilities(rec, httptest.NewRequest(http.MethodGet, "/v1/capabilities", http.NoBody))
	return rec.Body.Bytes()
}

func produceCreateIdle(t *testing.T) []byte {
	t.Helper()
	rig := &fakeRig{runID: parseTestUUID(t, fixSessionID), runSess: &fakeSession{}}
	srv := newServer[*fakeSession, fakeSessionOption](rig, nil, newConfig())
	rec := httptest.NewRecorder()
	srv.handleCreate(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions", http.NoBody))
	return rec.Body.Bytes()
}

func produceCreateWithCommand(t *testing.T) []byte {
	t.Helper()
	sess := &fakeSession{submitID: parseTestUUID(t, fixCommandID)}
	rig := &fakeRig{runID: parseTestUUID(t, fixSessionID), runSess: sess}
	srv := newServer[*fakeSession, fakeSessionOption](rig, nil, newConfig())
	rec := httptest.NewRecorder()
	srv.handleCreate(rec, httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(validBlocksBody)))
	return rec.Body.Bytes()
}

func produceSessionList(t *testing.T) []byte {
	t.Helper()
	reader := &fakeReader{list: SessionList{
		Sessions: []SessionSummary{{
			SessionID:    parseTestUUID(t, fixSessionID),
			State:        "idle",
			Title:        "demo session",
			CreatedAt:    fixedInstant,
			LastActiveAt: fixedInstant,
		}},
		Skip:     0,
		Limit:    100,
		NextSkip: 0,
		Done:     true,
	}}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, reader, newConfig())
	rec := httptest.NewRecorder()
	srv.handleListSessions(rec, readRequest("/v1/sessions", ""))
	return rec.Body.Bytes()
}

func produceRestore(t *testing.T) []byte {
	t.Helper()
	rig := &fakeRig{restoreSess: &fakeSession{}}
	srv := newServer[*fakeSession, fakeSessionOption](rig, nil, newConfig())
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+fixSessionID+"/restore", http.NoBody)
	req.SetPathValue("sid", fixSessionID)
	rec := httptest.NewRecorder()
	srv.handleRestore(rec, req)
	return rec.Body.Bytes()
}

func produceInput(t *testing.T) []byte {
	t.Helper()
	sess := &fakeSession{submitID: parseTestUUID(t, fixCommandID)}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, newConfig())
	srv.registry.put(parseTestUUID(t, fixSessionID), sess)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+fixSessionID+"/input", strings.NewReader(validBlocksBody))
	req.SetPathValue("sid", fixSessionID)
	rec := httptest.NewRecorder()
	srv.handleInput(rec, req)
	return rec.Body.Bytes()
}

func produceInterrupt(t *testing.T) []byte {
	t.Helper()
	sess := &fakeSession{interruptResult: true}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, newConfig())
	srv.registry.put(parseTestUUID(t, fixSessionID), sess)
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+fixSessionID+"/interrupt", http.NoBody)
	req.SetPathValue("sid", fixSessionID)
	rec := httptest.NewRecorder()
	srv.handleInterrupt(rec, req)
	return rec.Body.Bytes()
}

func produceGateAccepted(t *testing.T) []byte {
	t.Helper()
	sess := &fakeSession{}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, nil, newConfig())
	srv.registry.put(parseTestUUID(t, fixSessionID), sess)
	rec := httptest.NewRecorder()
	srv.handleGateResponse(rec, gateRequest(fixSessionID, fixGateID, `{"action":"approve","values":{"scope":"session"}}`, true))
	return rec.Body.Bytes()
}

func produceStatusRunning(t *testing.T) []byte {
	t.Helper()
	status := SessionStatus{
		SessionID:      parseTestUUID(t, fixSessionID),
		State:          "running",
		LastJournalSeq: 7,
		ActiveTurnID:   parseTestUUID(t, fixTurnID),
		LastTurn:       &StatusEvent{JournalSeq: 7, Event: event.TurnDone{TurnIndex: 1}},
		LastStep:       &StatusEvent{JournalSeq: 6, Event: event.StepDone{}},
		UpdatedAt:      fixedInstant,
	}
	reader := &fakeReader{status: status}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, reader, newConfig())
	rec := httptest.NewRecorder()
	srv.handleStatus(rec, readRequest("/v1/sessions/"+fixSessionID+"/status", fixSessionID))
	return rec.Body.Bytes()
}

func produceJournalPage(t *testing.T) []byte {
	t.Helper()
	page := EventJournalPage{
		Events:         []StatusEvent{{JournalSeq: 3, Event: event.TurnDone{TurnIndex: 1}}},
		NextJournalSeq: 4,
		Done:           false,
	}
	reader := &fakeReader{journal: page}
	srv := newServer[*fakeSession, fakeSessionOption](&fakeRig{}, reader, newConfig())
	rec := httptest.NewRecorder()
	srv.handleJournal(rec, readRequest("/v1/sessions/"+fixSessionID+"/journal", fixSessionID))
	return rec.Body.Bytes()
}

// produceError returns a producer that emits the nested error envelope for one
// (status, code, message, retryable) tuple — the canonical body for that status.
func produceError(status int, code, message string, retryable bool) func(t *testing.T) []byte {
	return func(t *testing.T) []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		writeError(rec, status, code, message, retryable)
		return rec.Body.Bytes()
	}
}

func produceEnduringFrame(t *testing.T) []byte {
	t.Helper()
	ev := event.TurnDone{Header: fixedHeader(t), TurnIndex: 1}
	frame, ok := encodeDelivery(event.Delivery{Event: ev, JournalSeq: 42})
	if !ok {
		t.Fatal("encodeDelivery skipped the enduring delivery")
	}
	return frame
}

func produceEphemeralFrame(t *testing.T) []byte {
	t.Helper()
	ev := event.TokenDelta{Header: fixedHeader(t), TurnIndex: 1, Chunk: &content.TextChunk{Text: "hello"}}
	frame, ok := encodeDelivery(event.Delivery{Event: ev})
	if !ok {
		t.Fatal("encodeDelivery skipped the ephemeral delivery")
	}
	return frame
}
