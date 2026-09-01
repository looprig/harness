// Package catalogreader is the concrete read-plane adapter behind serve.Reader. It
// implements serve's list/status/journal reads over the session store's catalog
// projection and event replayer, translating persisted SessionMeta / journal events
// into serve's transport DTOs.
//
// It lives in its OWN package (not pkg/serve) on purpose: serve obeys strict
// Dependency Inversion and may not import any store package, so the adapter that DOES
// import pkg/sessionstore sits behind serve's Reader interface here. serve depends on
// the interface; the composition root wires this adapter in. Because this is a
// separate directory, serve's dependency-guard test (which scans only the serve
// directory) stays green.
package catalogreader

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/serve"
	"github.com/looprig/harness/pkg/sessionstore"
	harnesssessionwire "github.com/looprig/harness/pkg/sessionwire"
)

// Reader adapts a sessionstore Catalog + Store to serve.Reader. The Catalog backs the
// replay-free list and status reads (ListSessions / ReadMeta); the Store backs the
// journal read (OpenEventReplayer). It holds no per-request state — one Reader serves
// every read — so it is safe for concurrent use to the extent its backends are.
type Reader struct {
	catalog   *sessionstore.Catalog
	store     *sessionstore.Store
	authority harnesssessionwire.ReadAuthority
	residency coresessionwire.SessionResidency
}

// coreProjectionAbsence records why a supported catalog row has no truthful
// representation in the stricter Core read contract. It is deliberately an
// internal compatibility fact: legacy serve DTOs remain available, while a
// future Core response path cannot mistake an omitted projection for a zero
// Core value or invent state/time facts to fill the gap.
type coreProjectionAbsence uint8

const coreProjectionPresent coreProjectionAbsence = 0

const (
	coreProjectionMissingState coreProjectionAbsence = 1 << iota
	coreProjectionMissingActivity
)

type optionalSessionSummary struct {
	value   coresessionwire.SessionSummary
	absence coreProjectionAbsence
}

type optionalSessionStatus struct {
	value   coresessionwire.SessionStatus
	absence coreProjectionAbsence
}

// compatibilityProjectionError reports an invalid source shape without
// including catalog payload values in its error text.
type compatibilityProjectionError struct{ field string }

func (e *compatibilityProjectionError) Error() string {
	return "catalogreader: invalid compatibility projection " + e.field
}

// PrivateEventError reports an internal event encountered at a public serve
// reconstruction boundary. It contains no event payload.
type PrivateEventError struct{ Visibility event.EventVisibility }

func (e *PrivateEventError) Error() string { return "catalogreader: non-public event refused" }

// compile-time proof the adapter satisfies the serve read plane.
var _ serve.Reader = (*Reader)(nil)

// New builds a Reader over the supplied catalog and store, wired at the composition
// root. Both are required; a nil argument is a programming error the composition root
// does not make and is not defended against here.
func New(catalog *sessionstore.Catalog, store *sessionstore.Store) *Reader {
	return NewScoped(
		catalog,
		store,
		harnesssessionwire.ReadAuthority{TenantID: "legacy-single-tenant-v1", AgentID: "legacy-harness"},
		coresessionwire.SessionResidencyCold,
	)
}

// NewScoped builds the same legacy serve.Reader while requiring the caller's
// Core tenant/agent authority and the independently observed residency. The
// values affect only Core validation/projection; legacy serve DTO bytes remain
// unchanged during migration.
func NewScoped(catalog *sessionstore.Catalog, store *sessionstore.Store, authority harnesssessionwire.ReadAuthority, residency coresessionwire.SessionResidency) *Reader {
	return &Reader{catalog: catalog, store: store, authority: authority, residency: residency}
}

// ListSessions reads EVERY catalog entry, stable-sorts it by last-active descending
// then session id ascending (the picker's most-recent-first order, SPEC §6), and
// returns the requested [Skip, Skip+Limit) window as summaries. Done reports the end
// of the list was reached (the window returned fewer than Limit entries); NextSkip is
// the resume offset when more may remain. A backend read failure is wrapped in a
// serve.StoreReadError (mapped to 500 by the handler). It trusts the Page — the HTTP
// boundary already validated Skip/Limit — but is bounds-safe against an out-of-range
// Skip.
func (r *Reader) ListSessions(ctx context.Context, page serve.Page) (serve.SessionList, error) {
	if err := harnesssessionwire.ValidateReadAuthority(r.authority); err != nil {
		return serve.SessionList{}, serve.StoreReadError{Op: "project", Cause: err}
	}
	metas, err := r.catalog.ListSessions(ctx)
	if err != nil {
		return serve.SessionList{}, serve.StoreReadError{Op: "list", Cause: err}
	}

	sort.SliceStable(metas, func(i, j int) bool {
		if !metas[i].LastActiveAt.Equal(metas[j].LastActiveAt) {
			return metas[i].LastActiveAt.After(metas[j].LastActiveAt)
		}
		return metas[i].SessionID.String() < metas[j].SessionID.String()
	})

	total := len(metas)
	lo := page.Skip
	if lo < 0 {
		lo = 0
	}
	if lo > total {
		lo = total
	}
	hi := lo + page.Limit
	if page.Limit < 0 || hi > total {
		hi = total
	}
	window := metas[lo:hi]
	summaries := make([]serve.SessionSummary, 0, len(window))
	coreSummaries := make([]coresessionwire.SessionSummary, 0, len(window))
	for _, m := range window {
		projected, projectErr := projectLegacySummary(r.authority, m)
		if projectErr != nil {
			return serve.SessionList{}, serve.StoreReadError{Op: "project", Cause: projectErr}
		}
		if projected.absence == coreProjectionPresent {
			coreSummaries = append(coreSummaries, projected.value)
		}
		summaries = append(summaries, serve.SessionSummary{
			SessionID:    m.SessionID,
			State:        string(m.State),
			Title:        m.Title,
			CreatedAt:    m.CreatedAt,
			LastActiveAt: m.LastActiveAt,
		})
	}
	if err := (coresessionwire.SessionPage{Sessions: coreSummaries}).Validate(); err != nil {
		return serve.SessionList{}, serve.StoreReadError{Op: "project", Cause: err}
	}

	list := serve.SessionList{
		Sessions: summaries,
		Skip:     page.Skip,
		Limit:    page.Limit,
		Done:     len(window) < page.Limit,
	}
	if !list.Done {
		list.NextSkip = page.Skip + len(window)
	}
	return list, nil
}

// ReadStatus reads one session's projected status by a SINGLE catalog load — NEVER a
// replay. An absent entry yields a serve.SessionNotFoundError (mapped to 404); a
// backend read failure yields a serve.StoreReadError (500). The LastTurn/LastStep
// summaries are reconstructed losslessly from the meta's codec-safe eventSummary raw
// bytes via event.UnmarshalEvent, so the DTO carries the concrete event, not a lossy
// projection.
func (r *Reader) ReadStatus(ctx context.Context, id uuid.UUID) (serve.SessionStatus, error) {
	scope := r.readScope(id)
	if err := harnesssessionwire.ValidateReadScope(scope); err != nil {
		return serve.SessionStatus{}, serve.StoreReadError{Op: "project", Cause: err}
	}
	meta, found, err := r.catalog.ReadMeta(ctx, id)
	if err != nil {
		return serve.SessionStatus{}, serve.StoreReadError{Op: "get", Cause: err}
	}
	if !found {
		return serve.SessionStatus{}, serve.SessionNotFoundError{SessionID: id}
	}
	_, err = projectLegacyStatus(scope, meta)
	if err != nil {
		return serve.SessionStatus{}, serve.StoreReadError{Op: "project", Cause: err}
	}
	state := string(meta.State)

	status := serve.SessionStatus{
		SessionID:      meta.SessionID,
		State:          state,
		LastJournalSeq: meta.LastJournalSeq,
		ActiveTurnID:   meta.ActiveTurnID,
		WaitingGateID:  meta.WaitingGateID,
		UpdatedAt:      meta.LastActiveAt,
	}
	if meta.LastTurn != nil {
		se, derr := reconstruct(meta.LastTurn.JournalSeq, meta.LastTurn.Event)
		if derr != nil {
			return serve.SessionStatus{}, serve.StoreReadError{Op: "decode", Cause: derr}
		}
		status.LastTurn = se
	}
	if meta.LastStep != nil {
		se, derr := reconstruct(meta.LastStep.JournalSeq, meta.LastStep.Event)
		if derr != nil {
			return serve.SessionStatus{}, serve.StoreReadError{Op: "decode", Cause: derr}
		}
		status.LastStep = se
	}
	return status, nil
}

func projectLegacySummary(authority harnesssessionwire.ReadAuthority, meta sessionstore.SessionMeta) (optionalSessionSummary, error) {
	if err := validateCompatibilityRecord(meta, uuid.UUID{}); err != nil {
		return optionalSessionSummary{}, err
	}
	absence := projectionAbsence(meta, true)
	if absence != coreProjectionPresent {
		return optionalSessionSummary{absence: absence}, nil
	}
	record := catalogRecord(meta)
	value, err := harnesssessionwire.ProjectSessionSummary(harnesssessionwire.ReadScope{
		TenantID: authority.TenantID, SessionID: record.SessionID,
		AgentID: authority.AgentID, Residency: coresessionwire.SessionResidencyCold,
	}, record)
	if err != nil {
		return optionalSessionSummary{}, err
	}
	return optionalSessionSummary{value: value}, nil
}

func projectLegacyStatus(scope harnesssessionwire.ReadScope, meta sessionstore.SessionMeta) (optionalSessionStatus, error) {
	expected, err := uuid.Parse(string(scope.SessionID))
	if err != nil {
		return optionalSessionStatus{}, &compatibilityProjectionError{field: "session_id"}
	}
	if err := validateCompatibilityRecord(meta, expected); err != nil {
		return optionalSessionStatus{}, err
	}
	absence := projectionAbsence(meta, false)
	if absence != coreProjectionPresent {
		return optionalSessionStatus{absence: absence}, nil
	}
	value, err := harnesssessionwire.ProjectSessionStatus(scope, catalogRecord(meta))
	if err != nil {
		return optionalSessionStatus{}, err
	}
	return optionalSessionStatus{value: value}, nil
}

func projectionAbsence(meta sessionstore.SessionMeta, summary bool) coreProjectionAbsence {
	var absence coreProjectionAbsence
	if meta.State == "" {
		absence |= coreProjectionMissingState
	}
	if summary && meta.CreatedAt.IsZero() && meta.LastActiveAt.IsZero() {
		absence |= coreProjectionMissingActivity
	}
	return absence
}

// validateCompatibilityRecord validates everything that must remain fail-closed
// before the narrowly documented historical omissions may bypass Core projection.
// expected is zero only for list rows, whose KV key is not exposed by Catalog.
func validateCompatibilityRecord(meta sessionstore.SessionMeta, expected uuid.UUID) error {
	if meta.SessionID.IsZero() || (!expected.IsZero() && meta.SessionID != expected) {
		return &compatibilityProjectionError{field: "session_id"}
	}
	switch meta.State {
	case "",
		sessionstore.StateRunning,
		sessionstore.StateWaitingOnGate,
		sessionstore.StateIdle,
		sessionstore.StateFailed,
		sessionstore.StateInterrupted,
		sessionstore.StateStopped:
		return nil
	default:
		return &compatibilityProjectionError{field: "state"}
	}
}

func catalogRecord(meta sessionstore.SessionMeta) harnesssessionwire.CatalogRecord {
	record := harnesssessionwire.CatalogRecord{
		SessionID: coresessionwire.SessionID(meta.SessionID.String()),
		State:     harnesssessionwire.CatalogState(meta.State), Title: meta.Title,
		CreatedAt: meta.CreatedAt, LastActiveAt: meta.LastActiveAt, LastJournalSeq: meta.LastJournalSeq,
	}
	if !meta.WaitingGateID.IsZero() {
		record.WaitingGateID = coresessionwire.GateID(meta.WaitingGateID.String())
	}
	return record
}

func (r *Reader) readScope(id uuid.UUID) harnesssessionwire.ReadScope {
	return harnesssessionwire.ReadScope{
		TenantID: r.authority.TenantID, SessionID: coresessionwire.SessionID(id.String()),
		AgentID: r.authority.AgentID, Residency: r.residency,
	}
}

// reconstruct rebuilds a serve.StatusEvent from a catalog eventSummary's durable wire
// bytes and journal sequence, decoding the event via event.UnmarshalEvent (the same
// authority that produced the summary's MarshalEvent bytes), so the round-trip is
// lossless. Empty bytes yield a nil StatusEvent (no summary recorded yet).
func reconstruct(seq uint64, raw json.RawMessage) (*serve.StatusEvent, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	ev, err := event.UnmarshalEvent(raw)
	if err != nil {
		return nil, err
	}
	if ev.Visibility() != event.Public {
		return nil, &PrivateEventError{Visibility: ev.Visibility()}
	}
	return &serve.StatusEvent{JournalSeq: seq, Event: ev}, nil
}

// ReadJournal reads a page of a session's Enduring events from the store's event
// replayer, positioned at page.From (inclusive; 0 = beginning). It collects up to
// page.Limit events, then reports Done when the replayer drained BEFORE the limit was
// hit and NextJournalSeq (the last delivered sequence + 1) as the resume cursor when
// more may remain. GatePrepared never appears — the replayer filters it. A replayer
// open or read failure is wrapped in a serve.StoreReadError (500).
func (r *Reader) ReadJournal(ctx context.Context, id uuid.UUID, page serve.JournalPage) (serve.EventJournalPage, error) {
	scope := r.readScope(id)
	if err := harnesssessionwire.ValidateReadScope(scope); err != nil {
		return serve.EventJournalPage{}, serve.StoreReadError{Op: "project", Cause: err}
	}
	replayer, err := r.store.OpenEventReplayer(id, sessionstore.ReplayRequest{FromSeq: page.From})
	if err != nil {
		return serve.EventJournalPage{}, serve.StoreReadError{Op: "open_replayer", Cause: err}
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{})
	if err != nil {
		return serve.EventJournalPage{}, serve.StoreReadError{Op: "open_cursor", Cause: err}
	}
	defer func() { _ = cursor.Close() }()

	events := make([]serve.StatusEvent, 0)
	var lastSeq uint64
	exhausted := false
	for len(events) < page.Limit {
		ev, seq, nerr := cursor.Next(ctx)
		if errors.Is(nerr, io.EOF) {
			exhausted = true
			break
		}
		if nerr != nil {
			return serve.EventJournalPage{}, serve.StoreReadError{Op: "replay", Cause: nerr}
		}
		if !ev.Visibility().Valid() {
			return serve.EventJournalPage{}, serve.StoreReadError{Op: "replay", Cause: &event.InvalidEventError{Event: "Event", Field: event.FieldVisibility, Rule: event.RuleInvalid}}
		}
		if ev.Visibility() != event.Public {
			continue
		}
		if _, perr := harnesssessionwire.ProjectJournalPage(scope, []harnesssessionwire.JournalRecord{{JournalSeq: seq, Event: ev}}, seq, seq, "", ""); perr != nil {
			return serve.EventJournalPage{}, serve.StoreReadError{Op: "project", Cause: perr}
		}
		events = append(events, serve.StatusEvent{JournalSeq: seq, Event: ev})
		lastSeq = seq
	}

	out := serve.EventJournalPage{Events: events, Done: exhausted}
	if !exhausted {
		if len(events) > 0 {
			out.NextJournalSeq = lastSeq + 1
		} else {
			// Degenerate window (Limit <= 0): nothing read, more may remain — resume
			// exactly where the caller asked (From) rather than fabricating a sequence.
			out.NextJournalSeq = page.From
		}
	}
	return out, nil
}
