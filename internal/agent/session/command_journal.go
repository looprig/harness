package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/session/journal"
	"github.com/inventivepotter/urvi/internal/uuid"
)

// commandAppender is the session's narrow durable-write seam for the INTENT LOG:
// append one command (the session dispatched to a loop) to the session's durable
// journal. The session depends only on this one method (Interface Segregation) —
// never on the full SessionJournal, its stream management, or the record codec. The
// composition root (Phase 10) wires a real adapter over SessionJournal
// (journal.JournalCommandAppender); the default is the nop appender so existing tests
// and headless/no-persistence mode are unchanged.
//
// Unlike the hub's eventAppender (a REQUIRED durable tap that faults the session on
// failure), this seam is AUDIT-ONLY: the session calls AppendCommand BEFORE dispatch
// and, on a non-nil error, LOGS LOUDLY and PROCEEDS with the dispatch. Losing a command
// record must never block the user's action or corrupt restore — it is the ONE
// deliberate non-fatal persistence path in the session. AppendCommand therefore returns
// its error for the caller to log; the session never propagates it.
type commandAppender interface {
	AppendCommand(ctx context.Context, rec journal.CommandRecord) error
}

// nopCommandAppender is the default appender wired into a session built without an
// injected one. It persists nothing and never fails, so the audit-only append path is
// a pure no-op in no-persistence mode — every command is dispatched exactly as before
// the intent log landed. Headless runs and existing tests use this.
type nopCommandAppender struct{}

func (nopCommandAppender) AppendCommand(context.Context, journal.CommandRecord) error { return nil }

// Option configures an optional session dependency at construction. The bare
// New(ctx, cfg) installs the nop command appender; an Option overrides it. This mirrors
// the hub's Option pattern so the composition root injects the durable intent-log
// appender (Phase 10) without New growing a positional parameter.
type Option func(*Session)

// WithCommandAppender injects the audit-only intent-log appender (the composition
// root's adapter over SessionJournal). A nil appender is ignored (the nop default stays
// installed) so a caller can never accidentally null out the field and nil-deref the
// dispatch path.
func WithCommandAppender(a commandAppender) Option {
	return func(s *Session) {
		if a != nil {
			s.cmdAppender = a
		}
	}
}

// stampNow returns the session clock's current time, defaulting to the wall clock if
// the clock seam is unset (a struct-literal test session). The session stamps this onto
// every dispatched command's Header.CreatedAt at the dispatch boundary, so a journaled
// intent-log record carries its creation time minted from the SAME seam as the
// session's events.
func (s *Session) stampNow() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// appendCommand is the session's DRY, AUDIT-ONLY intent-log write, called at every
// command-dispatch site BEFORE the command is sent to the loop. It wraps cmd in a
// journal.CommandRecord targeting (sessionID, loopID) — the dispatch target the command
// itself may not carry (Interrupt/Shutdown route per-loop) — and appends it.
//
// On a non-nil append error it LOGS LOUDLY and RETURNS (the caller proceeds with the
// dispatch): losing a command record must never block the user's action or fault the
// session. This is the single deliberate proceed-on-failure persistence path. The
// appender is nil-guarded (a struct-literal test session leaves it unset) so the path
// is a safe no-op in no-persistence mode.
func (s *Session) appendCommand(ctx context.Context, loopID uuid.UUID, cmd command.Command) {
	app := s.cmdAppender
	if app == nil {
		return
	}
	rec := journal.NewCommandRecord(s.SessionID, loopID, cmd)
	if err := app.AppendCommand(ctx, rec); err != nil {
		// Audit-only: log loudly and proceed. Never block the dispatch, never fault the
		// session — a lost intent-log record is recoverable; a blocked user action is not.
		slog.ErrorContext(ctx, "session: intent-log command append failed (audit-only, proceeding)",
			"session", s.SessionID,
			"loop", loopID,
			"command_id", cmd.CommandHeader().CommandID,
			"err", err,
		)
	}
}
