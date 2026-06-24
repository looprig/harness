package persistence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ciram-co/looprig/pkg/uuid"
)

const (
	// sessionMetaFileName is the per-session manifest. It holds only non-secret,
	// listable fields and is rewritten atomically.
	sessionMetaFileName = "meta.json"
	// sessionMetaFilePerm keeps the manifest owner-only; though non-secret, it stays
	// consistent with the owner-only session tree.
	sessionMetaFilePerm = 0o600
	// maxSessionTitleLen bounds a stored title in runes; longer titles are truncated.
	maxSessionTitleLen = 120
)

// TitleSource records how a session's title was derived.
type TitleSource string

const (
	// TitleSourceNone is the initial state: no title chosen yet.
	TitleSourceNone TitleSource = "none"
	// TitleSourceGenerated marks a model-generated title.
	TitleSourceGenerated TitleSource = "generated"
	// TitleSourceFirstUserMessage marks a title taken from the first user message.
	TitleSourceFirstUserMessage TitleSource = "first_user_message"
)

func (s TitleSource) valid() bool {
	switch s {
	case TitleSourceNone, TitleSourceGenerated, TitleSourceFirstUserMessage:
		return true
	default:
		return false
	}
}

// SessionStatus records a session's lifecycle state for listing.
type SessionStatus string

const (
	// SessionStatusActive is a session that has not been cleanly closed.
	SessionStatusActive SessionStatus = "active"
	// SessionStatusClosed is a session whose agent has torn down cleanly.
	SessionStatusClosed SessionStatus = "closed"
)

func (s SessionStatus) valid() bool {
	switch s {
	case SessionStatusActive, SessionStatusClosed:
		return true
	default:
		return false
	}
}

// SessionMeta is the non-secret, listable manifest for one session. It deliberately holds
// no model spec, API key, request, response, or transcript — only identity, a display
// title, status, and timestamps.
type SessionMeta struct {
	ID          uuid.UUID     `json:"id"`
	Title       string        `json:"title"`
	TitleSource TitleSource   `json:"title_source"`
	Status      SessionStatus `json:"status"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

var (
	errInvalidSessionTitle  = errors.New("invalid session title")
	errInvalidTitleSource   = errors.New("invalid title source")
	errInvalidSessionStatus = errors.New("invalid session status")
	errMissingSessionMeta   = errors.New("session metadata not found")
	errCorruptSessionMeta   = errors.New("session metadata is corrupt")
)

// SessionMetaStore is a serialized, atomic manifest writer for one session directory. A
// private mutex orders read-modify-write updates so concurrent callers never clobber each
// other's fields, and every write lands via a temp file + rename so a reader never sees a
// partial manifest.
type SessionMetaStore struct {
	mu  sync.Mutex
	id  uuid.UUID
	dir string
}

// OpenSessionMeta returns a manifest writer for id's directory, creating the (confined,
// owner-only) directory if it does not yet exist.
func (r *SessionStoreRoot) OpenSessionMeta(id uuid.UUID) (*SessionMetaStore, error) {
	dir, err := r.CreateSessionDir(id)
	if err != nil {
		return nil, err
	}
	return &SessionMetaStore{id: id, dir: dir}, nil
}

// Init writes the initial manifest (empty title, source none, active) when absent and
// returns it. On a resumed session with an existing manifest it returns that manifest
// unchanged, so resume never resets a title or status.
func (s *SessionMetaStore) Init(now time.Time) (SessionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := s.read()
	switch {
	case err == nil:
		return meta, nil
	case errors.Is(err, errMissingSessionMeta), errors.Is(err, errCorruptSessionMeta):
		// Missing or corrupt: (re)create a fresh manifest. The journal is authoritative for
		// the conversation, so repairing the non-load-bearing manifest is always safe.
	default:
		return SessionMeta{}, err
	}

	meta = SessionMeta{
		ID:          s.id,
		Title:       "",
		TitleSource: TitleSourceNone,
		Status:      SessionStatusActive,
		CreatedAt:   now.UTC(),
		UpdatedAt:   now.UTC(),
	}
	if err := s.write(meta); err != nil {
		return SessionMeta{}, err
	}
	return meta, nil
}

// Read returns the current manifest, or a typed errMissingSessionMeta/errCorruptSessionMeta
// the caller can downgrade to a warning.
func (s *SessionMetaStore) Read() (SessionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

// SetTitle validates and stores a one-line display title from source, preserving every
// other manifest field. Control characters make the title illegal; an over-long title is
// truncated to maxSessionTitleLen runes. The none source is not a settable title.
func (s *SessionMetaStore) SetTitle(title string, source TitleSource, now time.Time) (SessionMeta, error) {
	if !source.valid() || source == TitleSourceNone {
		return SessionMeta{}, errInvalidTitleSource
	}
	clean, err := sanitizeTitle(title)
	if err != nil {
		return SessionMeta{}, err
	}
	return s.update(now, func(m *SessionMeta) {
		m.Title = clean
		m.TitleSource = source
	})
}

// SetStatus stores a new lifecycle status, preserving all other fields.
func (s *SessionMetaStore) SetStatus(status SessionStatus, now time.Time) (SessionMeta, error) {
	if !status.valid() {
		return SessionMeta{}, errInvalidSessionStatus
	}
	return s.update(now, func(m *SessionMeta) {
		m.Status = status
	})
}

// update applies mutate to the current manifest under the writer lock and atomically
// rewrites it. Reading first means each writer preserves fields it does not touch.
func (s *SessionMetaStore) update(now time.Time, mutate func(*SessionMeta)) (SessionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := s.read()
	if err != nil {
		return SessionMeta{}, err
	}
	mutate(&meta)
	meta.UpdatedAt = now.UTC()
	if err := s.write(meta); err != nil {
		return SessionMeta{}, err
	}
	return meta, nil
}

func (s *SessionMetaStore) read() (SessionMeta, error) {
	return readSessionMeta(filepath.Join(s.dir, sessionMetaFileName))
}

func (s *SessionMetaStore) write(meta SessionMeta) error {
	return writeSessionMeta(s.dir, meta)
}

func readSessionMeta(path string) (SessionMeta, error) {
	// #nosec G304 -- path is the confined manifest under a validated session directory.
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionMeta{}, errMissingSessionMeta
		}
		return SessionMeta{}, err
	}

	var meta SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return SessionMeta{}, fmt.Errorf("%w: %v", errCorruptSessionMeta, err)
	}
	if meta.ID.IsZero() || !meta.TitleSource.valid() || !meta.Status.valid() {
		return SessionMeta{}, fmt.Errorf("%w: malformed fields", errCorruptSessionMeta)
	}
	return meta, nil
}

func writeSessionMeta(dir string, meta SessionMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, sessionMetaFileName+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(sessionMetaFilePerm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, filepath.Join(dir, sessionMetaFileName)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	committed = true
	return syncDir(dir)
}

func syncDir(dir string) error {
	// #nosec G304 -- dir is a validated, confined session directory.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func sanitizeTitle(title string) (string, error) {
	if !utf8.ValidString(title) {
		return "", errInvalidSessionTitle
	}
	for _, r := range title {
		if r == ' ' {
			continue
		}
		if unicode.IsControl(r) || r == '\uFEFF' {
			return "", errInvalidSessionTitle
		}
	}
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", errInvalidSessionTitle
	}
	return truncateRunes(trimmed, maxSessionTitleLen), nil
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max]))
}

// SessionListEntry is one row of the session list. A readable manifest sets Meta with a nil
// Err; a missing or corrupt manifest sets Err and the ID parsed from the directory name.
type SessionListEntry struct {
	Meta SessionMeta
	Err  error
}

// ListSessionMeta reads every session directory's manifest, returning one entry per
// session directory sorted most-recently-updated first. A corrupt or missing manifest
// yields an entry with a non-nil Err rather than failing the whole listing, so one bad
// session never hides the rest.
func (r *SessionStoreRoot) ListSessionMeta() ([]SessionListEntry, error) {
	if err := r.validate(); err != nil {
		return nil, &SessionStoreError{Operation: SessionStoreResolve, Cause: err}
	}

	dirents, err := os.ReadDir(r.sessionsDir)
	if err != nil {
		return nil, &SessionStoreError{Operation: SessionStoreResolve, Path: r.sessionsDir, Cause: err}
	}

	entries := make([]SessionListEntry, 0, len(dirents))
	for _, de := range dirents {
		if !de.IsDir() {
			continue
		}
		var id uuid.UUID
		if perr := id.UnmarshalText([]byte(de.Name())); perr != nil {
			continue // not a session directory
		}

		meta, rerr := readSessionMeta(filepath.Join(r.sessionsDir, de.Name(), sessionMetaFileName))
		if rerr != nil {
			entries = append(entries, SessionListEntry{Meta: SessionMeta{ID: id}, Err: rerr})
			continue
		}
		if meta.ID != id {
			entries = append(entries, SessionListEntry{
				Meta: SessionMeta{ID: id},
				Err:  fmt.Errorf("%w: id mismatch", errCorruptSessionMeta),
			})
			continue
		}
		entries = append(entries, SessionListEntry{Meta: meta})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return sessionListLess(entries[i], entries[j])
	})
	return entries, nil
}

// sessionListLess orders valid entries first (most-recently-updated first) and sinks
// invalid entries to the end, ordered by ID for a deterministic listing.
func sessionListLess(a, b SessionListEntry) bool {
	aValid := a.Err == nil
	bValid := b.Err == nil
	if aValid != bValid {
		return aValid
	}
	if !aValid {
		return a.Meta.ID.String() < b.Meta.ID.String()
	}
	if !a.Meta.UpdatedAt.Equal(b.Meta.UpdatedAt) {
		return a.Meta.UpdatedAt.After(b.Meta.UpdatedAt)
	}
	return a.Meta.ID.String() < b.Meta.ID.String()
}
