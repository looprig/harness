package sessionstore

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
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
// composition the ruling (§2.2 item 3) asks for: harness store over fsstore
// KV/Ledger/Leaser with a conforming Blobs, publish an object AFTER the catalog
// entry exists, then ListSessions. It cannot pass on fsstore v0.5.1, whose KV maps
// "sessions/<uuid>" and "sessions/<uuid>/object-metadata/…" onto one directory
// entry (the separate fsstore defect, fixed by fsstore v0.6.0's suffixed layout).
func TestCatalogListSessionsIgnoresNestedObjectMetadataOverFSStoreKV(t *testing.T) {
	t.Skip("TODO(fsstore v0.6.0): fsstore v0.5.1 KV cannot hold a key and its '/' descendant; " +
		"compose fsstore KV/Ledger/Leaser + memstore Blobs here once fsstore v0.6.0 is released and pinned")
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
