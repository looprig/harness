package journal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/nats-io/nats.go"
)

// fixedClock returns a CatalogClock that always reports t — deterministic LastActiveAt.
func fixedClock(t time.Time) CatalogClock { return func() time.Time { return t } }

// userMsg builds a *content.UserMessage carrying a single text block — the shape
// TurnStarted's Message has when deriving a Title.
func userMsg(text string) *content.UserMessage {
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

// TestApplyEvent covers the catalog-relevant event→field mapping: the single source of
// truth applyEvent both the inline update and repair share. It is pure (clock injected),
// so it needs no KV.
func TestApplyEvent(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x01)
	created := time.Date(2026, 6, 21, 8, 0, 0, 0, time.UTC)
	active := time.Date(2026, 6, 21, 9, 30, 0, 0, time.UTC)
	fp := event.ConfigFingerprint{AgentKind: "primary", ModelID: "m1"}

	tests := []struct {
		name        string
		start       SessionMeta
		ev          event.Event
		wantChanged bool
		check       func(*testing.T, SessionMeta)
	}{
		{
			name: "SessionStarted creates the record",
			ev: event.SessionStarted{
				Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: created},
				Config: fp,
			},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.SessionID != sid {
					t.Errorf("SessionID = %v, want %v", m.SessionID, sid)
				}
				if !m.CreatedAt.Equal(created) {
					t.Errorf("CreatedAt = %v, want %v", m.CreatedAt, created)
				}
				if m.Status != StatusActive {
					t.Errorf("Status = %q, want active", m.Status)
				}
				if m.AgentKind != "primary" {
					t.Errorf("AgentKind = %q, want primary (from fingerprint)", m.AgentKind)
				}
				if m.LoopCount != 1 {
					t.Errorf("LoopCount = %d, want 1 (primary counted)", m.LoopCount)
				}
				if !m.ConfigFingerprint.Equal(fp) {
					t.Errorf("ConfigFingerprint = %+v, want %+v", m.ConfigFingerprint, fp)
				}
			},
		},
		{
			name:  "first TurnStarted sets Title and bumps LastActiveAt",
			start: SessionMeta{SessionID: sid, Status: StatusActive},
			ev: event.TurnStarted{
				Header:  event.Header{Coordinates: identity.Coordinates{SessionID: sid}},
				Message: userMsg("Hello there, fix the bug"),
			},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.Title != "Hello there, fix the bug" {
					t.Errorf("Title = %q, want derived from message", m.Title)
				}
				if !m.LastActiveAt.Equal(active) {
					t.Errorf("LastActiveAt = %v, want %v", m.LastActiveAt, active)
				}
			},
		},
		{
			name:  "second TurnStarted does not overwrite Title (first wins) but bumps activity",
			start: SessionMeta{SessionID: sid, Title: "first title"},
			ev: event.TurnStarted{
				Header:  event.Header{Coordinates: identity.Coordinates{SessionID: sid}},
				Message: userMsg("second message"),
			},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.Title != "first title" {
					t.Errorf("Title = %q, want unchanged first title", m.Title)
				}
				if !m.LastActiveAt.Equal(active) {
					t.Errorf("LastActiveAt = %v, want bumped", m.LastActiveAt)
				}
			},
		},
		{
			name:        "StepDone bumps LastActiveAt",
			start:       SessionMeta{SessionID: sid},
			ev:          event.StepDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}}},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if !m.LastActiveAt.Equal(active) {
					t.Errorf("LastActiveAt = %v, want %v", m.LastActiveAt, active)
				}
			},
		},
		{
			name:        "RestoreDone bumps LastActiveAt",
			start:       SessionMeta{SessionID: sid},
			ev:          event.RestoreDone{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}}},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if !m.LastActiveAt.Equal(active) {
					t.Errorf("LastActiveAt = %v, want %v", m.LastActiveAt, active)
				}
			},
		},
		{
			name:        "LoopStarted increments LoopCount",
			start:       SessionMeta{SessionID: sid, LoopCount: 1},
			ev:          event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}}},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.LoopCount != 2 {
					t.Errorf("LoopCount = %d, want 2", m.LoopCount)
				}
			},
		},
		{
			name:        "SessionStopped flips Status",
			start:       SessionMeta{SessionID: sid, Status: StatusActive},
			ev:          event.SessionStopped{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}}},
			wantChanged: true,
			check: func(t *testing.T, m SessionMeta) {
				if m.Status != StatusStopped {
					t.Errorf("Status = %q, want stopped", m.Status)
				}
			},
		},
		{
			name:        "non-catalog event is a no-op",
			start:       SessionMeta{SessionID: sid, Title: "keep"},
			ev:          event.SessionActive{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}}},
			wantChanged: false,
			check: func(t *testing.T, m SessionMeta) {
				if m.Title != "keep" {
					t.Errorf("Title = %q, want unchanged on no-op", m.Title)
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, changed := applyEvent(tt.start, tt.ev, fixedClock(active))
			if changed != tt.wantChanged {
				t.Fatalf("applyEvent changed = %v, want %v", changed, tt.wantChanged)
			}
			tt.check(t, got)
		})
	}
}

// TestDeriveTitle covers the title derivation: first non-empty line of concatenated
// text, rune-truncated; nil/empty yields "".
func TestDeriveTitle(t *testing.T) {
	t.Parallel()
	long := make([]rune, titleMaxLen+10)
	for i := range long {
		long[i] = 'a'
	}
	tests := []struct {
		name string
		msg  *content.UserMessage
		want string
	}{
		{name: "nil message", msg: nil, want: ""},
		{name: "single line", msg: userMsg("do the thing"), want: "do the thing"},
		{name: "multi-line takes first non-empty", msg: userMsg("\n\n  first\nsecond"), want: "first"},
		{name: "blank message", msg: userMsg("   \n  "), want: ""},
		{name: "no text blocks", msg: &content.UserMessage{Message: content.Message{Role: content.RoleUser}}, want: ""},
		{name: "truncated to max runes", msg: userMsg(string(long)), want: string(long[:titleMaxLen])},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := deriveTitle(tt.msg); got != tt.want {
				t.Errorf("deriveTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecodeSessionMeta covers the fail-closed decode boundary: valid round-trips, every
// malformed shape errors rather than guessing a record.
func TestDecodeSessionMeta(t *testing.T) {
	t.Parallel()
	valid := SessionMeta{
		SessionID:    fixedUUID(0x07),
		Title:        "t",
		CreatedAt:    time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC),
		LastActiveAt: time.Date(2026, 6, 21, 13, 0, 0, 0, time.UTC),
		Status:       StatusActive,
		LoopCount:    2,
	}
	validBytes, err := encodeSessionMeta(valid)
	if err != nil {
		t.Fatalf("encodeSessionMeta: %v", err)
	}
	tests := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{name: "valid round-trips", data: validBytes},
		{name: "empty bytes", data: []byte{}, wantErr: true},
		{name: "not an object", data: []byte(`42`), wantErr: true},
		{name: "unknown field", data: []byte(`{"session_id":"x","bogus":1}`), wantErr: true},
		{name: "trailing data", data: []byte(`{}{}`), wantErr: true},
		{name: "zero-value object decodes", data: []byte(`{}`)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeSessionMeta(tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeSessionMeta() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestCatalogOptions covers the constructor knobs: each ignores a zero/empty/nil value
// and applies a valid one.
func TestCatalogOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		apply  func(*catalogOptions)
		assert func(*testing.T, catalogOptions)
	}{
		{
			name:  "bucket applied",
			apply: func(o *catalogOptions) { WithCatalogBucket("custom")(o) },
			assert: func(t *testing.T, o catalogOptions) {
				if o.bucket != "custom" {
					t.Errorf("bucket = %q, want custom", o.bucket)
				}
			},
		},
		{
			name:  "empty bucket ignored",
			apply: func(o *catalogOptions) { WithCatalogBucket("")(o) },
			assert: func(t *testing.T, o catalogOptions) {
				if o.bucket != catalogBucket {
					t.Errorf("bucket = %q, want default", o.bucket)
				}
			},
		},
		{
			name:  "clock applied",
			apply: func(o *catalogOptions) { WithCatalogClock(fixedClock(time.Unix(7, 0)))(o) },
			assert: func(t *testing.T, o catalogOptions) {
				if got := o.now(); !got.Equal(time.Unix(7, 0)) {
					t.Errorf("now() = %v, want 7s epoch", got)
				}
			},
		},
		{
			name:  "nil clock ignored",
			apply: func(o *catalogOptions) { WithCatalogClock(nil)(o) },
			assert: func(t *testing.T, o catalogOptions) {
				if o.now == nil {
					t.Fatal("now nil after WithCatalogClock(nil); default must be retained")
				}
			},
		},
		{
			name:  "nil logger ignored",
			apply: func(o *catalogOptions) { WithCatalogLogger(nil)(o) },
			assert: func(t *testing.T, o catalogOptions) {
				if o.log == nil {
					t.Fatal("log nil after WithCatalogLogger(nil); default must be retained")
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := catalogOptions{bucket: catalogBucket, now: time.Now, log: nopCatalogLogger{}}
			tt.apply(&o)
			tt.assert(t, o)
		})
	}
}

// fakeKV is a minimal nats.KeyValue double for unit-testing the best-effort
// UpdateOnEvent without a live server. It implements only Get/Put/Keys; the embedded
// nil interface makes any other call panic (unused in these tests). getErr/putErr force
// failures to prove the update is best-effort.
type fakeKV struct {
	nats.KeyValue // embedded nil: unimplemented methods panic if ever called
	store         map[string][]byte
	getErr        error
	putErr        error
	puts          int
}

func newFakeKV() *fakeKV { return &fakeKV{store: map[string][]byte{}} }

type fakeEntry struct {
	nats.KeyValueEntry
	key string
	val []byte
}

func (e fakeEntry) Key() string   { return e.key }
func (e fakeEntry) Value() []byte { return e.val }

func (k *fakeKV) Get(key string) (nats.KeyValueEntry, error) {
	if k.getErr != nil {
		return nil, k.getErr
	}
	v, ok := k.store[key]
	if !ok {
		return nil, nats.ErrKeyNotFound
	}
	return fakeEntry{key: key, val: v}, nil
}

func (k *fakeKV) Put(key string, value []byte) (uint64, error) {
	k.puts++
	if k.putErr != nil {
		return 0, k.putErr
	}
	k.store[key] = append([]byte(nil), value...)
	return uint64(len(k.store)), nil
}

func (k *fakeKV) Keys(_ ...nats.WatchOpt) ([]string, error) {
	if k.getErr != nil {
		return nil, k.getErr
	}
	if len(k.store) == 0 {
		return nil, nats.ErrNoKeysFound
	}
	keys := make([]string, 0, len(k.store))
	for key := range k.store {
		keys = append(keys, key)
	}
	return keys, nil
}

// recordingLogger captures best-effort update failures so a test can assert the error
// was logged (and swallowed, never returned).
type recordingLogger struct{ errs []error }

func (l *recordingLogger) CatalogUpdateFailed(err error) { l.errs = append(l.errs, err) }

// TestCatalogUpdateOnEventBestEffort proves UpdateOnEvent (a) upserts on a relevant
// event, (b) NEVER returns an error and logs+swallows a KV failure, and (c) skips KV
// I/O entirely for a non-catalog event.
func TestCatalogUpdateOnEventBestEffort(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11)
	now := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	started := event.SessionStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}, CreatedAt: now},
		Config: event.ConfigFingerprint{ModelID: "m"},
	}

	t.Run("happy path upserts", func(t *testing.T) {
		t.Parallel()
		kv := newFakeKV()
		c := &Catalog{kv: kv, bucket: catalogBucket, now: fixedClock(now), log: nopCatalogLogger{}}
		if err := c.UpdateOnEvent(context.Background(), started); err != nil {
			t.Fatalf("UpdateOnEvent = %v, want nil", err)
		}
		if kv.puts != 1 {
			t.Fatalf("puts = %d, want 1", kv.puts)
		}
		raw, ok := kv.store[sid.String()]
		if !ok {
			t.Fatal("no entry stored under session id")
		}
		meta, err := decodeSessionMeta(raw)
		if err != nil {
			t.Fatalf("decode stored meta: %v", err)
		}
		if meta.Status != StatusActive || meta.SessionID != sid {
			t.Errorf("stored meta = %+v, want active/%v", meta, sid)
		}
	})

	t.Run("KV write failure is logged and swallowed", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("kv down")
		kv := newFakeKV()
		kv.putErr = boom
		log := &recordingLogger{}
		c := &Catalog{kv: kv, bucket: catalogBucket, now: fixedClock(now), log: log}
		if err := c.UpdateOnEvent(context.Background(), started); err != nil {
			t.Fatalf("UpdateOnEvent = %v, want nil (best-effort)", err)
		}
		if len(log.errs) != 1 {
			t.Fatalf("logged %d errors, want 1", len(log.errs))
		}
		var we *CatalogWriteError
		if !errors.As(log.errs[0], &we) {
			t.Errorf("logged error %v is not *CatalogWriteError", log.errs[0])
		}
	})

	t.Run("KV read failure is logged and swallowed", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("kv read down")
		kv := newFakeKV()
		kv.getErr = boom
		log := &recordingLogger{}
		c := &Catalog{kv: kv, bucket: catalogBucket, now: fixedClock(now), log: log}
		if err := c.UpdateOnEvent(context.Background(), started); err != nil {
			t.Fatalf("UpdateOnEvent = %v, want nil (best-effort)", err)
		}
		if len(log.errs) != 1 {
			t.Fatalf("logged %d errors, want 1", len(log.errs))
		}
		var re *CatalogReadError
		if !errors.As(log.errs[0], &re) {
			t.Errorf("logged error %v is not *CatalogReadError", log.errs[0])
		}
		if kv.puts != 0 {
			t.Errorf("puts = %d, want 0 (read failed before write)", kv.puts)
		}
	})

	t.Run("non-catalog event skips KV entirely", func(t *testing.T) {
		t.Parallel()
		kv := newFakeKV()
		c := &Catalog{kv: kv, bucket: catalogBucket, now: fixedClock(now), log: nopCatalogLogger{}}
		ev := event.SessionActive{Header: event.Header{Coordinates: identity.Coordinates{SessionID: sid}}}
		if err := c.UpdateOnEvent(context.Background(), ev); err != nil {
			t.Fatalf("UpdateOnEvent = %v, want nil", err)
		}
		if kv.puts != 0 {
			t.Errorf("puts = %d, want 0 (no-op event)", kv.puts)
		}
	})
}
