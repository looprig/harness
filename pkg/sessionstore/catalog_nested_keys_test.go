package sessionstore

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/fsstore"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// TestCatalogListSessionsIgnoresNestedObjectMetadata is the v0.40.1 regression for
// F1 (FABLE_RULING_KV_PREFIX_COLLISION.md): SessionStore's legacy layout files a
// published object's metadata index UNDER the catalog key —
// "sessions/<uuid>/object-metadata/v1/…" — and KV.Keys(prefix) is a substring
// filter that returns those descendants. ListSessions used to decode every key
// under "sessions/" as SessionMeta, so one tool-result capture (v0.40.0) or one
// journal offload (v0.34.0) failed the whole listing on every backend. It must list
// exactly the sessions, and only the sessions.
func TestCatalogListSessionsIgnoresNestedObjectMetadata(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)
	backend := memstore.New()
	store, err := Open(backend, WithOffloadThreshold(64))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	catalog := store.OpenCatalog(WithCatalogClock(fixedClock(now)))

	sessions := []uuid.UUID{fixedUUID(0x11), fixedUUID(0x22), fixedUUID(0x33)}
	for _, sid := range sessions {
		started := event.SessionStarted{
			Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now},
			Config: event.ConfigFingerprint{ModelID: "m"},
		}
		if err := catalog.UpdateOnEvent(ctx, started, 0); err != nil {
			t.Fatalf("UpdateOnEvent(%v): %v", sid, err)
		}
	}

	// A retained tool result for the first session (harness v0.40.0 capture path).
	publishToolResult(t, store.ToolResultObjects(), sessions[0], bytes.Repeat([]byte("captured output\n"), 256))

	// A journal record over the offload threshold for the second session (the
	// v0.34.0 journal-offload path): it publishes a journal-runtime object.
	lease, err := store.AcquireLease(ctx, sessions[1])
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	j, err := store.OpenJournal(ctx, sessions[1], lease)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	rec, _ := largeCommand(sessions[1], newTestUUID(t))
	seq, err := j.Append(ctx, rec)
	if err != nil {
		t.Fatalf("Append large command: %v", err)
	}
	if env := readEnvelope(t, store, sessions[1], seq); env.Kind != string(kindBlobPtr) {
		t.Fatalf("command record kind = %q, want %q (must offload)", env.Kind, kindBlobPtr)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Pin the precondition: the backend really holds keys nested under a catalog
	// key for BOTH sessions, so this test exercises the collision rather than
	// passing vacuously if SessionStore ever relocates its index.
	keys, err := backend.Keys(ctx, sessionsPrefix)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	for _, sid := range sessions[:2] {
		nested := sessionsPrefix + sid.String() + "/"
		found := false
		for _, key := range keys {
			if strings.HasPrefix(key, nested) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no KV key nested under %q in %q; the regression precondition is gone", nested, keys)
		}
	}

	metas, err := catalog.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions = %v, want nil", err)
	}
	if len(metas) != len(sessions) {
		t.Fatalf("ListSessions = %d entries (%+v), want exactly %d", len(metas), metas, len(sessions))
	}
	for i, sid := range sessions {
		if metas[i].SessionID != sid {
			t.Errorf("metas[%d].SessionID = %v, want %v", i, metas[i].SessionID, sid)
		}
	}
}

// TestCatalogListSessionsIgnoresNestedObjectMetadataOverFSStoreKV is the fsstore
// composition the ruling (§2.2 item 3, §2.4) asks for: harness store over fsstore
// KV/Ledger/Leaser/OrderedIndex with a conforming Blobs, an object published AFTER
// the catalog entry exists, then ListSessions. Raw fsstore Blobs deliberately lacks
// storage.BlobReaderLifecycle (Open refuses it), so Blobs come from memstore; the
// collision under test is a KV one. fsstore ≤ v0.5.1 mapped "sessions/<uuid>" and
// "sessions/<uuid>/object-metadata/…" onto one directory entry, so the publish
// failed (object backend (metadata_commit)); v0.6.0's suffixed layout holds both.
func TestCatalogListSessionsIgnoresNestedObjectMetadataOverFSStoreKV(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	disk, err := fsstore.Open(fsstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("fsstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = disk.Close() })
	diskBackend := disk.Backend()
	backend, err := storage.NewCompositeWithOrderedIndex(
		diskBackend.Ledger,
		diskBackend.Leaser,
		diskBackend.KV,
		memstore.New().Blobs,
		diskBackend.OrderedIndex,
	)
	if err != nil {
		t.Fatalf("compose fsstore KV + memstore Blobs: %v", err)
	}
	store, err := Open(backend)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	catalog := store.OpenCatalog(WithCatalogClock(fixedClock(now)))

	sessions := []uuid.UUID{fixedUUID(0x11), fixedUUID(0x22)}
	for _, sid := range sessions {
		started := event.SessionStarted{
			Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now},
			Config: event.ConfigFingerprint{ModelID: "m"},
		}
		if err := catalog.UpdateOnEvent(ctx, started, 0); err != nil {
			t.Fatalf("UpdateOnEvent(%v): %v", sid, err)
		}
	}

	// The catalog entry exists; now publish an object whose metadata index nests
	// beneath it on the same fsstore KV.
	body := bytes.Repeat([]byte("captured output\n"), 256)
	metadata := publishToolResult(t, store.ToolResultObjects(), sessions[0], body)

	// Precondition: fsstore KV really holds the catalog key AND a key nested
	// beneath it, so the test exercises the collision rather than passing
	// vacuously if SessionStore ever relocates its index.
	keys, err := backend.Keys(ctx, sessionsPrefix)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	entry := sessionsPrefix + sessions[0].String()
	var haveEntry, haveNested bool
	for _, key := range keys {
		haveEntry = haveEntry || key == entry
		haveNested = haveNested || strings.HasPrefix(key, entry+"/")
	}
	if !haveEntry || !haveNested {
		t.Fatalf("fsstore KV keys %q: entry=%v nested=%v, want both; the regression precondition is gone", keys, haveEntry, haveNested)
	}

	metas, err := catalog.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions = %v, want nil", err)
	}
	if len(metas) != len(sessions) {
		t.Fatalf("ListSessions = %d entries (%+v), want exactly %d", len(metas), metas, len(sessions))
	}
	for i, sid := range sessions {
		if metas[i].SessionID != sid {
			t.Errorf("metas[%d].SessionID = %v, want %v", i, metas[i].SessionID, sid)
		}
	}

	// The catalog entry survives a fold after the publish (the key is still a
	// writable leaf, not a directory), and the object still reads back.
	turn := event.TurnStarted{Header: hdr(sessions[0]), Message: userMsg("after publish")}
	if err := catalog.UpdateOnEvent(ctx, turn, 1); err != nil {
		t.Fatalf("UpdateOnEvent after publish: %v", err)
	}
	got, err := readToolResult(store.ToolResultObjects(), sessions[0], metadata.Reference)
	if err != nil {
		t.Fatalf("read tool result back: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("tool result read back %d bytes, want %d identical bytes", len(got), len(body))
	}
}

// TestIsCatalogEntryKey pins the exact-depth filter clause by clause.
func TestIsCatalogEntryKey(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11).String()
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{name: "catalog entry", key: sessionsPrefix + sid, want: true},
		{name: "object metadata beneath entry", key: sessionsPrefix + sid + "/object-metadata/v1/tool-result/ab/1", want: false},
		{name: "one nested segment", key: sessionsPrefix + sid + "/protocol", want: false},
		{name: "bare prefix", key: sessionsPrefix, want: false},
		{name: "other namespace", key: "sessionstore/layout", want: false},
		{name: "prefix without separator", key: "sessions" + sid, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isCatalogEntryKey(tt.key); got != tt.want {
				t.Fatalf("isCatalogEntryKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}
