package journal

import (
	"errors"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/uuid"
	"github.com/nats-io/nats.go"
)

// gcStubLease is a white-box ownershipToken double for GC unit tests. valid drives the
// lease guard; lost (when non-nil and closed) drives the loss-channel arm.
type gcStubLease struct {
	epoch     uint64
	sessionID uuid.UUID
	valid     bool
	lost      chan struct{}
}

func (l *gcStubLease) Epoch() uint64 { return l.epoch }
func (l *gcStubLease) Valid() bool   { return l.valid }
func (l *gcStubLease) Lost() <-chan struct{} {
	if l.lost == nil {
		return nil
	}
	return l.lost
}

// gcFakeLister is a test double for gcLister. It serves a fixed object inventory and
// records the names passed to Delete (so the reap decision can be asserted without a
// live store). delErr, when set, fails the next Delete to drive the fail-closed arm.
type gcFakeLister struct {
	objs    []*nats.ObjectInfo
	deleted []string
	delErr  error
}

func (f *gcFakeLister) List(_ ...nats.ListObjectsOpt) ([]*nats.ObjectInfo, error) {
	return f.objs, nil
}

func (f *gcFakeLister) GetInfo(name string, _ ...nats.GetObjectInfoOpt) (*nats.ObjectInfo, error) {
	for _, o := range f.objs {
		if o.Name == name {
			return o, nil
		}
	}
	return nil, nats.ErrObjectNotFound
}

func (f *gcFakeLister) Delete(name string) error {
	if f.delErr != nil {
		return f.delErr
	}
	f.deleted = append(f.deleted, name)
	return nil
}

// objInfo builds an *nats.ObjectInfo with the given content-addressed name and ModTime.
func objInfo(name string, modTime time.Time) *nats.ObjectInfo {
	return &nats.ObjectInfo{
		ObjectMeta: nats.ObjectMeta{Name: name},
		ModTime:    modTime,
	}
}

// TestGCGraceWindowExceedsMaxRetry pins the load-bearing invariant: the grace window
// must exceed the longest possible upload->pointer-append gap, so a just-uploaded object
// whose append is still in flight is never reaped. That gap is bounded by the dedup
// window plus the publish budget (the first append deadline plus the ambiguous-ack retry
// deadline).
func TestGCGraceWindowExceedsMaxRetry(t *testing.T) {
	t.Parallel()
	// The worst-case window an in-flight append can reference its uploaded object across:
	// the dedup window (a lost-ack republish dedups only within it) plus two publish
	// deadlines (the original attempt + the single bounded resolve retry).
	maxRetryWindow := dedupWindow + 2*defaultAppendTimeout
	if gcGraceWindow <= maxRetryWindow {
		t.Fatalf("gcGraceWindow = %v, want > maxRetryWindow %v (in-flight uploads would be reaped)", gcGraceWindow, maxRetryWindow)
	}
}

// TestWithGCClock covers the clock option: a nil clock is ignored (default kept) and a
// non-nil clock is applied.
func TestWithGCClock(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		clock   GCClock
		wantNil bool // want o.now left at the default (we set a sentinel default below)
	}{
		{name: "nil clock ignored", clock: nil, wantNil: true},
		{name: "non-nil clock applied", clock: func() time.Time { return time.Unix(42, 0) }, wantNil: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sentinel := time.Unix(7, 0)
			o := gcOptions{now: func() time.Time { return sentinel }}
			WithGCClock(tt.clock)(&o)
			got := o.now()
			if tt.wantNil && !got.Equal(sentinel) {
				t.Errorf("now() = %v, want default sentinel %v (nil clock must be ignored)", got, sentinel)
			}
			if !tt.wantNil && got.Equal(sentinel) {
				t.Errorf("now() = default sentinel %v, want the injected clock", got)
			}
		})
	}
}

// TestGCReap is the table-driven core of the reap decision: orphan past grace is
// deleted; referenced is kept; within-grace is kept; a lost lease mid-pass stops
// deleting; a delete failure fails closed.
func TestGCReap(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x01)
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * gcGraceWindow) // safely past the grace window
	fresh := now.Add(-time.Second)     // well within the grace window

	const refID = "1111111111111111111111111111111111111111111111111111111111111111"
	const orphanID = "2222222222222222222222222222222222222222222222222222222222222222"
	const freshID = "3333333333333333333333333333333333333333333333333333333333333333"

	tests := []struct {
		name        string
		valid       bool
		delErr      error
		objs        []*nats.ObjectInfo
		referenced  map[string]struct{}
		wantDeleted []string
		wantResult  GCResult
		wantErr     bool
	}{
		{
			name:        "orphan past grace is reaped",
			valid:       true,
			objs:        []*nats.ObjectInfo{objInfo(orphanID, old)},
			referenced:  map[string]struct{}{},
			wantDeleted: []string{orphanID},
			wantResult:  GCResult{Scanned: 1, Deleted: 1},
		},
		{
			name:        "referenced object is kept",
			valid:       true,
			objs:        []*nats.ObjectInfo{objInfo(refID, old)},
			referenced:  map[string]struct{}{refID: {}},
			wantDeleted: nil,
			wantResult:  GCResult{Scanned: 1, Referenced: 1},
		},
		{
			name:        "within-grace orphan is kept",
			valid:       true,
			objs:        []*nats.ObjectInfo{objInfo(freshID, fresh)},
			referenced:  map[string]struct{}{},
			wantDeleted: nil,
			wantResult:  GCResult{Scanned: 1, WithinGrace: 1},
		},
		{
			name:  "mixed inventory: only the aged orphan is reaped",
			valid: true,
			objs: []*nats.ObjectInfo{
				objInfo(refID, old),
				objInfo(orphanID, old),
				objInfo(freshID, fresh),
			},
			referenced:  map[string]struct{}{refID: {}},
			wantDeleted: []string{orphanID},
			wantResult:  GCResult{Scanned: 3, Referenced: 1, WithinGrace: 1, Deleted: 1},
		},
		{
			name:        "empty inventory deletes nothing",
			valid:       true,
			objs:        nil,
			referenced:  map[string]struct{}{},
			wantDeleted: nil,
			wantResult:  GCResult{},
		},
		{
			name:        "lost lease refuses before any delete",
			valid:       false,
			objs:        []*nats.ObjectInfo{objInfo(orphanID, old)},
			referenced:  map[string]struct{}{},
			wantDeleted: nil,
			wantErr:     true,
		},
		{
			name:        "delete failure fails closed",
			valid:       true,
			delErr:      errors.New("boom"),
			objs:        []*nats.ObjectInfo{objInfo(orphanID, old)},
			referenced:  map[string]struct{}{},
			wantDeleted: nil, // the fake records nothing on a failing Delete
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lister := &gcFakeLister{objs: tt.objs, delErr: tt.delErr}
			lease := &gcStubLease{epoch: 9, sessionID: sid, valid: tt.valid}
			g := &ObjectGC{
				js:        nil, // reap never touches js
				objects:   lister,
				lease:     lease,
				sessionID: sid,
				now:       func() time.Time { return now },
			}

			res, err := g.reap(tt.referenced, tt.objs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("reap() err = %v, wantErr %v", err, tt.wantErr)
			}
			if len(lister.deleted) != len(tt.wantDeleted) {
				t.Fatalf("deleted = %v, want %v", lister.deleted, tt.wantDeleted)
			}
			for i, name := range tt.wantDeleted {
				if lister.deleted[i] != name {
					t.Errorf("deleted[%d] = %q, want %q", i, lister.deleted[i], name)
				}
			}
			if !tt.wantErr && res != tt.wantResult {
				t.Errorf("reap() result = %+v, want %+v", res, tt.wantResult)
			}
		})
	}
}

// TestGCLeaseNotHeldErrorUnwrap asserts the refusal error unwraps to a *LeaseLostError
// so a caller can errors.As the underlying loss cause, and carries the session/epoch.
func TestGCLeaseNotHeldErrorUnwrap(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x02)
	err := &GCLeaseNotHeldError{SessionID: sid, Epoch: 5}
	var lost *LeaseLostError
	if !errors.As(err, &lost) {
		t.Fatalf("GCLeaseNotHeldError does not unwrap to *LeaseLostError")
	}
	if lost.SessionID != sid || lost.Epoch != 5 {
		t.Errorf("unwrapped LeaseLostError = %+v, want session %v epoch 5", lost, sid)
	}
}
