package sessionstore

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"

	coresessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	durablestore "github.com/looprig/sessionstore"
)

// tool_results.go is the harness store's implementation of READABLE tool-result
// retention, and the journal-evidence lookup an object route's policy needs.
//
// Captures are SessionStore "tool-result" objects in the SAME scope as the
// session's journal: this Store's tenant and the runtime session's canonical
// UUID. They therefore live exactly as long as the journal that references them
// and are untouched by workspace deletion, Host release or Host replacement.
//
// Retention is the journal's lifetime and nothing reclaims them: an object whose
// publish succeeded but whose StepDone never committed is an unreferenced orphan
// that stays until a SessionStore reaping API exists. That is a non-claim, not a
// defect this file can fix — the released store exports no enumeration or
// deletion of objects.

// ToolResultObjects returns the loop.ToolResultObjects seam over this Store,
// for rig.WithToolResultObjects. Wire it on the rig that journals into this same
// Store, so every capture lands beside the journal that references it.
func (s *Store) ToolResultObjects() loop.ToolResultObjects { return storeToolResultObjects{store: s} }

type storeToolResultObjects struct{ store *Store }

func (o storeToolResultObjects) PublishToolResultObject(ctx context.Context, session uuid.UUID, content io.Reader, size uint64, sum [32]byte) (coresessionwire.ObjectMetadata, error) {
	metadata, err := o.store.durable.PutObject(ctx, durablestore.PutObjectRequest{
		TenantID:  o.store.opts.TenantID,
		SessionID: harnessSessionID(session),
		Kind:      durablestore.ObjectKindToolResult,
		SizeBytes: size,
		SHA256:    sum,
		Body:      content,
	})
	if err != nil {
		return coresessionwire.ObjectMetadata{}, classifyToolResultObjectErr(err)
	}
	return metadata, nil
}

func (o storeToolResultObjects) OpenToolResultObject(ctx context.Context, session uuid.UUID, ref coresessionwire.ObjectReference) (coresessionwire.ObjectMetadata, io.ReadCloser, error) {
	metadata, err := o.store.durable.GetObjectMetadata(ctx, durablestore.GetObjectMetadataRequest{
		TenantID:     o.store.opts.TenantID,
		SessionID:    harnessSessionID(session),
		ExpectedKind: durablestore.ObjectKindToolResult,
		Reference:    ref,
	})
	if err != nil {
		return coresessionwire.ObjectMetadata{}, nil, classifyToolResultObjectErr(err)
	}
	reader, err := o.store.durable.GetObject(ctx, durablestore.GetObjectRequest{
		TenantID:     o.store.opts.TenantID,
		SessionID:    harnessSessionID(session),
		ExpectedKind: durablestore.ObjectKindToolResult,
		Metadata:     metadata,
	})
	if err != nil {
		return coresessionwire.ObjectMetadata{}, nil, classifyToolResultObjectErr(err)
	}
	return metadata, integrityClassifyingReader{ReadCloser: reader}, nil
}

// integrityClassifyingReader maps the released store's integrity failures onto
// loop.ErrToolResultObjectIntegrity, so a reader can tell corruption from
// unavailability without importing SessionStore.
type integrityClassifyingReader struct{ io.ReadCloser }

func (r integrityClassifyingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = classifyToolResultObjectErr(err)
	}
	return n, err
}

func (r integrityClassifyingReader) Close() error {
	return classifyToolResultObjectErr(r.ReadCloser.Close())
}

// toolResultIntegrityError wraps a store failure that is evidence of corrupted
// or substituted bytes. It matches both the original cause and
// loop.ErrToolResultObjectIntegrity.
type toolResultIntegrityError struct{ cause error }

func (e *toolResultIntegrityError) Error() string { return e.cause.Error() }
func (e *toolResultIntegrityError) Unwrap() []error {
	return []error{loop.ErrToolResultObjectIntegrity, e.cause}
}

func classifyToolResultObjectErr(err error) error {
	if err == nil {
		return nil
	}
	var objectErr *durablestore.ObjectError
	if errors.As(err, &objectErr) {
		switch objectErr.Code {
		case durablestore.ObjectErrorSize, durablestore.ObjectErrorDigest, durablestore.ObjectErrorIntegrity:
			return &toolResultIntegrityError{cause: err}
		}
	}
	return err
}

// ToolResultCaptureScanBudget is how many journal records
// LookupToolResultCapture examines, newest first, before giving up. It bounds
// the cost of a lookup whose reference is absent or very old; a found capture
// is cached, so the cost is paid once per process.
const ToolResultCaptureScanBudget = 1 << 16

// toolResultCaptureScanWindow is how many records one backward step replays.
const toolResultCaptureScanWindow = 256

// maxCachedToolResultCaptures bounds the positive lookup cache per Store.
const maxCachedToolResultCaptures = 4096

// ToolResultCaptureScanBudgetError reports that LookupToolResultCapture examined
// ToolResultCaptureScanBudget records without finding the reference and without
// reaching the start of the journal. It is NOT a "not found": the evidence may
// exist below the scanned window, so a policy must refuse rather than conclude
// the reference is foreign.
type ToolResultCaptureScanBudgetError struct {
	SessionID uuid.UUID
	Examined  uint64
}

func (e *ToolResultCaptureScanBudgetError) Error() string {
	return "sessionstore: tool result capture lookup for session " + e.SessionID.String() +
		" examined " + strconv.FormatUint(e.Examined, 10) + " records without reaching the journal start"
}

// LookupToolResultCapture finds the COMMITTED capture record that references
// ref in runtimeSession's journal. It is the evidence an object route's policy
// needs before serving a tool-result object: that the object is referenced by a
// public StepDone this session committed, not merely that a metadata row exists
// (which an unreferenced orphan also has).
//
// It scans the session's PUBLIC events backwards from the tip, in windows, up to
// ToolResultCaptureScanBudget records; exhausting the budget is a typed error,
// never (zero, false, nil). A found capture is cached per Store — a committed
// record is immutable, so the cache is never stale — and a miss is not cached.
// (false, nil) means the WHOLE journal was scanned and no committed capture names
// ref.
func (s *Store) LookupToolResultCapture(ctx context.Context, runtimeSession uuid.UUID, ref coresessionwire.ObjectReference) (event.ToolResultCapture, bool, error) {
	if err := ref.Validate(); err != nil {
		return event.ToolResultCapture{}, false, err
	}
	key := toolResultCaptureKey{session: runtimeSession, objectID: ref.ObjectID}
	if capture, ok := s.captures.get(key); ok {
		return capture, true, nil
	}
	name, err := sessionName(runtimeSession)
	if err != nil {
		return event.ToolResultCapture{}, false, err
	}
	tip, err := s.backend.Ledger.Tip(ctx, name)
	if err != nil {
		return event.ToolResultCapture{}, false, &ReplayReadError{Name: name, Cause: err}
	}
	var examined uint64
	for end := tip; end > 0; {
		if examined >= ToolResultCaptureScanBudget {
			return event.ToolResultCapture{}, false, &ToolResultCaptureScanBudgetError{SessionID: runtimeSession, Examined: examined}
		}
		start := uint64(1)
		if end > toolResultCaptureScanWindow {
			start = end - toolResultCaptureScanWindow + 1
		}
		capture, found, err := s.scanToolResultCaptures(ctx, runtimeSession, start, end, ref.ObjectID)
		if err != nil {
			return event.ToolResultCapture{}, false, err
		}
		if found {
			s.captures.put(key, capture)
			return capture, true, nil
		}
		examined += end - start + 1
		end = start - 1
	}
	return event.ToolResultCapture{}, false, nil
}

// scanToolResultCaptures replays public events in [start, end] and returns the
// capture naming objectID. A window holds at most one match that matters: a
// store-issued reference is unique, so the first found is the answer.
func (s *Store) scanToolResultCaptures(ctx context.Context, id uuid.UUID, start, end uint64, objectID string) (event.ToolResultCapture, bool, error) {
	replayer, err := s.OpenEventReplayer(id, ReplayRequest{FromSeq: start})
	if err != nil {
		return event.ToolResultCapture{}, false, err
	}
	cursor, err := replayer.Open(ctx, journal.ReplayRequest{SessionID: id, From: journal.Beginning()})
	if err != nil {
		return event.ToolResultCapture{}, false, err
	}
	defer func() { _ = cursor.Close() }()
	for {
		ev, seq, err := cursor.Next(ctx)
		if errors.Is(err, io.EOF) {
			return event.ToolResultCapture{}, false, nil
		}
		if err != nil {
			return event.ToolResultCapture{}, false, err
		}
		if seq > end {
			return event.ToolResultCapture{}, false, nil
		}
		done, ok := ev.(event.StepDone)
		if !ok {
			continue
		}
		for _, capture := range done.Captures {
			if capture.Reference != nil && capture.Reference.ObjectID == objectID {
				return capture, true, nil
			}
		}
	}
}

type toolResultCaptureKey struct {
	session  uuid.UUID
	objectID string
}

// toolResultCaptureCache is a bounded positive cache. When full, an arbitrary
// entry is evicted: every entry is equally cheap to recompute and none is ever
// stale, so no recency bookkeeping is worth its cost.
type toolResultCaptureCache struct {
	mu      sync.Mutex
	entries map[toolResultCaptureKey]event.ToolResultCapture
}

func newToolResultCaptureCache() *toolResultCaptureCache {
	return &toolResultCaptureCache{entries: map[toolResultCaptureKey]event.ToolResultCapture{}}
}

func (c *toolResultCaptureCache) get(key toolResultCaptureKey) (event.ToolResultCapture, bool) {
	if c == nil {
		return event.ToolResultCapture{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	capture, ok := c.entries[key]
	return capture, ok
}

func (c *toolResultCaptureCache) put(key toolResultCaptureKey, capture event.ToolResultCapture) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCachedToolResultCaptures {
		for evict := range c.entries {
			delete(c.entries, evict)
			break
		}
	}
	c.entries[key] = capture
}
