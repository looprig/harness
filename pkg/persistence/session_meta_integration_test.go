//go:build integration

package persistence

import (
	"testing"
	"time"
)

// TestSessionMetaPersistsAcrossStoreRoots proves a manifest written through one store root
// is durable: a freshly opened store root over the same data home reads it back and lists
// it. This is the property the CLI --list and resume paths depend on.
func TestSessionMetaPersistsAcrossStoreRoots(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	writeRoot, err := OpenSessionStoreRoot()
	if err != nil {
		t.Fatalf("OpenSessionStoreRoot (write): %v", err)
	}
	id := mustUUID(t)
	store, err := writeRoot.OpenSessionMeta(id)
	if err != nil {
		t.Fatalf("OpenSessionMeta: %v", err)
	}
	now := time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC)
	if _, err := store.Init(now); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := store.SetTitle("durable title", TitleSourceGenerated, now.Add(time.Minute)); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}

	readRoot, err := OpenSessionStoreRoot()
	if err != nil {
		t.Fatalf("OpenSessionStoreRoot (read): %v", err)
	}
	reread, err := readRoot.OpenSessionMeta(id)
	if err != nil {
		t.Fatalf("OpenSessionMeta (read): %v", err)
	}
	got, err := reread.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Title != "durable title" || got.TitleSource != TitleSourceGenerated {
		t.Errorf("reread manifest = %+v, want durable title/generated", got)
	}

	entries, err := readRoot.ListSessionMeta()
	if err != nil {
		t.Fatalf("ListSessionMeta: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Err == nil && e.Meta.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("listed sessions %+v do not include %v", entries, id)
	}
}
