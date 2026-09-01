package sessionwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

func catalogUUID(seed byte) uuid.UUID {
	var id uuid.UUID
	for index := range id {
		id[index] = seed
	}
	return id
}

func catalogScope(seed byte, residency coresessionwire.SessionResidency) ReadScope {
	return ReadScope{
		TenantID:  "tenant-a",
		SessionID: coresessionwire.SessionID(catalogUUID(seed).String()),
		AgentID:   "agent-primary",
		Residency: residency,
	}
}

func TestProjectCatalogColdAndRunningRecords(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 8, 29, 10, 0, 0, 123000000, time.FixedZone("fixture", -4*60*60))
	active := created.Add(5 * time.Minute)
	tests := []struct {
		name       string
		scope      ReadScope
		meta       CatalogRecord
		wantState  coresessionwire.SessionState
		wantLegacy string
		wantActive time.Time
	}{
		{
			name:  "cold idle session remains cold",
			scope: catalogScope(0x11, coresessionwire.SessionResidencyCold),
			meta: CatalogRecord{
				SessionID: coresessionwire.SessionID(catalogUUID(0x11).String()), State: CatalogStateIdle,
				Title: "cold", CreatedAt: created, LastJournalSeq: 7,
			},
			wantState: coresessionwire.SessionStateIdle, wantLegacy: "idle", wantActive: created,
		},
		{
			name:  "running resident session",
			scope: catalogScope(0x22, coresessionwire.SessionResidencyResident),
			meta: CatalogRecord{
				SessionID: coresessionwire.SessionID(catalogUUID(0x22).String()), State: CatalogStateRunning,
				Title: "running", CreatedAt: created, LastActiveAt: active, LastJournalSeq: 9,
			},
			wantState: coresessionwire.SessionStateRunning, wantLegacy: "running", wantActive: active,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			summary, err := ProjectSessionSummary(test.scope, test.meta)
			if err != nil {
				t.Fatalf("ProjectSessionSummary() error = %v", err)
			}
			status, err := ProjectSessionStatus(test.scope, test.meta)
			if err != nil {
				t.Fatalf("ProjectSessionStatus() error = %v", err)
			}
			if summary.SessionID != test.scope.SessionID || summary.AgentID != test.scope.AgentID || summary.State != test.wantState {
				t.Errorf("summary identity/state = %+v", summary)
			}
			if !summary.CreatedAt.Equal(created.UTC()) || !summary.LastActiveAt.Equal(test.wantActive.UTC()) {
				t.Errorf("summary times = %v/%v, want UTC %v/%v", summary.CreatedAt, summary.LastActiveAt, created.UTC(), test.wantActive.UTC())
			}
			if status.State != test.wantState || status.Residency != test.scope.Residency || status.JournalTip != test.meta.LastJournalSeq {
				t.Errorf("status = %+v", status)
			}
			if got := string(summary.State); got != test.wantLegacy {
				t.Errorf("legacy state spelling = %q, want %q", got, test.wantLegacy)
			}
		})
	}
}

func TestProjectCatalogRejectsCallerScopeMismatchAndMalformedAuthority(t *testing.T) {
	t.Parallel()

	meta := CatalogRecord{
		SessionID: coresessionwire.SessionID(catalogUUID(0x31).String()), State: CatalogStateIdle,
		Title: "private-title-marker", LastActiveAt: time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name      string
		scope     ReadScope
		wantField string
	}{
		{name: "wrong session", scope: catalogScope(0x32, coresessionwire.SessionResidencyCold), wantField: "session_id"},
		{name: "empty tenant", scope: ReadScope{SessionID: meta.SessionID, AgentID: "agent", Residency: coresessionwire.SessionResidencyCold}, wantField: "tenant_id"},
		{name: "empty agent", scope: ReadScope{TenantID: "tenant", SessionID: meta.SessionID, Residency: coresessionwire.SessionResidencyCold}, wantField: "agent_id"},
		{name: "empty residency", scope: ReadScope{TenantID: "tenant", SessionID: meta.SessionID, AgentID: "agent"}, wantField: "residency"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ProjectSessionStatus(test.scope, meta)
			var projectionErr *ReadProjectionError
			if err == nil || !errors.As(err, &projectionErr) {
				t.Fatalf("ProjectSessionStatus() error = %T %v, want *ReadProjectionError", err, err)
			}
			if projectionErr.Field != test.wantField {
				t.Errorf("projection error field = %q, want %q", projectionErr.Field, test.wantField)
			}
			if bytes.Contains([]byte(err.Error()), []byte(meta.Title)) {
				t.Fatalf("error leaked catalog payload: %v", err)
			}
		})
	}
}

func TestProjectSessionPagePreservesBoundedCursorMetadata(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	metas := []CatalogRecord{
		{SessionID: coresessionwire.SessionID(catalogUUID(0x41).String()), State: CatalogStateRunning, LastActiveAt: now},
		{SessionID: coresessionwire.SessionID(catalogUUID(0x42).String()), State: CatalogStateIdle, LastActiveAt: now.Add(-time.Minute)},
	}
	page, err := ProjectSessionPage(ReadAuthority{TenantID: "tenant-a", AgentID: "agent-primary"}, metas, "next-token", "previous-token")
	if err != nil {
		t.Fatalf("ProjectSessionPage() error = %v", err)
	}
	if len(page.Sessions) != 2 || page.NextCursor != "next-token" || page.PreviousCursor != "previous-token" {
		t.Fatalf("page = %+v", page)
	}
	if page.Sessions[0].SessionID != metas[0].SessionID || page.Sessions[1].SessionID != metas[1].SessionID {
		t.Errorf("page session order changed: %+v", page.Sessions)
	}
}

func TestProjectJournalPageRedactsToolDataAndCarriesCoverage(t *testing.T) {
	t.Parallel()

	scope := catalogScope(0x51, coresessionwire.SessionResidencyCold)
	header := event.Header{
		Coordinates: identity.Coordinates{SessionID: catalogUUID(0x51), LoopID: catalogUUID(0x52), TurnID: catalogUUID(0x53), StepID: catalogUUID(0x54)},
		EventID:     catalogUUID(0x55), CreatedAt: time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC),
	}
	resolved := event.GateResolved{
		Header: header, GateID: gate.ID(catalogUUID(0x56)), Resolver: gate.ResolverLoop,
		Reason: gate.CloseAnswered, Action: gate.FormActionAccept,
		Source: gate.ResponseSource{Kind: gate.ResponseFromUser},
		Audit:  gate.FormAudit{Values: map[string]string{"password": "raw-tool-marker"}},
	}
	page, err := ProjectJournalPage(scope, []JournalRecord{{JournalSeq: 8, Event: resolved}}, 11, 9, "next", "")
	if err != nil {
		t.Fatalf("ProjectJournalPage() error = %v", err)
	}
	if page.CapturedTip != 11 || page.CoveredThrough != 9 || page.NextCursor != "next" || len(page.Events) != 1 {
		t.Fatalf("page = %+v", page)
	}
	wire, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("json.Marshal(page) error = %v", err)
	}
	if bytes.Contains(wire, []byte("raw-tool-marker")) || bytes.Contains(wire, []byte(`"audit"`)) {
		t.Fatalf("public journal leaked private tool/gate data: %s", wire)
	}
	for _, marker := range [][]byte{[]byte(`"journal_tip":11`), []byte(`"covered_through":9`), []byte(`"journal_seq":8`)} {
		if !bytes.Contains(wire, marker) {
			t.Errorf("wire missing %s: %s", marker, wire)
		}
	}
}

func TestProjectGatePageProjectsMultiplePresentationSafeGates(t *testing.T) {
	t.Parallel()

	scope := catalogScope(0x61, coresessionwire.SessionResidencyResident)
	openedAt := time.Date(2026, 8, 29, 14, 0, 0, 0, time.UTC)
	makeOpen := func(seed byte, seq uint64, body string) OpenGate {
		return OpenGate{
			Event: event.GateOpened{
				Header: event.Header{
					Coordinates: identity.Coordinates{SessionID: catalogUUID(0x61), LoopID: catalogUUID(0x62), TurnID: catalogUUID(0x63), StepID: catalogUUID(seed + 1)},
					EventID:     catalogUUID(seed), CreatedAt: openedAt.Add(time.Duration(seq) * time.Second),
				},
				Gate: gate.Gate{
					ID: gate.ID(catalogUUID(seed + 2)), Kind: gate.KindPermission, Resolver: gate.ResolverLoop,
					Subject: gate.Subject{ToolExecutionID: gate.ID(catalogUUID(seed + 3)), ToolUseID: "private-tool-use-marker"},
					Prompt:  gate.Prompt{Title: "Permission", Body: body, Controls: gate.ApprovalControls()},
				},
			},
			JournalSeq: seq, Deadline: openedAt.Add(time.Hour), Answerability: coresessionwire.GateAnswerabilityResident,
		}
	}
	page, err := ProjectGatePage(scope, []OpenGate{makeOpen(0x70, 4, "first"), makeOpen(0x80, 7, "second")}, 9, 2, "", "")
	if err != nil {
		t.Fatalf("ProjectGatePage() error = %v", err)
	}
	if len(page.Gates) != 2 || page.OpenGateCount != 2 || page.JournalTip != 9 {
		t.Fatalf("page = %+v", page)
	}
	if page.Gates[0].OpenedJournalSeq != 4 || page.Gates[1].OpenedJournalSeq != 7 {
		t.Errorf("gate order = %d/%d, want 4/7", page.Gates[0].OpenedJournalSeq, page.Gates[1].OpenedJournalSeq)
	}
	wire, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("json.Marshal(page) error = %v", err)
	}
	for _, private := range [][]byte{[]byte("private-tool-use-marker"), []byte("tool_execution_id"), []byte("response_policy"), []byte("restorable")} {
		if bytes.Contains(wire, private) {
			t.Errorf("gate page leaked %q: %s", private, wire)
		}
	}
	for _, public := range [][]byte{[]byte(`"body":"first"`), []byte(`"body":"second"`), []byte(`"open_gate_count":2`)} {
		if !bytes.Contains(wire, public) {
			t.Errorf("gate page missing %s: %s", public, wire)
		}
	}
}
