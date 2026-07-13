package sessionstore

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// fixedUUID builds a deterministic non-zero uuid from a single seed byte so the table
// tests round-trip stable, readable ids that also sort predictably by seed.
func fixedUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

// fixedClock returns a CatalogClock that always reports t — deterministic LastActiveAt.
func fixedClock(t time.Time) CatalogClock { return func() time.Time { return t } }

// userMsg builds a *content.UserMessage carrying a single text block — the shape
// TurnStarted's Message has when deriving a Title.
func userMsg(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// hdr builds an event.Header carrying only the session id — the coordinates a
// session-scoped catalog event needs.
func hdr(sid uuid.UUID) event.Header {
	return event.Header{Coordinates: identity.Coordinates{SessionID: sid}}
}

func mustApplyEvent(t *testing.T, meta SessionMeta, ev event.Event, seq uint64, now CatalogClock) (SessionMeta, bool) {
	t.Helper()
	updated, changed, err := applyEvent(meta, ev, seq, now)
	if err != nil {
		t.Fatalf("applyEvent(%T) error = %v", ev, err)
	}
	return updated, changed
}

// --- fakeKV: an in-memory storage.KV double with fault + conflict injection ---

type kvEnt struct {
	val []byte
	rev uint64
}

// fakeKV is a minimal storage.KV double for unit-testing the catalog without a real
// backend. getErr/putErr force I/O failures (best-effort proof); conflictN forces the
// first N Puts to return a *ConflictError (rev-CAS retry proof).
type fakeKV struct {
	mu        sync.Mutex
	entries   map[string]kvEnt
	getErr    error
	putErr    error
	conflictN int
	puts      int
	gets      int
}

func newFakeKV() *fakeKV { return &fakeKV{entries: map[string]kvEnt{}} }

var _ storage.KV = (*fakeKV)(nil)

func (k *fakeKV) Get(_ context.Context, key string) ([]byte, uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gets++
	if k.getErr != nil {
		return nil, 0, k.getErr
	}
	e, ok := k.entries[key]
	if !ok {
		return nil, 0, &storage.KeyNotFoundError{Key: key}
	}
	return append([]byte(nil), e.val...), e.rev, nil
}

func (k *fakeKV) Put(_ context.Context, key string, expectedRev uint64, val []byte) (uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.puts++
	if k.putErr != nil {
		return 0, k.putErr
	}
	if k.conflictN > 0 {
		k.conflictN--
		return 0, &storage.ConflictError{Name: key, Expected: expectedRev}
	}
	cur := k.entries[key].rev
	if expectedRev != cur {
		return 0, &storage.ConflictError{Name: key, Expected: expectedRev}
	}
	nr := cur + 1
	k.entries[key] = kvEnt{val: append([]byte(nil), val...), rev: nr}
	return nr, nil
}

func (k *fakeKV) Keys(_ context.Context, prefix string) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.getErr != nil {
		return nil, k.getErr
	}
	var out []string
	for key := range k.entries {
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (k *fakeKV) Delete(_ context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.entries, key)
	return nil
}

// recordingLogger captures best-effort update failures so a test can assert the error
// was logged (and swallowed, never returned).
type recordingLogger struct {
	mu   sync.Mutex
	errs []error
}

func (l *recordingLogger) CatalogUpdateFailed(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, err)
}

func (l *recordingLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.errs)
}

// --- fakeOpener/fakeReplayer: a scripted EventReplayerOpener for RepairCatalog ---

type fakeOpener struct {
	events  []event.Event
	openErr error
}

func (o *fakeOpener) OpenEventReplayer(_ uuid.UUID, _ ReplayRequest) (journal.EventReplayer, error) {
	if o.openErr != nil {
		return nil, o.openErr
	}
	return &fakeReplayer{events: o.events}, nil
}

type fakeReplayer struct{ events []event.Event }

func (r *fakeReplayer) Open(_ context.Context, _ journal.ReplayRequest) (journal.EventCursor, error) {
	return &fakeEventCursor{events: r.events}, nil
}

type fakeEventCursor struct {
	events []event.Event
	pos    int
}

func (c *fakeEventCursor) Next(_ context.Context) (event.Event, uint64, error) {
	if c.pos >= len(c.events) {
		return nil, 0, io.EOF
	}
	ev := c.events[c.pos]
	c.pos++
	return ev, uint64(c.pos), nil
}

func (c *fakeEventCursor) Close() error { return nil }

func TestWorkspacePointersFoldCheckpointAndRestoreIndependently(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x71)
	eventA, eventB, restoreA := fixedUUID(0x72), fixedUUID(0x73), fixedUUID(0x74)
	refA := workspacestore.Ref("v1:sha256:" + strings.Repeat("a", 64))
	refB := workspacestore.Ref("v1:sha256:" + strings.Repeat("b", 64))
	meta := SessionMeta{}

	meta, _ = mustApplyEvent(t, meta, event.WorkspaceCheckpointed{
		Header:      event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: eventA},
		Ref:         string(refA),
		Consistency: event.SnapshotQuiescent,
		Trigger:     event.SnapshotTriggerManual,
	}, 11, fixedClock(time.Time{}))
	meta, _ = mustApplyEvent(t, meta, event.WorkspaceCheckpointed{
		Header:      event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: eventB},
		Ref:         string(refB),
		Consistency: event.SnapshotFuzzy,
		Trigger:     event.SnapshotTriggerIdle,
	}, 17, fixedClock(time.Time{}))
	meta, _ = mustApplyEvent(t, meta, event.WorkspaceRestored{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: restoreA},
		Ref:    string(refA),
	}, 23, fixedClock(time.Time{}))

	wantLast := CheckpointSummary{Ref: refB, EventID: eventB, Seq: 17, Consistency: event.SnapshotFuzzy}
	if meta.LastCheckpoint != wantLast {
		t.Errorf("LastCheckpoint = %+v, want %+v", meta.LastCheckpoint, wantLast)
	}
	wantCurrent := WorkspacePointer{Ref: refA, EventID: restoreA, Seq: 23, Source: WorkspacePointerSourceRestore}
	if meta.CurrentWorkspace != wantCurrent {
		t.Errorf("CurrentWorkspace = %+v, want %+v", meta.CurrentWorkspace, wantCurrent)
	}

	// A delayed catalog upsert may fold an older durable event after a newer one. Journal
	// sequence, not arrival order or content identity, decides the pointer.
	meta, _ = mustApplyEvent(t, meta, event.WorkspaceCheckpointed{
		Header:      event.Header{Coordinates: identity.Coordinates{SessionID: sid}, EventID: fixedUUID(0x70)},
		Ref:         string(refA),
		Consistency: event.SnapshotQuiescent,
		Trigger:     event.SnapshotTriggerManual,
	}, 5, fixedClock(time.Time{}))
	if meta.LastCheckpoint != wantLast || meta.CurrentWorkspace != wantCurrent {
		t.Errorf("older event rewound pointers: last=%+v current=%+v", meta.LastCheckpoint, meta.CurrentWorkspace)
	}
}

func TestWorkspacePointerLegacyMetadataDecodesUnknown(t *testing.T) {
	t.Parallel()
	sid, eid := fixedUUID(0x75), fixedUUID(0x76)
	ref := "v1:sha256:" + strings.Repeat("c", 64)
	data := []byte(`{"session_id":"` + sid.String() + `","last_checkpoint":{"ref":"` + ref + `","event_id":"` + eid.String() + `","seq":9},"current_workspace":{"ref":"` + ref + `","event_id":"` + eid.String() + `","seq":9}}`)

	meta, err := decodeSessionMeta(data)
	if err != nil {
		t.Fatalf("decodeSessionMeta() err = %v", err)
	}
	if meta.LastCheckpoint.Consistency != event.SnapshotConsistencyUnknown {
		t.Errorf("legacy consistency = %v, want unknown", meta.LastCheckpoint.Consistency)
	}
	if meta.CurrentWorkspace.Source != WorkspacePointerSourceUnknown {
		t.Errorf("legacy source = %v, want unknown", meta.CurrentWorkspace.Source)
	}
}

// TestApplyEvent covers the catalog-relevant event->field mapping: the single source of
// truth applyEvent both the inline update and repair share. It is pure (clock injected),
// so it needs no KV.
func TestApplyEvent(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x01)
	created := time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC)
	active := time.Date(2026, 6, 21, 9, 30, 0, 0, time.UTC)
	fp := event.ConfigFingerprint{AgentKind: "primary", ModelID: "m1"}

	tests := []struct {
		name        string
		start       SessionMeta
		ev          event.Event
		wantChanged bool
		check       func(*testing.T, SessionMeta)
	}{
		{
			name: "SessionStarted creates the record",
			ev: event.SessionStarted{
				Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: created},
				Config: fp,
			},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.SessionID != sid {
					t.Errorf("SessionID = %v, want %v", m.SessionID, sid)
				}
				if !m.CreatedAt.Equal(created) {
					t.Errorf("CreatedAt = %v, want %v", m.CreatedAt, created)
				}
				if m.Status != StatusActive {
					t.Errorf("Status = %q, want active", m.Status)
				}
				if m.AgentKind != "primary" {
					t.Errorf("AgentKind = %q, want primary (from fingerprint)", m.AgentKind)
				}
				if m.LoopCount != 1 {
					t.Errorf("LoopCount = %d, want 1 (primary counted)", m.LoopCount)
				}
				if !m.ConfigFingerprint.Equal(fp) {
					t.Errorf("ConfigFingerprint = %+v, want %+v", m.ConfigFingerprint, fp)
				}
			},
		},
		{
			name:  "first TurnStarted sets Title and bumps LastActiveAt",
			start: SessionMeta{SessionID: sid, Status: StatusActive},
			ev: event.TurnStarted{
				Header:  hdr(sid),
				Message: userMsg("Hello there, fix the bug"),
			},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.Title != "Hello there, fix the bug" {
					t.Errorf("Title = %q, want derived from message", m.Title)
				}
				if !m.LastActiveAt.Equal(active) {
					t.Errorf("LastActiveAt = %v, want %v", m.LastActiveAt, active)
				}
			},
		},
		{
			name:  "second TurnStarted does not overwrite Title (first wins) but bumps activity",
			start: SessionMeta{SessionID: sid, Title: "first title"},
			ev: event.TurnStarted{
				Header:  hdr(sid),
				Message: userMsg("second message"),
			},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.Title != "first title" {
					t.Errorf("Title = %q, want unchanged first title", m.Title)
				}
				if !m.LastActiveAt.Equal(active) {
					t.Errorf("LastActiveAt = %v, want bumped", m.LastActiveAt)
				}
			},
		},
		{
			name:        "StepDone bumps LastActiveAt",
			start:       SessionMeta{SessionID: sid},
			ev:          event.StepDone{Header: hdr(sid)},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if !m.LastActiveAt.Equal(active) {
					t.Errorf("LastActiveAt = %v, want %v", m.LastActiveAt, active)
				}
			},
		},
		{
			name:        "RestoreDone bumps LastActiveAt",
			start:       SessionMeta{SessionID: sid},
			ev:          event.RestoreDone{Header: hdr(sid)},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if !m.LastActiveAt.Equal(active) {
					t.Errorf("LastActiveAt = %v, want %v", m.LastActiveAt, active)
				}
			},
		},
		{
			name:        "LoopStarted increments LoopCount",
			start:       SessionMeta{SessionID: sid, LoopCount: 1},
			ev:          event.LoopStarted{Header: hdr(sid)},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.LoopCount != 2 {
					t.Errorf("LoopCount = %d, want 2", m.LoopCount)
				}
			},
		},
		{
			name:        "SessionStopped flips Status",
			start:       SessionMeta{SessionID: sid, Status: StatusActive},
			ev:          event.SessionStopped{Header: hdr(sid)},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.Status != StatusStopped {
					t.Errorf("Status = %q, want stopped", m.Status)
				}
			},
		},
		{
			name:        "non-catalog event is a no-op",
			start:       SessionMeta{SessionID: sid, Title: "keep"},
			ev:          event.SessionActive{Header: hdr(sid)},
			wantChanged: false,
			check: func(t *testing.T, m SessionMeta) {
				if m.Title != "keep" {
					t.Errorf("Title = %q, want unchanged on no-op", m.Title)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, changed := mustApplyEvent(t, tt.start, tt.ev, 0, fixedClock(active))
			if changed != tt.wantChanged {
				t.Fatalf("applyEvent changed = %v, want %v", changed, tt.wantChanged)
			}
			tt.check(t, got)
		})
	}
}

// hdrTurn builds an event.Header carrying the session and an active turn id — the
// coordinates a turn-scoped catalog event needs for the status fold's ActiveTurnID.
func hdrTurn(sid, tid uuid.UUID) event.Header {
	return event.Header{Coordinates: identity.Coordinates{SessionID: sid, TurnID: tid}}
}

// TestApplyEventStatusFold covers the status projection: each catalog-relevant event
// maps to the derived State / LastJournalSeq / ActiveTurnID / WaitingGateID / summary
// fields. It is pure (clock + seq injected), so it needs no KV.
func TestApplyEventStatusFold(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x01)
	tid := fixedUUID(0x0A)
	gid := fixedUUID(0x0B)
	created := time.Date(2026, 7, 8, 8, 0, 0, 0, time.UTC)
	clock := time.Date(2026, 7, 8, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		start       SessionMeta
		ev          event.Event
		seq         uint64
		wantChanged bool
		wantOrder   bool
		check       func(*testing.T, SessionMeta)
	}{
		{
			name: "SessionStarted folds to idle",
			ev: event.SessionStarted{
				Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: created},
				Config: event.ConfigFingerprint{ModelID: "m"},
			},
			seq:         5,
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.State != StateIdle {
					t.Errorf("State = %q, want idle", m.State)
				}
				if m.LastJournalSeq != 5 {
					t.Errorf("LastJournalSeq = %d, want 5", m.LastJournalSeq)
				}
			},
		},
		{
			name:        "TurnStarted folds to running and sets ActiveTurnID",
			start:       SessionMeta{SessionID: sid, State: StateIdle},
			ev:          event.TurnStarted{Header: hdrTurn(sid, tid), Message: userMsg("go")},
			seq:         6,
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.State != StateRunning {
					t.Errorf("State = %q, want running", m.State)
				}
				if m.ActiveTurnID != tid {
					t.Errorf("ActiveTurnID = %v, want %v", m.ActiveTurnID, tid)
				}
				if m.LastJournalSeq != 6 {
					t.Errorf("LastJournalSeq = %d, want 6", m.LastJournalSeq)
				}
			},
		},
		{
			name:        "GateOpened folds to waiting_on_gate and sets WaitingGateID",
			start:       SessionMeta{SessionID: sid, State: StateRunning, ActiveTurnID: tid},
			ev:          event.GateOpened{Header: hdr(sid), Gate: gate.Gate{ID: gid}},
			seq:         7,
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.State != StateWaitingOnGate {
					t.Errorf("State = %q, want waiting_on_gate", m.State)
				}
				if m.WaitingGateID != gid {
					t.Errorf("WaitingGateID = %v, want %v", m.WaitingGateID, gid)
				}
			},
		},
		{
			name:        "GateResolved clears gate and returns to running with an active turn",
			start:       SessionMeta{SessionID: sid, State: StateWaitingOnGate, WaitingGateID: gid, ActiveTurnID: tid},
			ev:          event.GateResolved{Header: hdr(sid), GateID: gid},
			seq:         8,
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if !m.WaitingGateID.IsZero() {
					t.Errorf("WaitingGateID = %v, want cleared", m.WaitingGateID)
				}
				if m.State != StateRunning {
					t.Errorf("State = %q, want running (active turn)", m.State)
				}
			},
		},
		{
			name:        "GateResolved with no active turn returns to idle",
			start:       SessionMeta{SessionID: sid, State: StateWaitingOnGate, WaitingGateID: gid},
			ev:          event.GateResolved{Header: hdr(sid), GateID: gid},
			seq:         9,
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if !m.WaitingGateID.IsZero() {
					t.Errorf("WaitingGateID = %v, want cleared", m.WaitingGateID)
				}
				if m.State != StateIdle {
					t.Errorf("State = %q, want idle (no active turn)", m.State)
				}
			},
		},
		{
			name:        "TurnDone folds to idle, records LastTurn, clears ActiveTurnID",
			start:       SessionMeta{SessionID: sid, State: StateRunning, ActiveTurnID: tid},
			ev:          event.TurnDone{Header: hdrTurn(sid, tid)},
			seq:         10,
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.State != StateIdle {
					t.Errorf("State = %q, want idle", m.State)
				}
				if !m.ActiveTurnID.IsZero() {
					t.Errorf("ActiveTurnID = %v, want cleared", m.ActiveTurnID)
				}
				if m.LastTurn == nil || m.LastTurn.JournalSeq != 10 || len(m.LastTurn.Event) == 0 {
					t.Errorf("LastTurn = %+v, want summary at seq 10 with event bytes", m.LastTurn)
				}
			},
		},
		{
			name:        "TurnFailed folds to failed and records LastTurn",
			start:       SessionMeta{SessionID: sid, State: StateRunning},
			ev:          event.TurnFailed{Header: hdrTurn(sid, tid)},
			seq:         11,
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.State != StateFailed {
					t.Errorf("State = %q, want failed", m.State)
				}
				if m.LastTurn == nil || m.LastTurn.JournalSeq != 11 {
					t.Errorf("LastTurn = %+v, want summary at seq 11", m.LastTurn)
				}
			},
		},
		{
			name:        "TurnInterrupted folds to interrupted",
			start:       SessionMeta{SessionID: sid, State: StateRunning},
			ev:          event.TurnInterrupted{Header: hdrTurn(sid, tid)},
			seq:         12,
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.State != StateInterrupted {
					t.Errorf("State = %q, want interrupted", m.State)
				}
			},
		},
		{
			name:        "SessionStopped folds to stopped (terminal wins)",
			start:       SessionMeta{SessionID: sid, State: StateRunning, Status: StatusActive},
			ev:          event.SessionStopped{Header: hdr(sid)},
			seq:         13,
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.State != StateStopped {
					t.Errorf("State = %q, want stopped", m.State)
				}
				if m.Status != StatusStopped {
					t.Errorf("Status = %q, want stopped", m.Status)
				}
			},
		},
		{
			name:        "StepDone records LastStep and bumps LastJournalSeq",
			start:       SessionMeta{SessionID: sid, LastJournalSeq: 3},
			ev:          event.StepDone{Header: hdr(sid)},
			seq:         20,
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.LastStep == nil || m.LastStep.JournalSeq != 20 || len(m.LastStep.Event) == 0 {
					t.Errorf("LastStep = %+v, want summary at seq 20 with event bytes", m.LastStep)
				}
				if m.LastJournalSeq != 20 {
					t.Errorf("LastJournalSeq = %d, want 20", m.LastJournalSeq)
				}
			},
		},
		{
			name:        "unrelated event is a no-op leaving state untouched",
			start:       SessionMeta{SessionID: sid, State: StateRunning, LastJournalSeq: 50},
			ev:          event.SessionActive{Header: hdr(sid)},
			seq:         99,
			wantChanged: false,
			check: func(t *testing.T, m SessionMeta) {
				if m.State != StateRunning {
					t.Errorf("State = %q, want unchanged running", m.State)
				}
				if m.LastJournalSeq != 50 {
					t.Errorf("LastJournalSeq = %d, want unchanged 50 (no-op)", m.LastJournalSeq)
				}
			},
		},
		{
			name:        "lower additive seq requests authoritative repair",
			start:       SessionMeta{SessionID: sid, State: StateIdle, LastJournalSeq: 100},
			ev:          event.StepDone{Header: hdr(sid)},
			seq:         5,
			wantChanged: false,
			wantOrder:   true,
			check: func(t *testing.T, m SessionMeta) {
				if m.LastJournalSeq != 100 {
					t.Errorf("LastJournalSeq = %d, want 100 (max wins)", m.LastJournalSeq)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, changed, err := applyEvent(tt.start, tt.ev, tt.seq, fixedClock(clock))
			var ordering *CatalogOrderingError
			if tt.wantOrder {
				if !errors.As(err, &ordering) {
					t.Fatalf("applyEvent error = %T %v, want *CatalogOrderingError", err, err)
				}
			} else if err != nil {
				t.Fatalf("applyEvent error = %v", err)
			}
			if changed != tt.wantChanged {
				t.Fatalf("applyEvent changed = %v, want %v", changed, tt.wantChanged)
			}
			tt.check(t, got)
		})
	}
}

// TestSessionMetaStatusRoundTrip proves a SessionMeta carrying the status-fold fields —
// State, ActiveTurnID, WaitingGateID, and the codec-safe LastTurn/LastStep summaries —
// survives the package's DisallowUnknownFields decode path (decodeSessionMeta) and that
// a summary's opaque event bytes reconstruct losslessly via event.UnmarshalEvent (the
// A4 RawMessage resolution).
func TestSessionMetaStatusRoundTrip(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x21)
	tid := fixedUUID(0x22)
	gid := fixedUUID(0x23)
	lid := fixedUUID(0x25)

	// Fully-valid turn/step events so the reconstructed event survives ValidateEvent (run
	// by UnmarshalEvent): turnProfile needs SessionID+LoopID+TurnID; stepProfile also needs
	// StepID; both need EventID.
	turnEv := event.TurnDone{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid},
		EventID:     fixedUUID(0x26),
	}}
	stepEv := event.StepDone{Header: event.Header{
		Coordinates: identity.Coordinates{SessionID: sid, LoopID: lid, TurnID: tid, StepID: fixedUUID(0x27)},
		EventID:     fixedUUID(0x28),
	}}
	turnSum, err := newEventSummary(turnEv, 10)
	if err != nil {
		t.Fatalf("newEventSummary(TurnDone) = %v", err)
	}
	stepSum, err := newEventSummary(stepEv, 11)
	if err != nil {
		t.Fatalf("newEventSummary(StepDone) = %v", err)
	}
	meta := SessionMeta{
		SessionID:      sid,
		Status:         StatusActive,
		State:          StateWaitingOnGate,
		LastJournalSeq: 12,
		ActiveTurnID:   tid,
		WaitingGateID:  gid,
		LastTurn:       turnSum,
		LastStep:       stepSum,
	}

	data, err := encodeSessionMeta(meta)
	if err != nil {
		t.Fatalf("encodeSessionMeta = %v", err)
	}
	got, err := decodeSessionMeta(data)
	if err != nil {
		t.Fatalf("decodeSessionMeta (DisallowUnknownFields) = %v, want nil", err)
	}
	if got.State != StateWaitingOnGate || got.LastJournalSeq != 12 {
		t.Errorf("decoded state/seq = %q/%d, want waiting_on_gate/12", got.State, got.LastJournalSeq)
	}
	if got.ActiveTurnID != tid || got.WaitingGateID != gid {
		t.Errorf("decoded ids = %v/%v, want %v/%v", got.ActiveTurnID, got.WaitingGateID, tid, gid)
	}
	if got.LastTurn == nil || got.LastTurn.JournalSeq != 10 {
		t.Fatalf("decoded LastTurn = %+v, want summary at seq 10", got.LastTurn)
	}
	if got.LastStep == nil || got.LastStep.JournalSeq != 11 {
		t.Fatalf("decoded LastStep = %+v, want summary at seq 11", got.LastStep)
	}
	// Lossless reconstruction: the opaque event bytes decode back to the concrete event.
	ev, err := event.UnmarshalEvent(got.LastTurn.Event)
	if err != nil {
		t.Fatalf("UnmarshalEvent(LastTurn.Event) = %v", err)
	}
	if _, ok := ev.(event.TurnDone); !ok {
		t.Errorf("reconstructed LastTurn event = %T, want event.TurnDone", ev)
	}
}

// TestReadMeta proves ReadMeta is a projection-only single-key read: an absent id yields
// found=false with no error, and a present id yields the projected meta.
func TestReadMeta(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	c, sid := newTestCatalog(t, now)
	ctx := context.Background()

	if _, found, err := c.ReadMeta(ctx, fixedUUID(0x77)); err != nil || found {
		t.Fatalf("ReadMeta(absent) = found=%v err=%v, want found=false nil", found, err)
	}

	started := event.SessionStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now},
		Config: event.ConfigFingerprint{ModelID: "m"},
	}
	if err := c.UpdateOnEvent(ctx, started, 4); err != nil {
		t.Fatalf("UpdateOnEvent = %v", err)
	}
	meta, found, err := c.ReadMeta(ctx, sid)
	if err != nil || !found {
		t.Fatalf("ReadMeta(present) = found=%v err=%v, want found=true nil", found, err)
	}
	if meta.SessionID != sid || meta.State != StateIdle || meta.Status != StatusActive {
		t.Errorf("ReadMeta present meta = %+v, want idle/active for %v", meta, sid)
	}
	if meta.LastJournalSeq != 4 {
		t.Errorf("LastJournalSeq = %d, want 4 (threaded from UpdateOnEvent)", meta.LastJournalSeq)
	}
}

// TestDeriveTitle covers the title derivation: first non-empty line of concatenated
// text, rune-truncated; nil/empty yields "".
func TestDeriveTitle(t *testing.T) {
	t.Parallel()
	long := make([]rune, titleMaxLen+10)
	for i := range long {
		long[i] = 'a'
	}
	tests := []struct {
		name string
		msg  *content.UserMessage
		want string
	}{
		{name: "nil message", msg: nil, want: ""},
		{name: "single line", msg: userMsg("do the thing"), want: "do the thing"},
		{name: "multi-line takes first non-empty", msg: userMsg("\n\n  first\nsecond"), want: "first"},
		{name: "blank message", msg: userMsg("   \n  "), want: ""},
		{name: "no text blocks", msg: &content.UserMessage{Message: content.Message{Role: content.RoleUser}}, want: ""},
		{name: "truncated to max runes", msg: userMsg(string(long)), want: string(long[:titleMaxLen])},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := deriveTitle(tt.msg); got != tt.want {
				t.Errorf("deriveTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecodeSessionMeta covers the fail-closed decode boundary: valid round-trips, every
// malformed shape errors rather than guessing a record.
func TestDecodeSessionMeta(t *testing.T) {
	t.Parallel()
	valid := SessionMeta{
		SessionID:    fixedUUID(0x07),
		Title:        "t",
		CreatedAt:    time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC),
		LastActiveAt: time.Date(2026, 6, 21, 13, 0, 0, 0, time.UTC),
		Status:       StatusActive,
		LoopCount:    2,
	}
	validBytes, err := encodeSessionMeta(valid)
	if err != nil {
		t.Fatalf("encodeSessionMeta: %v", err)
	}
	tests := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{name: "valid round-trips", data: validBytes},
		{name: "empty bytes", data: []byte{}, wantErr: true},
		{name: "not an object", data: []byte(`42`), wantErr: true},
		{name: "unknown field", data: []byte(`{"session_id":"x","bogus":1}`), wantErr: true},
		{name: "trailing data", data: []byte(`{}{}`), wantErr: true},
		{name: "zero-value object decodes", data: []byte(`{}`)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeSessionMeta(tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeSessionMeta() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestCatalogOptions covers the constructor knobs: each ignores a zero/empty/nil value
// and applies a valid one.
func TestCatalogOptions(t *testing.T) {
	t.Parallel()
	opener := &fakeOpener{}
	tests := []struct {
		name   string
		apply  func(*catalogOptions)
		assert func(*testing.T, catalogOptions)
	}{
		{
			name:  "clock applied",
			apply: func(o *catalogOptions) { WithCatalogClock(fixedClock(time.Unix(7, 0)))(o) },
			assert: func(t *testing.T, o catalogOptions) {
				if got := o.now(); !got.Equal(time.Unix(7, 0)) {
					t.Errorf("now() = %v, want 7s epoch", got)
				}
			},
		},
		{
			name:  "nil clock ignored",
			apply: func(o *catalogOptions) { WithCatalogClock(nil)(o) },
			assert: func(t *testing.T, o catalogOptions) {
				if o.now == nil {
					t.Fatal("now nil after WithCatalogClock(nil); default must be retained")
				}
			},
		},
		{
			name:  "nil logger ignored",
			apply: func(o *catalogOptions) { WithCatalogLogger(nil)(o) },
			assert: func(t *testing.T, o catalogOptions) {
				if o.log == nil {
					t.Fatal("log nil after WithCatalogLogger(nil); default must be retained")
				}
			},
		},
		{
			name:  "replayer applied",
			apply: func(o *catalogOptions) { WithCatalogReplayer(opener)(o) },
			assert: func(t *testing.T, o catalogOptions) {
				if o.opener != opener {
					t.Error("opener not applied by WithCatalogReplayer")
				}
			},
		},
		{
			name:  "nil replayer ignored",
			apply: func(o *catalogOptions) { WithCatalogReplayer(nil)(o) },
			assert: func(t *testing.T, o catalogOptions) {
				if o.opener == nil {
					t.Fatal("opener nil after WithCatalogReplayer(nil); default must be retained")
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := catalogOptions{now: time.Now, log: nopCatalogLogger{}, opener: opener}
			tt.apply(&o)
			tt.assert(t, o)
		})
	}
}

// newTestCatalog opens a Store over a fresh memstore and returns a Catalog with the
// injected clock, plus the session id used across the round-trip tests.
func newTestCatalog(t *testing.T, now time.Time) (*Catalog, uuid.UUID) {
	t.Helper()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store.OpenCatalog(WithCatalogClock(fixedClock(now))), fixedUUID(0x11)
}

// TestCatalogUpsertRoundTrip proves an UpdateOnEvent upserts a session's entry that a
// subsequent load and ListSessions both read back.
func TestCatalogUpsertRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	c, sid := newTestCatalog(t, now)
	ctx := context.Background()

	started := event.SessionStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now},
		Config: event.ConfigFingerprint{ModelID: "m"},
	}
	if err := c.UpdateOnEvent(ctx, started, 0); err != nil {
		t.Fatalf("UpdateOnEvent = %v, want nil", err)
	}

	meta, rev, err := c.load(ctx, sid)
	if err != nil {
		t.Fatalf("load = %v, want nil", err)
	}
	if rev == 0 {
		t.Error("rev = 0 after upsert, want a committed revision")
	}
	if meta.SessionID != sid || meta.Status != StatusActive {
		t.Errorf("loaded meta = %+v, want active/%v", meta, sid)
	}

	metas, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions = %v, want nil", err)
	}
	if len(metas) != 1 || metas[0].SessionID != sid {
		t.Errorf("ListSessions = %+v, want one entry for %v", metas, sid)
	}
}

// TestCatalogUpdateSemantics proves a sequence of catalog events folds into ONE entry
// (an upsert merges into the existing record rather than replacing it wholesale).
func TestCatalogUpdateSemantics(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 21, 11, 0, 0, 0, time.UTC)
	c, sid := newTestCatalog(t, now)
	ctx := context.Background()

	evs := []event.Event{
		event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now}, Config: event.ConfigFingerprint{ModelID: "m"}},
		event.TurnStarted{Header: hdr(sid), Message: userMsg("first task")},
		event.LoopStarted{Header: hdr(sid)},
		event.SessionStopped{Header: hdr(sid)},
	}
	for _, ev := range evs {
		if err := c.UpdateOnEvent(ctx, ev, 0); err != nil {
			t.Fatalf("UpdateOnEvent(%T) = %v, want nil", ev, err)
		}
	}

	meta, _, err := c.load(ctx, sid)
	if err != nil {
		t.Fatalf("load = %v", err)
	}
	if meta.Title != "first task" {
		t.Errorf("Title = %q, want first task (folded)", meta.Title)
	}
	if meta.LoopCount != 2 {
		t.Errorf("LoopCount = %d, want 2 (primary + one LoopStarted)", meta.LoopCount)
	}
	if meta.Status != StatusStopped {
		t.Errorf("Status = %q, want stopped (final)", meta.Status)
	}
	if !meta.LastActiveAt.Equal(now) {
		t.Errorf("LastActiveAt = %v, want %v (bumped by TurnStarted)", meta.LastActiveAt, now)
	}
	if !meta.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v (kept from SessionStarted)", meta.CreatedAt, now)
	}

	// Only one entry despite four upserts.
	metas, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions = %v", err)
	}
	if len(metas) != 1 {
		t.Errorf("ListSessions = %d entries, want 1 (folded into one)", len(metas))
	}
}

// TestCatalogListOrder proves ListSessions returns entries sorted ascending by session
// id (the storage KV.Keys canonical order), independent of insertion order.
func TestCatalogListOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := store.OpenCatalog(WithCatalogClock(fixedClock(now)))
	ctx := context.Background()

	// Insert out of sorted order: 0x33, 0x11, 0x22.
	for _, seed := range []byte{0x33, 0x11, 0x22} {
		sid := fixedUUID(seed)
		ev := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now}, Config: event.ConfigFingerprint{ModelID: "m"}}
		if err := c.UpdateOnEvent(ctx, ev, 0); err != nil {
			t.Fatalf("UpdateOnEvent = %v", err)
		}
	}

	metas, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions = %v", err)
	}
	want := []uuid.UUID{fixedUUID(0x11), fixedUUID(0x22), fixedUUID(0x33)}
	if len(metas) != len(want) {
		t.Fatalf("ListSessions = %d entries, want %d", len(metas), len(want))
	}
	for i, w := range want {
		if metas[i].SessionID != w {
			t.Errorf("metas[%d].SessionID = %v, want %v (ascending)", i, metas[i].SessionID, w)
		}
	}
}

// TestCatalogListEmpty proves ListSessions returns an empty slice (not an error) for a
// catalog with no entries.
func TestCatalogListEmpty(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := store.OpenCatalog()
	metas, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions on empty = %v, want nil", err)
	}
	if len(metas) != 0 {
		t.Errorf("ListSessions on empty = %d entries, want 0", len(metas))
	}
}

// TestCatalogGetAbsent proves load treats an absent key as "create": a zero meta, rev 0,
// and NO error (the upsert path relies on this to distinguish create from a read fault).
func TestCatalogGetAbsent(t *testing.T) {
	t.Parallel()
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := store.OpenCatalog()
	meta, rev, err := c.load(context.Background(), fixedUUID(0x44))
	if err != nil {
		t.Fatalf("load absent = %v, want nil", err)
	}
	if rev != 0 {
		t.Errorf("rev = %d, want 0 for absent", rev)
	}
	if !reflect.DeepEqual(meta, SessionMeta{}) {
		t.Errorf("meta = %+v, want zero for absent", meta)
	}
}

// TestCatalogUpdateBestEffort proves UpdateOnEvent (a) NEVER returns an error and
// logs+swallows a KV read/write failure, and (b) skips KV I/O entirely for a
// non-catalog event.
func TestCatalogUpdateBestEffort(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11)
	now := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	started := event.SessionStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now},
		Config: event.ConfigFingerprint{ModelID: "m"},
	}

	t.Run("KV write failure is logged and swallowed", func(t *testing.T) {
		t.Parallel()
		kv := newFakeKV()
		kv.putErr = errors.New("kv down")
		log := &recordingLogger{}
		c := &Catalog{kv: kv, now: fixedClock(now), log: log}
		if err := c.UpdateOnEvent(context.Background(), started, 0); err != nil {
			t.Fatalf("UpdateOnEvent = %v, want nil (best-effort)", err)
		}
		if log.count() != 1 {
			t.Fatalf("logged %d errors, want 1", log.count())
		}
		var we *CatalogWriteError
		if !errors.As(log.errs[0], &we) {
			t.Errorf("logged error %v is not *CatalogWriteError", log.errs[0])
		}
	})

	t.Run("KV read failure is logged and swallowed", func(t *testing.T) {
		t.Parallel()
		kv := newFakeKV()
		kv.getErr = errors.New("kv read down")
		log := &recordingLogger{}
		c := &Catalog{kv: kv, now: fixedClock(now), log: log}
		if err := c.UpdateOnEvent(context.Background(), started, 0); err != nil {
			t.Fatalf("UpdateOnEvent = %v, want nil (best-effort)", err)
		}
		if log.count() != 1 {
			t.Fatalf("logged %d errors, want 1", log.count())
		}
		var re *CatalogReadError
		if !errors.As(log.errs[0], &re) {
			t.Errorf("logged error %v is not *CatalogReadError", log.errs[0])
		}
		if kv.puts != 0 {
			t.Errorf("puts = %d, want 0 (read failed before write)", kv.puts)
		}
	})

	t.Run("non-catalog event skips KV entirely", func(t *testing.T) {
		t.Parallel()
		kv := newFakeKV()
		c := &Catalog{kv: kv, now: fixedClock(now), log: nopCatalogLogger{}}
		ev := event.SessionActive{Header: hdr(sid)}
		if err := c.UpdateOnEvent(context.Background(), ev, 0); err != nil {
			t.Fatalf("UpdateOnEvent = %v, want nil", err)
		}
		if kv.puts != 0 || kv.gets != 0 {
			t.Errorf("puts=%d gets=%d, want 0/0 (no-op event skips KV)", kv.puts, kv.gets)
		}
	})
}

// TestCatalogUpsertRetriesOnConflict proves the read-modify-write loop retries on a
// *storage.ConflictError (a concurrent writer advanced the rev) and eventually lands
// the update — matching the NATS catalog's last-write-wins "the update lands" semantics.
func TestCatalogUpsertRetriesOnConflict(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11)
	now := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	kv := newFakeKV()
	kv.conflictN = 2 // first two Puts conflict, third succeeds
	log := &recordingLogger{}
	c := &Catalog{kv: kv, now: fixedClock(now), log: log}
	started := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now}, Config: event.ConfigFingerprint{ModelID: "m"}}

	if err := c.UpdateOnEvent(context.Background(), started, 0); err != nil {
		t.Fatalf("UpdateOnEvent = %v, want nil", err)
	}
	if log.count() != 0 {
		t.Errorf("logged %d errors, want 0 (retry should succeed silently)", log.count())
	}
	if kv.puts != 3 {
		t.Errorf("puts = %d, want 3 (2 conflicts + 1 success)", kv.puts)
	}
	// The update landed.
	if _, ok := kv.entries[sessionsPrefix+sid.String()]; !ok {
		t.Error("no entry stored after retry; update was lost")
	}
}

// TestCatalogUpsertConflictExhausted proves that when every Put conflicts, UpdateOnEvent
// exhausts its bounded retries and logs+swallows a typed *CatalogConflictError (still
// best-effort: never returned to the caller).
func TestCatalogUpsertConflictExhausted(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11)
	now := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	kv := newFakeKV()
	kv.conflictN = 1 << 30 // effectively always conflicts
	log := &recordingLogger{}
	c := &Catalog{kv: kv, now: fixedClock(now), log: log}
	started := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now}, Config: event.ConfigFingerprint{ModelID: "m"}}

	if err := c.UpdateOnEvent(context.Background(), started, 0); err != nil {
		t.Fatalf("UpdateOnEvent = %v, want nil (best-effort even on exhaustion)", err)
	}
	if log.count() != 1 {
		t.Fatalf("logged %d errors, want 1", log.count())
	}
	var ce *CatalogConflictError
	if !errors.As(log.errs[0], &ce) {
		t.Errorf("logged error %v is not *CatalogConflictError", log.errs[0])
	}
	if kv.puts != catalogMaxCASRetries {
		t.Errorf("puts = %d, want %d (bounded retries)", kv.puts, catalogMaxCASRetries)
	}
}

// TestCatalogConcurrentDistinctSessions proves concurrent updates to DIFFERENT sessions
// over the real memstore all land exactly, with no cross-key corruption: N goroutines each
// upsert a distinct session, and ListSessions then reports all N as active. There is no
// per-key contention, so the rev-CAS always wins first try. Run under -race.
func TestCatalogConcurrentDistinctSessions(t *testing.T) {
	t.Parallel()
	const n = 24
	now := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	store, err := Open(memstore.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := store.OpenCatalog(WithCatalogClock(fixedClock(now)))
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sid := fixedUUID(byte(i + 1))
			ev := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now}, Config: event.ConfigFingerprint{ModelID: "m"}}
			if err := c.UpdateOnEvent(ctx, ev, 0); err != nil {
				t.Errorf("concurrent UpdateOnEvent = %v, want nil", err)
			}
		}()
	}
	wg.Wait()

	metas, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions = %v", err)
	}
	if len(metas) != n {
		t.Errorf("ListSessions = %d entries, want %d", len(metas), n)
	}
	for _, m := range metas {
		if m.Status != StatusActive {
			t.Errorf("session %v status = %q, want active", m.SessionID, m.Status)
		}
	}
}

// TestCatalogConcurrentSameSession proves concurrent updates on the SAME session are
// race-safe and never corrupt the entry. UpdateOnEvent is best-effort (matching the NATS
// catalog's last-write-wins Put, which also loses concurrent increments): the rev-CAS retry
// loop is bounded, so a wildly contended increment may be dropped rather than serialized.
// The invariant is therefore that no call errors, the stored entry stays valid and active,
// and the folded LoopCount lands within [2, 1+N] — NOT exact serializability (which the
// single-writer appender provides in production, not this synthetic stress). Run under -race.
func TestCatalogConcurrentSameSession(t *testing.T) {
	t.Parallel()
	const n = 24
	now := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	c, sid := newTestCatalog(t, now)
	ctx := context.Background()

	started := event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now}, Config: event.ConfigFingerprint{ModelID: "m"}}
	if err := c.UpdateOnEvent(ctx, started, 0); err != nil {
		t.Fatalf("seed UpdateOnEvent = %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.UpdateOnEvent(ctx, event.LoopStarted{Header: hdr(sid)}, 0); err != nil {
				t.Errorf("concurrent UpdateOnEvent = %v, want nil (best-effort)", err)
			}
		}()
	}
	wg.Wait()

	meta, rev, err := c.load(ctx, sid)
	if err != nil {
		t.Fatalf("load = %v (entry must stay valid/decodable)", err)
	}
	if rev == 0 {
		t.Error("rev = 0, want a committed revision after concurrent updates")
	}
	if meta.Status != StatusActive {
		t.Errorf("Status = %q, want active (never corrupted)", meta.Status)
	}
	if meta.LoopCount < 2 || meta.LoopCount > 1+n {
		t.Errorf("LoopCount = %d, want within [2, %d] (best-effort, no corruption)", meta.LoopCount, 1+n)
	}
}

// TestCatalogRepair covers RepairCatalog's three paths: a rebuild from a scripted stream
// stores and returns the folded meta; a stream with no SessionStarted yields
// *EmptySessionError; and a Catalog with no opener yields a typed *CatalogReadError that
// unwraps to errNoReplayer.
func TestCatalogRepair(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 21, 13, 0, 0, 0, time.UTC)
	sid := fixedUUID(0x55)

	t.Run("rebuild from stream stores and returns folded meta", func(t *testing.T) {
		t.Parallel()
		store, err := Open(memstore.New())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		opener := &fakeOpener{events: []event.Event{
			event.SessionStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now}, Config: event.ConfigFingerprint{ModelID: "m"}},
			event.TurnStarted{Header: hdr(sid), Message: userMsg("rebuilt title")},
			event.LoopStarted{Header: hdr(sid)},
		}}
		c := store.OpenCatalog(WithCatalogClock(fixedClock(now)), WithCatalogReplayer(opener))

		meta, err := c.RepairCatalog(context.Background(), sid)
		if err != nil {
			t.Fatalf("RepairCatalog = %v, want nil", err)
		}
		if meta.Title != "rebuilt title" || meta.LoopCount != 2 || meta.Status != StatusActive {
			t.Errorf("rebuilt meta = %+v, want title/loop=2/active", meta)
		}
		// Persisted: a subsequent load reads it back.
		got, rev, err := c.load(context.Background(), sid)
		if err != nil || rev == 0 {
			t.Fatalf("load after repair = %+v rev=%d err=%v", got, rev, err)
		}
		if got.Title != "rebuilt title" {
			t.Errorf("persisted title = %q, want rebuilt title", got.Title)
		}
	})

	t.Run("no SessionStarted yields EmptySessionError", func(t *testing.T) {
		t.Parallel()
		store, err := Open(memstore.New())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		opener := &fakeOpener{events: []event.Event{
			event.StepDone{Header: hdr(sid)}, // activity but no SessionStarted
		}}
		c := store.OpenCatalog(WithCatalogReplayer(opener))
		_, err = c.RepairCatalog(context.Background(), sid)
		var ese *EmptySessionError
		if !errors.As(err, &ese) {
			t.Fatalf("RepairCatalog = %v, want *EmptySessionError", err)
		}
		if ese.SessionID != sid {
			t.Errorf("EmptySessionError.SessionID = %v, want %v", ese.SessionID, sid)
		}
	})

	t.Run("no opener yields typed CatalogReadError", func(t *testing.T) {
		t.Parallel()
		c := &Catalog{kv: newFakeKV(), now: time.Now, log: nopCatalogLogger{}, opener: nil}
		_, err := c.RepairCatalog(context.Background(), sid)
		var re *CatalogReadError
		if !errors.As(err, &re) {
			t.Fatalf("RepairCatalog with no opener = %v, want *CatalogReadError", err)
		}
		if !errors.Is(err, errNoReplayer) {
			t.Errorf("error does not unwrap to errNoReplayer: %v", err)
		}
	})
}
