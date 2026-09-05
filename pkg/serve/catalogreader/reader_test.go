package catalogreader_test

// These are in-process tests over a REAL memstore-backed sessionstore.Store +
// Catalog. Because memstore is an in-process reference backend (no process boundary,
// no filesystem, no network), they are ordinary unit tests — NOT integration-tagged.
// CLAUDE.md's integration-tag rule targets code that crosses a process boundary; an
// in-memory store does not, so the fast default `go test` covers this adapter.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	//lint:ignore SA1019 These tests pin the retained serve compatibility contract.
	"github.com/looprig/harness/pkg/serve"
	"github.com/looprig/harness/pkg/serve/catalogreader"
	"github.com/looprig/harness/pkg/sessionstore"
	harnesssessionwire "github.com/looprig/harness/pkg/sessionwire"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/storage/memstore"
)

// mutClock is a mutable CatalogClock backing so a test can stamp a distinct
// LastActiveAt per session (the field the list read sorts on).
type mutClock struct{ t time.Time }

func (m *mutClock) now() time.Time { return m.t }

// fixedUUID builds a deterministic non-zero uuid from a seed byte; ids sort by seed.
func fixedUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

func aiMsg(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

func userMsg(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// sessionStarted builds a session-scoped SessionStarted for sid.
func sessionStarted(sid uuid.UUID) event.SessionStarted {
	return event.SessionStarted{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid},
		EventID:     fixedUUID(0xA0),
	}}
}

// turnStarted builds a loop-scoped TurnStarted for sid/loop/turn.
func turnStarted(sid, loop, turn uuid.UUID) event.TurnStarted {
	return event.TurnStarted{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn}, EventID: fixedUUID(0xA1)},
		TurnIndex: 1,
		Message:   userMsg("hello"),
	}
}

// turnDone builds a valid loop-scoped TurnDone (reconstructed as LastTurn).
// eventID must be distinct across every TurnDone appended to the same
// journal: a fixed, shared EventID across two different TurnDone events
// (different TurnID/content) is a genuine idempotency collision under
// journal's fingerprint-based dedup (pkg/journal), not a legitimate retry.
func turnDone(sid, loop, turn, eventID uuid.UUID) event.TurnDone {
	return event.TurnDone{
		Header:    event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn}, EventID: eventID},
		TurnIndex: 1,
		Message:   aiMsg("done"),
	}
}

// stepDone builds a valid step-scoped StepDone (reconstructed as LastStep).
func stepDone(sid, loop, turn, step uuid.UUID) event.StepDone {
	return event.StepDone{
		Header:   event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn, StepID: step}, EventID: fixedUUID(0xA3)},
		Messages: content.AgenticMessages{aiMsg("step")},
	}
}

// gateOpened builds a loop-scoped GateOpened carrying gate gid.
func gateOpened(sid, loop uuid.UUID, gid gate.ID) event.GateOpened {
	return event.GateOpened{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop}, EventID: fixedUUID(0xA4)},
		Gate:   gate.Gate{ID: gid, Kind: gate.KindPermission, Resolver: gate.ResolverLoop},
	}
}

func sessionStopped(sid uuid.UUID) event.SessionStopped {
	return event.SessionStopped{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid},
		EventID:     fixedUUID(0xA5),
	}}
}

// permissionReviewCompleted builds a valid, Internal-visibility
// PermissionReviewCompleted for sid — Task 17's first real producer of this
// event type. Used to prove ReadJournal filters it out of a public journal
// page exactly like GatePrepared/HustleStarted already are.
func permissionReviewCompleted(sid uuid.UUID) event.PermissionReviewCompleted {
	return event.PermissionReviewCompleted{
		Header: event.Header{
			Coordinates: identity.Coordinates{
				SessionID: sid, LoopID: fixedUUID(0xC3), TurnID: fixedUUID(0xC4), StepID: fixedUUID(0xC5),
			},
			EventID:         fixedUUID(0xC0),
			EventVisibility: event.Internal,
		},
		GateID:             gate.ID(fixedUUID(0xC1)),
		ToolExecutionID:    fixedUUID(0xC2),
		Classifier:         "command-safety",
		ClassifierRevision: "classifier-rev-1",
		Status:             gate.ReviewStatusNotApplicable,
	}
}

func hustleStarted(t *testing.T, sid uuid.UUID) event.HustleStarted {
	t.Helper()
	definition, err := hustle.Define(
		hustle.WithName("private.journal"),
		hustle.WithParticipation(hustle.ParticipationBackground),
		hustle.WithTimeout(time.Second),
		hustle.WithLimits(hustle.Limits{InputBytes: 1, OutputBytes: 1}),
		hustle.WithCurrentLoopModel(),
		hustle.WithSystemPrompt("private", "prompt-v1"),
		hustle.WithPolicyRevision("policy-v1"),
	)
	if err != nil {
		t.Fatalf("hustle.Define() error = %v", err)
	}
	return event.HustleStarted{
		Header: event.Header{
			Coordinates:     identity.Coordinates{SessionID: sid},
			EventID:         fixedUUID(0xB0),
			EventVisibility: event.Internal,
		},
		Run: event.HustleRunDescriptor{Definition: definition.Descriptor(), RunID: hustle.RunID(fixedUUID(0xB1))},
	}
}

// newCatalog opens a memstore Store + Catalog with a mutable clock.
func newCatalog(t *testing.T) (*sessionstore.Store, *sessionstore.Catalog, *mutClock) {
	t.Helper()
	st, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	clk := &mutClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cat := st.OpenCatalog(sessionstore.WithCatalogClock(clk.now))
	return st, cat, clk
}

// newStoredCatalogReader writes the supplied historical SessionMeta JSON through
// the real memstore KV boundary. This deliberately exercises SessionMeta's
// supported decoder rather than constructing a synthetic in-memory meta value.
func newStoredCatalogReader(t *testing.T, sid uuid.UUID, raw []byte) *catalogreader.Reader {
	t.Helper()
	return newStoredCatalogRowsReader(t, storedCatalogRow{sid: sid, raw: raw})
}

type storedCatalogRow struct {
	sid uuid.UUID
	raw []byte
}

func newStoredCatalogRowsReader(t *testing.T, rows ...storedCatalogRow) *catalogreader.Reader {
	t.Helper()
	backend := memstore.New()
	st, err := sessionstore.Open(backend)
	if err != nil {
		t.Fatalf("Open() err = %v", err)
	}
	for _, row := range rows {
		if _, err := backend.KV.Put(context.Background(), "sessions/"+row.sid.String(), 0, row.raw); err != nil {
			t.Fatalf("KV.Put(historical SessionMeta) err = %v", err)
		}
	}
	return catalogreader.NewScoped(
		st.OpenCatalog(),
		st,
		harnesssessionwire.ReadAuthority{TenantID: "tenant-a", AgentID: "fixture-agent"},
		coresessionwire.SessionResidencyCold,
	)
}

// update folds ev into the catalog at seq, failing the test on error.
func update(t *testing.T, cat *sessionstore.Catalog, ev event.Event, seq uint64) {
	t.Helper()
	if err := cat.UpdateOnEvent(context.Background(), ev, seq); err != nil {
		t.Fatalf("UpdateOnEvent(%T) err = %v", ev, err)
	}
}

func TestReaderListSessions(t *testing.T) {
	t.Parallel()

	// Three sessions with strictly increasing LastActiveAt; expected sort is
	// most-recent-first (C, B, A), tie-broken by session id ascending.
	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	idA, idB, idC := fixedUUID(0x11), fixedUUID(0x22), fixedUUID(0x33)

	seed := func(t *testing.T, cat *sessionstore.Catalog, clk *mutClock, sid uuid.UUID, active time.Time, seq uint64) {
		update(t, cat, sessionStarted(sid), seq)
		clk.t = active
		update(t, cat, turnStarted(sid, fixedUUID(0x01), fixedUUID(0x02)), seq+1)
	}

	tests := []struct {
		name     string
		empty    bool
		page     serve.Page
		wantIDs  []uuid.UUID
		wantSkip int
		wantNext int
		wantDone bool
	}{
		{name: "empty catalog", empty: true, page: serve.Page{Skip: 0, Limit: 100}, wantIDs: nil, wantSkip: 0, wantNext: 0, wantDone: true},
		{name: "default window sorted desc", page: serve.Page{Skip: 0, Limit: 100}, wantIDs: []uuid.UUID{idC, idB, idA}, wantSkip: 0, wantNext: 0, wantDone: true},
		{name: "limit smaller than total not done", page: serve.Page{Skip: 0, Limit: 2}, wantIDs: []uuid.UUID{idC, idB}, wantSkip: 0, wantNext: 2, wantDone: false},
		{name: "skip into middle", page: serve.Page{Skip: 1, Limit: 100}, wantIDs: []uuid.UUID{idB, idA}, wantSkip: 1, wantNext: 0, wantDone: true},
		{name: "skip past end is empty done", page: serve.Page{Skip: 10, Limit: 100}, wantIDs: nil, wantSkip: 10, wantNext: 0, wantDone: true},
		{name: "exact page boundary not done", page: serve.Page{Skip: 0, Limit: 3}, wantIDs: []uuid.UUID{idC, idB, idA}, wantSkip: 0, wantNext: 3, wantDone: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, cat, clk := newCatalog(t)
			if !tt.empty {
				seed(t, cat, clk, idA, base.Add(1*time.Hour), 10)
				seed(t, cat, clk, idB, base.Add(2*time.Hour), 20)
				seed(t, cat, clk, idC, base.Add(3*time.Hour), 30)
			}
			r := catalogreader.New(cat, st)

			got, err := r.ListSessions(context.Background(), tt.page)
			if err != nil {
				t.Fatalf("ListSessions() err = %v", err)
			}
			if len(got.Sessions) != len(tt.wantIDs) {
				t.Fatalf("returned %d sessions, want %d (%+v)", len(got.Sessions), len(tt.wantIDs), got.Sessions)
			}
			for i, want := range tt.wantIDs {
				if got.Sessions[i].SessionID != want {
					t.Errorf("session[%d] id = %v, want %v", i, got.Sessions[i].SessionID, want)
				}
			}
			if got.Skip != tt.wantSkip || got.NextSkip != tt.wantNext || got.Done != tt.wantDone {
				t.Errorf("paging = {skip:%d next:%d done:%v}, want {skip:%d next:%d done:%v}",
					got.Skip, got.NextSkip, got.Done, tt.wantSkip, tt.wantNext, tt.wantDone)
			}
			if got.Limit != tt.page.Limit {
				t.Errorf("limit = %d, want %d", got.Limit, tt.page.Limit)
			}
		})
	}
}

func TestReaderLegacyJSONParityThroughCoreProjection(t *testing.T) {
	t.Parallel()

	st, cat, clk := newCatalog(t)
	sid := fixedUUID(0x11)
	loop, turn := fixedUUID(0x12), fixedUUID(0x13)
	created := time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)
	active := time.Date(2026, 8, 29, 9, 45, 0, 0, time.UTC)
	started := sessionStarted(sid)
	started.CreatedAt = created
	started.Config.AgentKind = "fixture-agent"
	update(t, cat, started, 1)
	clk.t = active
	update(t, cat, turnStarted(sid, loop, turn), 2)

	r := catalogreader.NewScoped(
		cat,
		st,
		harnesssessionwire.ReadAuthority{TenantID: coresessionwire.TenantID("tenant-a"), AgentID: coresessionwire.AgentID("fixture-agent")},
		coresessionwire.SessionResidencyCold,
	)
	list, err := r.ListSessions(context.Background(), serve.Page{Skip: 0, Limit: 100})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	status, err := r.ReadStatus(context.Background(), sid)
	if err != nil {
		t.Fatalf("ReadStatus() error = %v", err)
	}

	listJSON, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("json.Marshal(list) error = %v", err)
	}
	statusJSON, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("json.Marshal(status) error = %v", err)
	}
	// These bytes are independently specified legacy serve DTO goldens. They are
	// deliberately not produced by marshaling Core records into the oracle.
	wantList := []byte(`{"sessions":[{"session_id":"11111111-1111-1111-1111-111111111111","state":"running","title":"hello","created_at":"2026-08-29T09:30:00Z","last_active_at":"2026-08-29T09:45:00Z"}],"skip":0,"limit":100,"next_skip":0,"done":true}`)
	wantStatus := []byte(`{"session_id":"11111111-1111-1111-1111-111111111111","state":"running","last_journal_seq":2,"active_turn_id":"13131313-1313-1313-1313-131313131313","updated_at":"2026-08-29T09:45:00Z"}`)
	if !bytes.Equal(listJSON, wantList) {
		t.Errorf("legacy list JSON changed\n got: %s\nwant: %s", listJSON, wantList)
	}
	if !bytes.Equal(statusJSON, wantStatus) {
		t.Errorf("legacy status JSON changed\n got: %s\nwant: %s", statusJSON, wantStatus)
	}
}

func TestReaderListSessionsPreservesSupportedPreProjectionRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sid  uuid.UUID
		raw  []byte
		want []byte
	}{
		{
			name: "legacy empty state and absent timestamps",
			sid:  fixedUUID(0x61),
			raw:  []byte(`{"session_id":"61616161-6161-6161-6161-616161616161","title":"legacy","status":"active","last_journal_seq":7}`),
			want: []byte(`{"sessions":[{"session_id":"61616161-6161-6161-6161-616161616161","title":"legacy"}],"skip":0,"limit":10,"next_skip":0,"done":true}`),
		},
		{
			name: "current state with absent activity timestamps",
			sid:  fixedUUID(0x62),
			raw:  []byte(`{"session_id":"62626262-6262-6262-6262-626262626262","title":"current","status":"active","state":"idle","last_journal_seq":8}`),
			want: []byte(`{"sessions":[{"session_id":"62626262-6262-6262-6262-626262626262","state":"idle","title":"current"}],"skip":0,"limit":10,"next_skip":0,"done":true}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := newStoredCatalogReader(t, test.sid, test.raw)
			got, err := r.ListSessions(context.Background(), serve.Page{Limit: 10})
			if err != nil {
				t.Fatalf("ListSessions() error = %T %v, want nil", err, err)
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("json.Marshal(ListSessions()) error = %v", err)
			}
			if !bytes.Equal(gotJSON, test.want) {
				t.Errorf("legacy list JSON changed\n got: %s\nwant: %s", gotJSON, test.want)
			}
		})
	}
}

func TestReaderListSessionsPreservesCreatedOnlyLegacyOrdering(t *testing.T) {
	t.Parallel()

	olderID, newerID := fixedUUID(0x11), fixedUUID(0x22)
	r := newStoredCatalogRowsReader(t,
		storedCatalogRow{
			sid: olderID,
			raw: []byte(`{"session_id":"11111111-1111-1111-1111-111111111111","state":"idle","title":"older","created_at":"2026-08-29T09:00:00Z","last_journal_seq":1}`),
		},
		storedCatalogRow{
			sid: newerID,
			raw: []byte(`{"session_id":"22222222-2222-2222-2222-222222222222","state":"idle","title":"newer","created_at":"2026-08-29T10:00:00Z","last_journal_seq":1}`),
		},
	)
	got, err := r.ListSessions(context.Background(), serve.Page{Limit: 10})
	if err != nil {
		t.Fatalf("ListSessions() error = %T %v, want nil", err, err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal(ListSessions()) error = %v", err)
	}
	want := []byte(`{"sessions":[{"session_id":"11111111-1111-1111-1111-111111111111","state":"idle","title":"older","created_at":"2026-08-29T09:00:00Z"},{"session_id":"22222222-2222-2222-2222-222222222222","state":"idle","title":"newer","created_at":"2026-08-29T10:00:00Z"}],"skip":0,"limit":10,"next_skip":0,"done":true}`)
	if !bytes.Equal(gotJSON, want) {
		t.Errorf("legacy list JSON changed\n got: %s\nwant: %s", gotJSON, want)
	}
}

func TestReaderReadStatusPreservesSupportedPreProjectionRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sid  uuid.UUID
		raw  []byte
		want []byte
	}{
		{
			name: "legacy empty state and absent timestamps",
			sid:  fixedUUID(0x63),
			raw:  []byte(`{"session_id":"63636363-6363-6363-6363-636363636363","status":"active","last_journal_seq":9}`),
			want: []byte(`{"session_id":"63636363-6363-6363-6363-636363636363","last_journal_seq":9}`),
		},
		{
			name: "current state with absent activity timestamps",
			sid:  fixedUUID(0x64),
			raw:  []byte(`{"session_id":"64646464-6464-6464-6464-646464646464","status":"active","state":"idle","last_journal_seq":10}`),
			want: []byte(`{"session_id":"64646464-6464-6464-6464-646464646464","state":"idle","last_journal_seq":10}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := newStoredCatalogReader(t, test.sid, test.raw)
			got, err := r.ReadStatus(context.Background(), test.sid)
			if err != nil {
				t.Fatalf("ReadStatus() error = %T %v, want nil", err, err)
			}
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("json.Marshal(ReadStatus()) error = %v", err)
			}
			if !bytes.Equal(gotJSON, test.want) {
				t.Errorf("legacy status JSON changed\n got: %s\nwant: %s", gotJSON, test.want)
			}
		})
	}
}

type storedEventSummary struct {
	JournalSeq uint64          `json:"journal_seq"`
	Event      json.RawMessage `json:"event"`
}

func storedStatusMetaJSON(t *testing.T, sid uuid.UUID, lastTurn, lastStep *storedEventSummary) []byte {
	t.Helper()
	return storedStatusMetaJSONAtTip(t, sid, 7, lastTurn, lastStep)
}

func storedStatusMetaJSONAtTip(t *testing.T, sid uuid.UUID, tip uint64, lastTurn, lastStep *storedEventSummary) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		SessionID      uuid.UUID           `json:"session_id"`
		State          string              `json:"state"`
		LastJournalSeq uint64              `json:"last_journal_seq"`
		LastTurn       *storedEventSummary `json:"last_turn,omitempty"`
		LastStep       *storedEventSummary `json:"last_step,omitempty"`
	}{SessionID: sid, State: "idle", LastJournalSeq: tip, LastTurn: lastTurn, LastStep: lastStep})
	if err != nil {
		t.Fatalf("json.Marshal(persisted SessionMeta) error = %v", err)
	}
	return raw
}

func persistedStatusEvent(t *testing.T, value event.Event) json.RawMessage {
	t.Helper()
	raw, err := event.MarshalEvent(value)
	if err != nil {
		t.Fatalf("event.MarshalEvent(%T) error = %v", value, err)
	}
	return raw
}

func TestReaderReadStatusRejectsForeignAndZeroSummarySessions(t *testing.T) {
	t.Parallel()

	requested, foreign := fixedUUID(0x73), fixedUUID(0x74)
	loop, turn, step := fixedUUID(0x75), fixedUUID(0x76), fixedUUID(0x77)
	turnWire := persistedStatusEvent(t, event.TurnDone{
		Header:  event.Header{Coordinates: identity.Coordinates{SessionID: foreign, LoopID: loop, TurnID: turn}, EventID: fixedUUID(0x78)},
		Message: aiMsg("TOP-SECRET-TURN-PAYLOAD"),
	})
	stepWire := persistedStatusEvent(t, event.StepDone{
		Header:   event.Header{Coordinates: identity.Coordinates{SessionID: foreign, LoopID: loop, TurnID: turn, StepID: step}, EventID: fixedUUID(0x79)},
		Messages: content.AgenticMessages{aiMsg("TOP-SECRET-STEP-PAYLOAD")},
	})
	zeroID := "00000000-0000-0000-0000-000000000000"
	tests := []struct {
		name     string
		lastTurn *storedEventSummary
		lastStep *storedEventSummary
	}{
		{name: "foreign last turn", lastTurn: &storedEventSummary{JournalSeq: 6, Event: turnWire}},
		{name: "zero-session last turn", lastTurn: &storedEventSummary{JournalSeq: 6, Event: bytes.ReplaceAll(turnWire, []byte(foreign.String()), []byte(zeroID))}},
		{name: "foreign last step", lastStep: &storedEventSummary{JournalSeq: 5, Event: stepWire}},
		{name: "zero-session last step", lastStep: &storedEventSummary{JournalSeq: 5, Event: bytes.ReplaceAll(stepWire, []byte(foreign.String()), []byte(zeroID))}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			raw := storedStatusMetaJSON(t, requested, test.lastTurn, test.lastStep)
			r := newStoredCatalogReader(t, requested, raw)
			status, err := r.ReadStatus(context.Background(), requested)
			if err == nil {
				t.Fatalf("ReadStatus() = %+v, nil; want fail-closed", status)
			}
			var storeErr serve.StoreReadError
			if !errors.As(err, &storeErr) {
				t.Fatalf("ReadStatus() error = %T %v, want serve.StoreReadError", err, err)
			}
			for _, forbidden := range []string{foreign.String(), "TOP-SECRET", "TurnDone", "StepDone"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Errorf("ReadStatus() error exposed persisted identity/payload %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestReaderReadStatusRejectsMisclassifiedPersistedSummaries(t *testing.T) {
	t.Parallel()

	sid := fixedUUID(0x81)
	loop, turn, step := fixedUUID(0x82), fixedUUID(0x83), fixedUUID(0x84)
	turnWire := persistedStatusEvent(t, event.TurnDone{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn}, EventID: fixedUUID(0x85)},
	})
	stepWire := persistedStatusEvent(t, event.StepDone{
		Header:   event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn, StepID: step}, EventID: fixedUUID(0x86)},
		Messages: content.AgenticMessages{aiMsg("step")},
	})
	tests := []struct {
		name     string
		lastTurn *storedEventSummary
		lastStep *storedEventSummary
	}{
		{name: "last turn cannot contain StepDone", lastTurn: &storedEventSummary{JournalSeq: 5, Event: stepWire}},
		{name: "last step cannot contain TurnDone", lastStep: &storedEventSummary{JournalSeq: 6, Event: turnWire}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := newStoredCatalogReader(t, sid, storedStatusMetaJSON(t, sid, test.lastTurn, test.lastStep))
			if status, err := r.ReadStatus(context.Background(), sid); err == nil {
				t.Fatalf("ReadStatus() = %+v, nil; want summary-kind rejection", status)
			}
		})
	}
}

func TestReaderReadStatusPreservesLegitimatePersistedSummaryJSON(t *testing.T) {
	t.Parallel()

	sid := fixedUUID(0x91)
	loop, turn, step := fixedUUID(0x92), fixedUUID(0x93), fixedUUID(0x94)
	turnWire := persistedStatusEvent(t, event.TurnDone{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn}, EventID: fixedUUID(0x95)},
	})
	stepWire := persistedStatusEvent(t, event.StepDone{
		Header:   event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn, StepID: step}, EventID: fixedUUID(0x96)},
		Messages: content.AgenticMessages{&content.AIMessage{Message: content.Message{Role: content.RoleAssistant}}},
	})
	raw := storedStatusMetaJSON(t, sid,
		&storedEventSummary{JournalSeq: 6, Event: turnWire},
		&storedEventSummary{JournalSeq: 5, Event: stepWire},
	)
	r := newStoredCatalogReader(t, sid, raw)
	status, err := r.ReadStatus(context.Background(), sid)
	if err != nil {
		t.Fatalf("ReadStatus() error = %v", err)
	}
	got, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("json.Marshal(ReadStatus()) error = %v", err)
	}
	want := []byte(`{"session_id":"91919191-9191-9191-9191-919191919191","state":"idle","last_journal_seq":7,"last_turn":{"journal_seq":6,"event":{"event_id":"95959595-9595-9595-9595-959595959595","loop_id":"92929292-9292-9292-9292-929292929292","session_id":"91919191-9191-9191-9191-919191919191","turn_id":"93939393-9393-9393-9393-939393939393","type":"TurnDone","v":1}},"last_step":{"journal_seq":5,"event":{"event_id":"96969696-9696-9696-9696-969696969696","loop_id":"92929292-9292-9292-9292-929292929292","messages":[{"role":"assistant"}],"session_id":"91919191-9191-9191-9191-919191919191","step_id":"94949494-9494-9494-9494-949494949494","turn_id":"93939393-9393-9393-9393-939393939393","type":"StepDone","v":1}}}`)
	if !bytes.Equal(got, want) {
		t.Errorf("legitimate persisted status JSON changed\n got: %s\nwant: %s", got, want)
	}
}

func TestReaderReadStatusEnforcesPersistedSummaryTipBounds(t *testing.T) {
	t.Parallel()

	sid := fixedUUID(0xA7)
	loop, turn, step := fixedUUID(0xA8), fixedUUID(0xA9), fixedUUID(0xAA)
	turnWire := persistedStatusEvent(t, event.TurnDone{
		Header:  event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn}, EventID: fixedUUID(0xAB)},
		Message: aiMsg("TOP-SECRET-FUTURE-TURN"),
	})
	stepWire := persistedStatusEvent(t, event.StepDone{
		Header:   event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn, StepID: step}, EventID: fixedUUID(0xAC)},
		Messages: content.AgenticMessages{aiMsg("TOP-SECRET-FUTURE-STEP")},
	})
	tests := []struct {
		name     string
		tip      uint64
		lastTurn *storedEventSummary
		lastStep *storedEventSummary
		wantErr  bool
	}{
		{name: "last turn future sequence", tip: 7, lastTurn: &storedEventSummary{JournalSeq: 99, Event: turnWire}, wantErr: true},
		{name: "last step future sequence", tip: 7, lastStep: &storedEventSummary{JournalSeq: 99, Event: stepWire}, wantErr: true},
		{name: "last turn equals tip", tip: 7, lastTurn: &storedEventSummary{JournalSeq: 7, Event: turnWire}},
		{name: "last step equals tip", tip: 7, lastStep: &storedEventSummary{JournalSeq: 7, Event: stepWire}},
		{name: "last turn below tip", tip: 7, lastTurn: &storedEventSummary{JournalSeq: 6, Event: turnWire}},
		{name: "last step below tip", tip: 7, lastStep: &storedEventSummary{JournalSeq: 6, Event: stepWire}},
		{name: "last turn zero at zero tip", lastTurn: &storedEventSummary{JournalSeq: 0, Event: turnWire}},
		{name: "last step zero at zero tip", lastStep: &storedEventSummary{JournalSeq: 0, Event: stepWire}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			raw := storedStatusMetaJSONAtTip(t, sid, test.tip, test.lastTurn, test.lastStep)
			r := newStoredCatalogReader(t, sid, raw)
			status, err := r.ReadStatus(context.Background(), sid)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ReadStatus() = %+v, nil; want future-sequence rejection", status)
				}
				for _, forbidden := range []string{sid.String(), "TOP-SECRET", "TurnDone", "StepDone", "99"} {
					if strings.Contains(err.Error(), forbidden) {
						t.Errorf("ReadStatus() error exposed persisted summary detail %q: %v", forbidden, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadStatus() error = %v, want legal boundary", err)
			}
			if test.lastTurn != nil && (status.LastTurn == nil || status.LastTurn.JournalSeq != test.lastTurn.JournalSeq) {
				t.Errorf("LastTurn = %+v, want seq %d", status.LastTurn, test.lastTurn.JournalSeq)
			}
			if test.lastStep != nil && (status.LastStep == nil || status.LastStep.JournalSeq != test.lastStep.JournalSeq) {
				t.Errorf("LastStep = %+v, want seq %d", status.LastStep, test.lastStep.JournalSeq)
			}
		})
	}
}

func TestReaderPreProjectionCompatibilityFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sid  uuid.UUID
		raw  []byte
	}{
		{
			name: "malformed nonempty state",
			sid:  fixedUUID(0x65),
			raw:  []byte(`{"session_id":"65656565-6565-6565-6565-656565656565","state":"future_state"}`),
		},
		{
			name: "missing record session id",
			sid:  fixedUUID(0x66),
			raw:  []byte(`{"state":"idle"}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := newStoredCatalogReader(t, test.sid, test.raw)
			_, listErr := r.ListSessions(context.Background(), serve.Page{Limit: 10})
			var listStoreErr serve.StoreReadError
			if listErr == nil || !errors.As(listErr, &listStoreErr) {
				t.Fatalf("ListSessions() error = %T %v, want serve.StoreReadError", listErr, listErr)
			}
			_, statusErr := r.ReadStatus(context.Background(), test.sid)
			var statusStoreErr serve.StoreReadError
			if statusErr == nil || !errors.As(statusErr, &statusStoreErr) {
				t.Fatalf("ReadStatus() error = %T %v, want serve.StoreReadError", statusErr, statusErr)
			}
		})
	}

	t.Run("status record cannot cross session scope", func(t *testing.T) {
		t.Parallel()
		keyID, recordID := fixedUUID(0x67), fixedUUID(0x68)
		raw := []byte(`{"session_id":"` + recordID.String() + `"}`)
		r := newStoredCatalogReader(t, keyID, raw)
		_, err := r.ReadStatus(context.Background(), keyID)
		var storeErr serve.StoreReadError
		if err == nil || !errors.As(err, &storeErr) {
			t.Fatalf("ReadStatus() error = %T %v, want serve.StoreReadError", err, err)
		}
	})
}

func TestReaderRejectsInvalidCoreReadAuthorityAtActualCallSite(t *testing.T) {
	t.Parallel()

	st, cat, clk := newCatalog(t)
	sid := fixedUUID(0x21)
	update(t, cat, sessionStarted(sid), 1)
	clk.t = time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	update(t, cat, turnStarted(sid, fixedUUID(0x22), fixedUUID(0x23)), 2)
	j := openJournal(t, st, sid)
	if _, err := j.Append(context.Background(), journal.NewEventRecord(sessionStarted(sid))); err != nil {
		t.Fatalf("Append(SessionStarted) error = %v", err)
	}
	r := catalogreader.NewScoped(
		cat,
		st,
		harnesssessionwire.ReadAuthority{AgentID: coresessionwire.AgentID("fixture-agent")},
		coresessionwire.SessionResidencyCold,
	)
	tests := []struct {
		name string
		read func() error
	}{
		{name: "list", read: func() error { _, err := r.ListSessions(context.Background(), serve.Page{Limit: 10}); return err }},
		{name: "status", read: func() error { _, err := r.ReadStatus(context.Background(), sid); return err }},
		{name: "journal", read: func() error {
			_, err := r.ReadJournal(context.Background(), sid, serve.JournalPage{Limit: 10})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.read()
			var storeErr serve.StoreReadError
			if err == nil || !errors.As(err, &storeErr) {
				t.Fatalf("read error = %T %v, want serve.StoreReadError", err, err)
			}
		})
	}
}

func TestReaderColdLegacyJSONStillOmitsMissingActivityTime(t *testing.T) {
	t.Parallel()

	st, cat, _ := newCatalog(t)
	sid := fixedUUID(0x31)
	started := sessionStarted(sid)
	started.CreatedAt = time.Date(2026, 8, 29, 11, 0, 0, 0, time.UTC)
	update(t, cat, started, 1)
	r := catalogreader.NewScoped(
		cat,
		st,
		harnesssessionwire.ReadAuthority{TenantID: "tenant-a", AgentID: "fixture-agent"},
		coresessionwire.SessionResidencyCold,
	)

	list, err := r.ListSessions(context.Background(), serve.Page{Skip: 0, Limit: 10})
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	status, err := r.ReadStatus(context.Background(), sid)
	if err != nil {
		t.Fatalf("ReadStatus() error = %v", err)
	}
	listJSON, _ := json.Marshal(list)
	statusJSON, _ := json.Marshal(status)
	wantList := []byte(`{"sessions":[{"session_id":"31313131-3131-3131-3131-313131313131","state":"idle","created_at":"2026-08-29T11:00:00Z"}],"skip":0,"limit":10,"next_skip":0,"done":true}`)
	wantStatus := []byte(`{"session_id":"31313131-3131-3131-3131-313131313131","state":"idle","last_journal_seq":1}`)
	if !bytes.Equal(listJSON, wantList) {
		t.Errorf("cold legacy list JSON changed\n got: %s\nwant: %s", listJSON, wantList)
	}
	if !bytes.Equal(statusJSON, wantStatus) {
		t.Errorf("cold legacy status JSON changed\n got: %s\nwant: %s", statusJSON, wantStatus)
	}
}

func TestReaderRejectsInvalidAuthorityBeforeStorageAccess(t *testing.T) {
	t.Parallel()

	r := catalogreader.NewScoped(
		nil,
		nil,
		harnesssessionwire.ReadAuthority{AgentID: "fixture-agent"},
		coresessionwire.SessionResidencyCold,
	)
	sid := fixedUUID(0x39)
	tests := []struct {
		name string
		read func() error
	}{
		{name: "list", read: func() error { _, err := r.ListSessions(context.Background(), serve.Page{Limit: 10}); return err }},
		{name: "status", read: func() error { _, err := r.ReadStatus(context.Background(), sid); return err }},
		{name: "journal", read: func() error {
			_, err := r.ReadJournal(context.Background(), sid, serve.JournalPage{Limit: 10})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.read()
			var storeErr serve.StoreReadError
			if err == nil || !errors.As(err, &storeErr) {
				t.Fatalf("read error = %T %v, want serve.StoreReadError", err, err)
			}
		})
	}
}

func TestReaderReadStatus(t *testing.T) {
	t.Parallel()

	sid := fixedUUID(0x44)
	loop, turn, step := fixedUUID(0x51), fixedUUID(0x52), fixedUUID(0x53)
	gid := gate.ID(fixedUUID(0x54))

	tests := []struct {
		name         string
		absent       bool
		build        func(t *testing.T, cat *sessionstore.Catalog)
		wantState    string
		wantActive   uuid.UUID
		wantWaiting  uuid.UUID
		wantLastTurn bool
		wantLastStep bool
		wantNotFound bool
	}{
		{
			name: "running (turn active)",
			build: func(t *testing.T, cat *sessionstore.Catalog) {
				update(t, cat, sessionStarted(sid), 1)
				update(t, cat, turnStarted(sid, loop, turn), 2)
			},
			wantState: "running", wantActive: turn,
		},
		{
			name: "waiting on gate",
			build: func(t *testing.T, cat *sessionstore.Catalog) {
				update(t, cat, sessionStarted(sid), 1)
				update(t, cat, turnStarted(sid, loop, turn), 2)
				update(t, cat, gateOpened(sid, loop, gid), 3)
			},
			wantState: "waiting_on_gate", wantActive: turn, wantWaiting: uuid.UUID(gid),
		},
		{
			name: "idle after turn done (last_turn set)",
			build: func(t *testing.T, cat *sessionstore.Catalog) {
				update(t, cat, sessionStarted(sid), 1)
				update(t, cat, turnStarted(sid, loop, turn), 2)
				update(t, cat, stepDone(sid, loop, turn, step), 3)
				update(t, cat, turnDone(sid, loop, turn, fixedUUID(0xA2)), 4)
			},
			wantState: "idle", wantLastTurn: true, wantLastStep: true,
		},
		{
			name: "stopped",
			build: func(t *testing.T, cat *sessionstore.Catalog) {
				update(t, cat, sessionStarted(sid), 1)
				update(t, cat, sessionStopped(sid), 2)
			},
			wantState: "stopped",
		},
		{name: "absent session is not found", absent: true, wantNotFound: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, cat, _ := newCatalog(t)
			if tt.build != nil {
				tt.build(t, cat)
			}
			r := catalogreader.New(cat, st)

			status, err := r.ReadStatus(context.Background(), sid)
			if tt.wantNotFound {
				var nf serve.SessionNotFoundError
				if err == nil || !errors.As(err, &nf) {
					t.Fatalf("ReadStatus() err = %v, want SessionNotFoundError", err)
				}
				if nf.SessionID != sid {
					t.Errorf("not-found session id = %v, want %v", nf.SessionID, sid)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadStatus() err = %v", err)
			}
			if status.SessionID != sid {
				t.Errorf("session id = %v, want %v", status.SessionID, sid)
			}
			if status.State != tt.wantState {
				t.Errorf("state = %q, want %q", status.State, tt.wantState)
			}
			if status.ActiveTurnID != tt.wantActive {
				t.Errorf("active_turn_id = %v, want %v", status.ActiveTurnID, tt.wantActive)
			}
			if status.WaitingGateID != tt.wantWaiting {
				t.Errorf("waiting_gate_id = %v, want %v", status.WaitingGateID, tt.wantWaiting)
			}
			if tt.wantLastTurn {
				if status.LastTurn == nil {
					t.Fatal("last_turn = nil, want reconstructed TurnDone")
				}
				if _, ok := status.LastTurn.Event.(event.TurnDone); !ok {
					t.Errorf("last_turn event = %T, want event.TurnDone", status.LastTurn.Event)
				}
				if status.LastTurn.JournalSeq != 4 {
					t.Errorf("last_turn seq = %d, want 4", status.LastTurn.JournalSeq)
				}
			}
			if tt.wantLastStep {
				if status.LastStep == nil {
					t.Fatal("last_step = nil, want reconstructed StepDone")
				}
				if _, ok := status.LastStep.Event.(event.StepDone); !ok {
					t.Errorf("last_step event = %T, want event.StepDone", status.LastStep.Event)
				}
				if status.LastStep.JournalSeq != 3 {
					t.Errorf("last_step seq = %d, want 3", status.LastStep.JournalSeq)
				}
			}
		})
	}
}

// openJournal acquires a lease and opens the session journal for appends.
func openJournal(t *testing.T, st *sessionstore.Store, id uuid.UUID) journal.SessionJournal {
	t.Helper()
	lease, err := st.AcquireLease(context.Background(), id)
	if err != nil {
		t.Fatalf("AcquireLease() err = %v", err)
	}
	j, err := st.OpenJournal(context.Background(), id, lease)
	if err != nil {
		t.Fatalf("OpenJournal() err = %v", err)
	}
	return j
}

func TestReaderReadJournal(t *testing.T) {
	t.Parallel()

	sid := fixedUUID(0x66)
	loop := fixedUUID(0x67)

	// Build a ledger holding: SessionStarted, a private GatePreparedRecord (must be
	// filtered), a private HustleStarted, a private PermissionReviewCompleted (Task
	// 17's first real producer of this type), then two TurnDones. The event
	// replayer yields ONLY the three public events.
	buildLedger := func(t *testing.T) *sessionstore.Store {
		st, err := sessionstore.Open(memstore.New())
		if err != nil {
			t.Fatalf("Open() err = %v", err)
		}
		j := openJournal(t, st, sid)

		stepH := event.Header{Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: fixedUUID(0x70), StepID: fixedUUID(0x71)}, EventID: fixedUUID(0x72)}
		g := gate.Gate{ID: gate.ID(fixedUUID(0x73)), Kind: gate.KindPermission, Resolver: gate.ResolverLoop}
		prepared := event.GatePrepared{Header: stepH, Gate: g}
		openPayload := gate.OpenPayload{GateID: g.ID, Payload: gate.PermissionPayload{Request: tool.Request{ToolName: "Bash", Summary: "echo ok", Requirements: []tool.Requirement{{Kind: "tool.invoke", Scope: "Bash", Match: "echo ok", Description: "run: echo ok"}}}}}

		recs := []journal.JournalRecord{
			journal.NewEventRecord(sessionStarted(sid)),                                   // seq 2 (after opening fence at 1)
			journal.NewGatePreparedRecord(prepared, openPayload),                          // seq 3 (filtered)
			journal.NewEventRecord(hustleStarted(t, sid)),                                 // seq 4 (internal, filtered)
			journal.NewEventRecord(permissionReviewCompleted(sid)),                        // seq 5 (internal, filtered)
			journal.NewEventRecord(turnDone(sid, loop, fixedUUID(0x74), fixedUUID(0xA6))), // seq 6
			journal.NewEventRecord(turnDone(sid, loop, fixedUUID(0x75), fixedUUID(0xA7))), // seq 7
		}
		for _, rec := range recs {
			if _, err := j.Append(context.Background(), rec); err != nil {
				t.Fatalf("Append(%T) err = %v", rec, err)
			}
		}
		return st
	}

	tests := []struct {
		name         string
		absent       bool
		page         serve.JournalPage
		wantCount    int
		wantFirstSeq uint64
		wantNext     uint64
		wantDone     bool
		wantNoGate   bool
	}{
		{name: "absent session yields empty done", absent: true, page: serve.JournalPage{From: 0, Limit: 100}, wantCount: 0, wantNext: 0, wantDone: true},
		{name: "from beginning yields all events done", page: serve.JournalPage{From: 0, Limit: 100}, wantCount: 3, wantFirstSeq: 2, wantNext: 0, wantDone: true, wantNoGate: true},
		{name: "limit under total not done", page: serve.JournalPage{From: 0, Limit: 2}, wantCount: 2, wantFirstSeq: 2, wantNext: 7, wantDone: false},
		{name: "from interior sequence", page: serve.JournalPage{From: 4, Limit: 100}, wantCount: 2, wantFirstSeq: 6, wantNext: 0, wantDone: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var st *sessionstore.Store
			if tt.absent {
				var err error
				st, err = sessionstore.Open(memstore.New())
				if err != nil {
					t.Fatalf("Open() err = %v", err)
				}
			} else {
				st = buildLedger(t)
			}
			cat := st.OpenCatalog()
			r := catalogreader.New(cat, st)

			page, err := r.ReadJournal(context.Background(), sid, tt.page)
			if err != nil {
				t.Fatalf("ReadJournal() err = %v", err)
			}
			if len(page.Events) != tt.wantCount {
				t.Fatalf("returned %d events, want %d (%+v)", len(page.Events), tt.wantCount, page.Events)
			}
			if tt.wantCount > 0 && page.Events[0].JournalSeq != tt.wantFirstSeq {
				t.Errorf("first event seq = %d, want %d", page.Events[0].JournalSeq, tt.wantFirstSeq)
			}
			if page.NextJournalSeq != tt.wantNext || page.Done != tt.wantDone {
				t.Errorf("cursor = {next:%d done:%v}, want {next:%d done:%v}", page.NextJournalSeq, page.Done, tt.wantNext, tt.wantDone)
			}
			if tt.wantNoGate {
				for _, se := range page.Events {
					switch se.Event.(type) {
					case event.GatePrepared, event.HustleStarted, event.HustleCompleted, event.HustleFailed,
						event.PermissionReviewStarted, event.PermissionReviewCompleted:
						t.Errorf("private event %T leaked into journal page at seq %d", se.Event, se.JournalSeq)
					}
				}
			}
		})
	}
}

func TestReaderJournalLegacyGoldenAndCoreRedactionParity(t *testing.T) {
	t.Parallel()

	sid, loop, turn, step := fixedUUID(0xB1), fixedUUID(0xB2), fixedUUID(0xB3), fixedUUID(0xB4)
	st, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	j := openJournal(t, st, sid)
	resolved := event.GateResolved{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn, StepID: step},
			EventID:     fixedUUID(0xB5), CreatedAt: time.Date(2026, 8, 29, 17, 0, 0, 0, time.UTC),
		},
		GateID: gate.ID(fixedUUID(0xB6)), Resolver: gate.ResolverLoop,
		Reason: gate.CloseAnswered, Action: gate.FormActionAccept,
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
		Audit:  gate.FormAudit{Values: map[string]string{"password": "raw-tool-marker"}},
	}
	if _, err := j.Append(context.Background(), journal.NewEventRecord(resolved)); err != nil {
		t.Fatalf("Append(GateResolved) error = %v", err)
	}
	r := catalogreader.NewScoped(
		st.OpenCatalog(), st,
		harnesssessionwire.ReadAuthority{TenantID: "tenant-a", AgentID: "fixture-agent"},
		coresessionwire.SessionResidencyCold,
	)
	legacy, err := r.ReadJournal(context.Background(), sid, serve.JournalPage{Limit: 10})
	if err != nil {
		t.Fatalf("ReadJournal() error = %v", err)
	}
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("json.Marshal(legacy) error = %v", err)
	}
	// This is the independently specified current serve compatibility wire. Its
	// native audit remains byte-stable; the corresponding Core body below is the
	// public redacted representation used by the new read plane.
	wantLegacy := []byte(`{"events":[{"journal_seq":2,"event":{"action":"accept","audit":{"kind":"form","data":{"values":{"password":"raw-tool-marker"}}},"created_at":"2026-08-29T17:00:00Z","event_id":"b5b5b5b5-b5b5-b5b5-b5b5-b5b5b5b5b5b5","gate_id":"b6b6b6b6-b6b6-b6b6-b6b6-b6b6b6b6b6b6","loop_id":"b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b2","reason":"answered","resolver":"loop","session_id":"b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1","source":{"kind":"user"},"step_id":"b4b4b4b4-b4b4-b4b4-b4b4-b4b4b4b4b4b4","turn_id":"b3b3b3b3-b3b3-b3b3-b3b3-b3b3b3b3b3b3","type":"GateResolved","v":1}}],"next_journal_seq":0,"done":true}`)
	if !bytes.Equal(legacyJSON, wantLegacy) {
		t.Fatalf("legacy journal JSON changed\n got: %s\nwant: %s", legacyJSON, wantLegacy)
	}
	corePage, err := harnesssessionwire.ProjectJournalPage(
		harnesssessionwire.ReadScope{TenantID: "tenant-a", SessionID: coresessionwire.SessionID(sid.String()), AgentID: "fixture-agent", Residency: coresessionwire.SessionResidencyCold},
		[]harnesssessionwire.JournalRecord{{JournalSeq: legacy.Events[0].JournalSeq, Event: legacy.Events[0].Event}},
		2, 2, "", "",
	)
	if err != nil {
		t.Fatalf("ProjectJournalPage() error = %v", err)
	}
	if len(corePage.Events) != 1 || corePage.Events[0].JournalSeq != 2 || corePage.Events[0].EventID != coresessionwire.EventID(fixedUUID(0xB5).String()) {
		t.Fatalf("Core journal projection = %+v", corePage)
	}
	if bytes.Contains(corePage.Events[0].Body, []byte("raw-tool-marker")) || bytes.Contains(corePage.Events[0].Body, []byte(`"audit"`)) {
		t.Fatalf("Core journal body leaked redacted tool data: %s", corePage.Events[0].Body)
	}
}

func TestReaderMultiplePublicGatesLegacyAndCoreParity(t *testing.T) {
	t.Parallel()

	sid, loop, turn := fixedUUID(0xC1), fixedUUID(0xC2), fixedUUID(0xC3)
	st, err := sessionstore.Open(memstore.New())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	j := openJournal(t, st, sid)
	makeOpened := func(eventSeed, gateSeed, stepSeed byte, at time.Time, body string) event.GateOpened {
		return event.GateOpened{
			Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: sid, LoopID: loop, TurnID: turn, StepID: fixedUUID(stepSeed)},
				EventID:     fixedUUID(eventSeed), CreatedAt: at,
			},
			Gate: gate.Gate{
				ID: gate.ID(fixedUUID(gateSeed)), Kind: gate.KindPermission, Resolver: gate.ResolverLoop,
				Subject: gate.Subject{ToolExecutionID: gate.ID(fixedUUID(stepSeed + 1)), ToolUseID: "private-tool-marker"},
				Prompt:  gate.Prompt{Body: body},
			},
		}
	}
	first := makeOpened(0xC6, 0xC8, 0xC4, time.Date(2026, 8, 29, 18, 0, 0, 0, time.UTC), "first")
	second := makeOpened(0xC7, 0xC9, 0xC5, time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC), "second")
	for _, opened := range []event.GateOpened{first, second} {
		if _, err := j.Append(context.Background(), journal.NewEventRecord(opened)); err != nil {
			t.Fatalf("Append(GateOpened) error = %v", err)
		}
	}
	r := catalogreader.NewScoped(
		st.OpenCatalog(), st,
		harnesssessionwire.ReadAuthority{TenantID: "tenant-a", AgentID: "fixture-agent"},
		coresessionwire.SessionResidencyResident,
	)
	legacy, err := r.ReadJournal(context.Background(), sid, serve.JournalPage{Limit: 10})
	if err != nil {
		t.Fatalf("ReadJournal() error = %v", err)
	}
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("json.Marshal(legacy) error = %v", err)
	}
	wantLegacy := []byte(`{"events":[{"journal_seq":2,"event":{"created_at":"2026-08-29T18:00:00Z","event_id":"c6c6c6c6-c6c6-c6c6-c6c6-c6c6c6c6c6c6","gate":{"id":"c8c8c8c8-c8c8-c8c8-c8c8-c8c8c8c8c8c8","kind":"harness.permission","resolver":"loop","subject":{"tool_execution_id":"c5c5c5c5-c5c5-c5c5-c5c5-c5c5c5c5c5c5","tool_use_id":"private-tool-marker"},"prompt":{"body":"first"}},"loop_id":"c2c2c2c2-c2c2-c2c2-c2c2-c2c2c2c2c2c2","session_id":"c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1","step_id":"c4c4c4c4-c4c4-c4c4-c4c4-c4c4c4c4c4c4","turn_id":"c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c3","type":"GateOpened","v":1}},{"journal_seq":3,"event":{"created_at":"2026-08-29T18:01:00Z","event_id":"c7c7c7c7-c7c7-c7c7-c7c7-c7c7c7c7c7c7","gate":{"id":"c9c9c9c9-c9c9-c9c9-c9c9-c9c9c9c9c9c9","kind":"harness.permission","resolver":"loop","subject":{"tool_execution_id":"c6c6c6c6-c6c6-c6c6-c6c6-c6c6c6c6c6c6","tool_use_id":"private-tool-marker"},"prompt":{"body":"second"}},"loop_id":"c2c2c2c2-c2c2-c2c2-c2c2-c2c2c2c2c2c2","session_id":"c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1","step_id":"c5c5c5c5-c5c5-c5c5-c5c5-c5c5c5c5c5c5","turn_id":"c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c3","type":"GateOpened","v":1}}],"next_journal_seq":0,"done":true}`)
	if !bytes.Equal(legacyJSON, wantLegacy) {
		t.Fatalf("legacy multi-gate journal JSON changed\n got: %s\nwant: %s", legacyJSON, wantLegacy)
	}
	opens := make([]harnesssessionwire.OpenGate, 0, len(legacy.Events))
	for _, statusEvent := range legacy.Events {
		opened, ok := statusEvent.Event.(event.GateOpened)
		if !ok {
			t.Fatalf("legacy event = %T, want event.GateOpened", statusEvent.Event)
		}
		opens = append(opens, harnesssessionwire.OpenGate{
			Event: opened, JournalSeq: statusEvent.JournalSeq,
			Deadline: time.Date(2026, 8, 29, 19, 0, 0, 0, time.UTC), Answerability: coresessionwire.GateAnswerabilityResident,
		})
	}
	corePage, err := harnesssessionwire.ProjectGatePage(
		harnesssessionwire.ReadScope{TenantID: "tenant-a", SessionID: coresessionwire.SessionID(sid.String()), AgentID: "fixture-agent", Residency: coresessionwire.SessionResidencyResident},
		opens, 3, 2, "", "",
	)
	if err != nil {
		t.Fatalf("ProjectGatePage() error = %v", err)
	}
	if len(corePage.Gates) != 2 || corePage.OpenGateCount != 2 || corePage.Gates[0].Prompt.Body != "first" || corePage.Gates[1].Prompt.Body != "second" || corePage.Gates[0].OpenedJournalSeq != 2 || corePage.Gates[1].OpenedJournalSeq != 3 {
		t.Fatalf("Core gate projection = %+v", corePage)
	}
	coreJSON, err := json.Marshal(corePage)
	if err != nil {
		t.Fatalf("json.Marshal(corePage) error = %v", err)
	}
	if bytes.Contains(coreJSON, []byte("private-tool-marker")) || bytes.Contains(coreJSON, []byte("tool_execution_id")) {
		t.Fatalf("Core gate page leaked tool subject: %s", coreJSON)
	}
}
