package journal

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestLeaseOptions covers the constructor knobs: each ignores a zero/empty/nil value
// (defaults retained) and applies a valid one (manager owns its invariants).
func TestLeaseOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		apply  func(*leaseOptions)
		assert func(*testing.T, leaseOptions)
	}{
		{
			name:  "bucket applied",
			apply: func(o *leaseOptions) { WithLeaseBucket("custom_bucket")(o) },
			assert: func(t *testing.T, o leaseOptions) {
				if o.bucket != "custom_bucket" {
					t.Errorf("bucket = %q, want custom_bucket", o.bucket)
				}
			},
		},
		{
			name:  "empty bucket ignored",
			apply: func(o *leaseOptions) { WithLeaseBucket("")(o) },
			assert: func(t *testing.T, o leaseOptions) {
				if o.bucket != defaultLeaseBucket {
					t.Errorf("bucket = %q, want default %q", o.bucket, defaultLeaseBucket)
				}
			},
		},
		{
			name:  "positive ttl applied",
			apply: func(o *leaseOptions) { WithLeaseTTL(5 * time.Second)(o) },
			assert: func(t *testing.T, o leaseOptions) {
				if o.ttl != 5*time.Second {
					t.Errorf("ttl = %v, want 5s", o.ttl)
				}
			},
		},
		{
			name:  "zero ttl ignored",
			apply: func(o *leaseOptions) { WithLeaseTTL(0)(o) },
			assert: func(t *testing.T, o leaseOptions) {
				if o.ttl != defaultLeaseTTL {
					t.Errorf("ttl = %v, want default %v", o.ttl, defaultLeaseTTL)
				}
			},
		},
		{
			name:  "negative ttl ignored",
			apply: func(o *leaseOptions) { WithLeaseTTL(-1 * time.Second)(o) },
			assert: func(t *testing.T, o leaseOptions) {
				if o.ttl != defaultLeaseTTL {
					t.Errorf("ttl = %v, want default %v", o.ttl, defaultLeaseTTL)
				}
			},
		},
		{
			name:  "clock applied",
			apply: func(o *leaseOptions) { WithLeaseClock(func() time.Time { return time.Unix(42, 0) })(o) },
			assert: func(t *testing.T, o leaseOptions) {
				if got := o.now(); !got.Equal(time.Unix(42, 0)) {
					t.Errorf("now() = %v, want 42s epoch", got)
				}
			},
		},
		{
			name:  "nil clock ignored",
			apply: func(o *leaseOptions) { WithLeaseClock(nil)(o) },
			assert: func(t *testing.T, o leaseOptions) {
				if o.now == nil {
					t.Fatal("now is nil after WithLeaseClock(nil); default must be retained")
				}
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := leaseOptions{bucket: defaultLeaseBucket, ttl: defaultLeaseTTL, now: time.Now}
			tt.apply(&o)
			tt.assert(t, o)
		})
	}
}

// TestDecodeLeaseRecord covers the fail-closed decode boundary: valid JSON round-trips
// and every malformed shape returns an error rather than a guessed record.
func TestDecodeLeaseRecord(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	valid := leaseRecord{Epoch: 7, Holder: "abc", ExpiresAt: exp}
	validBytes, err := encodeLeaseRecord(valid)
	if err != nil {
		t.Fatalf("encodeLeaseRecord: %v", err)
	}

	tests := []struct {
		name    string
		data    []byte
		want    leaseRecord
		wantErr bool
	}{
		{name: "valid round-trips", data: validBytes, want: valid},
		{name: "empty bytes", data: []byte{}, wantErr: true},
		{name: "not an object", data: []byte(`123`), wantErr: true},
		{name: "unknown field", data: []byte(`{"epoch":1,"bogus":2}`), wantErr: true},
		{name: "trailing data", data: []byte(`{"epoch":1}{"epoch":2}`), wantErr: true},
		{name: "negative epoch (uint64)", data: []byte(`{"epoch":-1}`), wantErr: true},
		{name: "zero-value object decodes", data: []byte(`{}`), want: leaseRecord{}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeLeaseRecord(tt.data)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeLeaseRecord() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.Epoch != tt.want.Epoch || got.Holder != tt.want.Holder || !got.ExpiresAt.Equal(tt.want.ExpiresAt) {
				t.Errorf("decodeLeaseRecord() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestLeaseExpired covers the authoritative clock-injectable expiry check: an entry is
// expired exactly when its ExpiresAt is at or before now.
func TestLeaseExpired(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		expiresAt time.Time
		now       time.Time
		want      bool
	}{
		{name: "future expiry is live", expiresAt: base.Add(time.Minute), now: base, want: false},
		{name: "past expiry is expired", expiresAt: base.Add(-time.Minute), now: base, want: true},
		{name: "exactly now is expired", expiresAt: base, now: base, want: true},
		{name: "one ns before now is expired", expiresAt: base.Add(-time.Nanosecond), now: base, want: true},
		{name: "one ns after now is live", expiresAt: base.Add(time.Nanosecond), now: base, want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &LeaseManager{now: func() time.Time { return tt.now }}
			if got := m.expired(leaseRecord{ExpiresAt: tt.expiresAt}); got != tt.want {
				t.Errorf("expired() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBackstopBucketTTL covers the bucket-level backstop TTL: a generous multiple of
// the lease TTL, floored so a very short lease TTL still yields a safely-long bucket
// TTL that never races a deterministic application-level expiry test.
func TestBackstopBucketTTL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{name: "short ttl hits floor", ttl: 200 * time.Millisecond, want: time.Hour},
		{name: "zero ttl hits floor", ttl: 0, want: time.Hour},
		{name: "long ttl scales by 100x", ttl: time.Minute, want: 100 * time.Minute},
		{name: "30s ttl 100x is under floor so floor wins", ttl: 30 * time.Second, want: time.Hour},
		{name: "40s ttl 100x exceeds floor", ttl: 40 * time.Second, want: 4000 * time.Second},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := backstopBucketTTL(tt.ttl); got != tt.want {
				t.Errorf("backstopBucketTTL(%v) = %v, want %v", tt.ttl, got, tt.want)
			}
		})
	}
}

// TestIsKVCASConflict covers the CAS-conflict classifier: the KV ErrKeyExists sentinel
// and a wrong-last-sequence APIError are conflicts; an unrelated error is not.
func TestIsKVCASConflict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "key exists is a conflict", err: nats.ErrKeyExists, want: true},
		{
			name: "wrong-last-seq APIError is a conflict",
			err:  &nats.APIError{ErrorCode: nats.JSErrCodeStreamWrongLastSequence},
			want: true,
		},
		{name: "key not found is not a CAS conflict", err: nats.ErrKeyNotFound, want: false},
		{name: "unrelated error is not a conflict", err: errors.New("boom"), want: false},
		{name: "nil is not a conflict", err: nil, want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isKVCASConflict(tt.err); got != tt.want {
				t.Errorf("isKVCASConflict(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
