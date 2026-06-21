package journal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCatalogRepairNoReplayer proves RepairCatalog fails with a typed error rather than
// silently no-op'ing when the Catalog was built without a replayer.
func TestCatalogRepairNoReplayer(t *testing.T) {
	t.Parallel()
	c := &Catalog{kv: newFakeKV(), bucket: catalogBucket, now: time.Now, log: nopCatalogLogger{}}
	_, err := c.RepairCatalog(context.Background(), fixedUUID(0x21))
	if err == nil {
		t.Fatal("RepairCatalog with no replayer = nil error, want typed error")
	}
	var re *CatalogReadError
	if !errors.As(err, &re) {
		t.Fatalf("error %v is not *CatalogReadError", err)
	}
	if !errors.Is(err, errNoReplayer) {
		t.Errorf("error does not unwrap to errNoReplayer: %v", err)
	}
}

// TestCatalogListSessionsEmptyBucket proves ListSessions returns an empty slice (not an
// error) for a bucket with no keys — the no-sessions case the picker shows as empty.
func TestCatalogListSessionsEmptyBucket(t *testing.T) {
	t.Parallel()
	c := &Catalog{kv: newFakeKV(), bucket: catalogBucket, now: time.Now, log: nopCatalogLogger{}}
	metas, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions on empty bucket = %v, want nil", err)
	}
	if len(metas) != 0 {
		t.Errorf("ListSessions on empty bucket = %d entries, want 0", len(metas))
	}
}
