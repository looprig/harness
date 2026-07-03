package persistence

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/uuid"
)

func testClock() time.Time {
	return time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
}

func openTestMeta(t *testing.T) (*SessionStoreRoot, *SessionMetaStore, uuid.UUID) {
	t.Helper()
	root := newTestSessionStoreRoot(t)
	id := mustUUID(t)
	store, err := root.OpenSessionMeta(id)
	if err != nil {
		t.Fatalf("OpenSessionMeta: %v", err)
	}
	return root, store, id
}

// TestSessionMetaInit proves a new session starts with an empty, source-none title in the
// active state, and that re-initialising a resumed session preserves the existing manifest.
func TestSessionMetaInit(t *testing.T) {
	_, store, id := openTestMeta(t)

	meta, err := store.Init(testClock())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if meta.ID != id {
		t.Errorf("ID = %v, want %v", meta.ID, id)
	}
	if meta.Title != "" {
		t.Errorf("Title = %q, want empty", meta.Title)
	}
	if meta.TitleSource != TitleSourceNone {
		t.Errorf("TitleSource = %q, want %q", meta.TitleSource, TitleSourceNone)
	}
	if meta.Status != SessionStatusActive {
		t.Errorf("Status = %q, want %q", meta.Status, SessionStatusActive)
	}
	if meta.CreatedAt.IsZero() || meta.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: %+v", meta)
	}

	// A second Init (resume) keeps the original manifest rather than resetting it.
	if _, err := store.SetTitle("kept", TitleSourceGenerated, testClock().Add(time.Minute)); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	again, err := store.Init(testClock().Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("Init (resume): %v", err)
	}
	if again.Title != "kept" || again.TitleSource != TitleSourceGenerated {
		t.Errorf("resume Init reset manifest: %+v", again)
	}
}

// TestSessionMetaSetTitle covers accepted sources, control-character rejection, the none
// source being illegal, empty rejection, and bounded truncation.
func TestSessionMetaSetTitle(t *testing.T) {
	longTitle := strings.Repeat("a", maxSessionTitleLen+80)

	tests := []struct {
		name      string
		title     string
		source    TitleSource
		wantTitle string
		wantErr   bool
	}{
		{name: "generated title accepted", title: "Refactor auth", source: TitleSourceGenerated, wantTitle: "Refactor auth"},
		{name: "first user message accepted", title: "fix the parser", source: TitleSourceFirstUserMessage, wantTitle: "fix the parser"},
		{name: "surrounding whitespace trimmed", title: "  spaced out  ", source: TitleSourceGenerated, wantTitle: "spaced out"},
		{name: "newline rejected", title: "two\nlines", source: TitleSourceGenerated, wantErr: true},
		{name: "tab rejected", title: "tab\tted", source: TitleSourceGenerated, wantErr: true},
		{name: "null byte rejected", title: "nul\x00", source: TitleSourceGenerated, wantErr: true},
		{name: "none source rejected", title: "whatever", source: TitleSourceNone, wantErr: true},
		{name: "unknown source rejected", title: "whatever", source: TitleSource("bogus"), wantErr: true},
		{name: "empty rejected", title: "   ", source: TitleSourceGenerated, wantErr: true},
		{name: "over-long truncated", title: longTitle, source: TitleSourceGenerated, wantTitle: strings.Repeat("a", maxSessionTitleLen)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, store, _ := openTestMeta(t)
			if _, err := store.Init(testClock()); err != nil {
				t.Fatalf("Init: %v", err)
			}

			meta, err := store.SetTitle(tt.title, tt.source, testClock().Add(time.Minute))
			if (err != nil) != tt.wantErr {
				t.Fatalf("SetTitle() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if meta.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", meta.Title, tt.wantTitle)
			}
			if meta.TitleSource != tt.source {
				t.Errorf("TitleSource = %q, want %q", meta.TitleSource, tt.source)
			}
			if !meta.UpdatedAt.After(meta.CreatedAt) {
				t.Errorf("UpdatedAt %v not after CreatedAt %v", meta.UpdatedAt, meta.CreatedAt)
			}
		})
	}
}

// TestSessionMetaConcurrentUpdate proves concurrent title and status writers neither corrupt
// the manifest nor clobber each other's fields: the status set by one writer survives the
// title writes of others.
func TestSessionMetaConcurrentUpdate(t *testing.T) {
	_, store, _ := openTestMeta(t)
	if _, err := store.Init(testClock()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	const writers = 24
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			now := testClock().Add(time.Duration(i+1) * time.Second)
			if i%2 == 0 {
				if _, err := store.SetTitle("t-"+strings.Repeat("x", i+1), TitleSourceGenerated, now); err != nil {
					t.Errorf("SetTitle: %v", err)
				}
				return
			}
			if _, err := store.SetStatus(SessionStatusClosed, now); err != nil {
				t.Errorf("SetStatus: %v", err)
			}
		}(i)
	}
	wg.Wait()

	final, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if final.Status != SessionStatusClosed {
		t.Errorf("Status = %q, want closed (title writes clobbered status)", final.Status)
	}
	if final.Title == "" || !strings.HasPrefix(final.Title, "t-") {
		t.Errorf("Title = %q, want a t- prefixed value (status writes clobbered title)", final.Title)
	}
}

// TestSessionMetaNoSecretsInJSON proves the serialized manifest exposes only the
// non-secret identity/title/status/timestamp fields — never a model spec, key, or
// transcript.
func TestSessionMetaNoSecretsInJSON(t *testing.T) {
	meta := SessionMeta{
		ID:          mustUUID(t),
		Title:       "a title",
		TitleSource: TitleSourceGenerated,
		Status:      SessionStatusActive,
		CreatedAt:   testClock(),
		UpdatedAt:   testClock(),
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	allowed := map[string]bool{
		"id": true, "title": true, "title_source": true,
		"status": true, "created_at": true, "updated_at": true,
	}
	for key := range fields {
		if !allowed[key] {
			t.Errorf("unexpected manifest field %q (possible secret leak)", key)
		}
	}
	for _, banned := range []string{"key", "api", "token", "secret", "model", "transcript", "request", "response"} {
		if strings.Contains(strings.ToLower(string(data)), banned) {
			t.Errorf("serialized manifest contains banned substring %q: %s", banned, data)
		}
	}
}

// TestListSessionMeta proves listing returns valid manifests most-recently-updated first
// and flags missing or corrupt manifests as invalid entries rather than failing.
func TestListSessionMeta(t *testing.T) {
	root := newTestSessionStoreRoot(t)

	// Two valid sessions at distinct update times.
	older := writeListedSession(t, root, "older session", testClock())
	newer := writeListedSession(t, root, "newer session", testClock().Add(time.Hour))

	// A session directory with no manifest (missing).
	missingID := mustUUID(t)
	if _, err := root.CreateSessionDir(missingID); err != nil {
		t.Fatalf("CreateSessionDir(missing): %v", err)
	}

	// A session directory with corrupt manifest bytes.
	corruptID := mustUUID(t)
	corruptDir, err := root.CreateSessionDir(corruptID)
	if err != nil {
		t.Fatalf("CreateSessionDir(corrupt): %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, sessionMetaFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	entries, err := root.ListSessionMeta()
	if err != nil {
		t.Fatalf("ListSessionMeta: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(entries), entries)
	}

	// Valid entries come first, most-recently-updated first.
	if entries[0].Err != nil || entries[0].Meta.ID != newer {
		t.Errorf("entry[0] = %+v, want valid newer %v", entries[0], newer)
	}
	if entries[1].Err != nil || entries[1].Meta.ID != older {
		t.Errorf("entry[1] = %+v, want valid older %v", entries[1], older)
	}

	invalid := map[uuid.UUID]bool{}
	for _, e := range entries[2:] {
		if e.Err == nil {
			t.Errorf("entry %v expected invalid, got valid", e.Meta.ID)
		}
		invalid[e.Meta.ID] = true
	}
	if !invalid[missingID] || !invalid[corruptID] {
		t.Errorf("invalid set %v missing %v or %v", invalid, missingID, corruptID)
	}

	// The slice must already be sorted (stable) as returned.
	if !sort.SliceIsSorted(entries, func(i, j int) bool { return sessionListLess(entries[i], entries[j]) }) {
		t.Error("ListSessionMeta result is not sorted")
	}
}

func writeListedSession(t *testing.T, root *SessionStoreRoot, title string, now time.Time) uuid.UUID {
	t.Helper()
	id := mustUUID(t)
	store, err := root.OpenSessionMeta(id)
	if err != nil {
		t.Fatalf("OpenSessionMeta: %v", err)
	}
	if _, err := store.Init(now); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := store.SetTitle(title, TitleSourceGenerated, now); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	return id
}

func TestSessionMetaReadMissingIsTyped(t *testing.T) {
	_, store, _ := openTestMeta(t)
	if _, err := store.Read(); !errors.Is(err, errMissingSessionMeta) {
		t.Fatalf("Read before Init error = %v, want errMissingSessionMeta", err)
	}
}

// TestSessionMetaInitRepairsCorruptManifest proves Init repairs (overwrites) a corrupt
// manifest rather than failing, so a resumed session whose manifest was truncated/garbled
// becomes listable again — JetStream remains authoritative for the conversation.
func TestSessionMetaInitRepairsCorruptManifest(t *testing.T) {
	root, store, id := openTestMeta(t)

	dir, err := root.CreateSessionDir(id)
	if err != nil {
		t.Fatalf("CreateSessionDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionMetaFileName), []byte("{corrupt-not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt manifest: %v", err)
	}

	meta, err := store.Init(testClock())
	if err != nil {
		t.Fatalf("Init did not repair a corrupt manifest: %v", err)
	}
	if meta.ID != id || meta.Status != SessionStatusActive {
		t.Errorf("repaired manifest = %+v, want id %v / active", meta, id)
	}
	if _, err := store.Read(); err != nil {
		t.Fatalf("Read after repair failed: %v", err)
	}
}
