package journal

import (
	"errors"
	"testing"
)

func TestLeaseFenceCodecRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		fence LeaseFence
		want  string
	}{
		{name: "zero epoch", fence: LeaseFence{Epoch: 0}, want: `{"epoch":0}`},
		{name: "one", fence: LeaseFence{Epoch: 1}, want: `{"epoch":1}`},
		{name: "mid", fence: LeaseFence{Epoch: 42}, want: `{"epoch":42}`},
		{name: "max uint64", fence: LeaseFence{Epoch: 1<<64 - 1}, want: `{"epoch":18446744073709551615}`},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := MarshalLeaseFence(tt.fence)
			if err != nil {
				t.Fatalf("MarshalLeaseFence(%+v) err = %v", tt.fence, err)
			}
			if string(data) != tt.want {
				t.Errorf("MarshalLeaseFence = %s, want %s", data, tt.want)
			}
			got, err := UnmarshalLeaseFence(data)
			if err != nil {
				t.Fatalf("UnmarshalLeaseFence(%s) err = %v", data, err)
			}
			if got != tt.fence {
				t.Errorf("round-trip = %+v, want %+v", got, tt.fence)
			}
		})
	}
}

func TestUnmarshalLeaseFenceErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data []byte
	}{
		{name: "nil", data: nil},
		{name: "empty", data: []byte("")},
		{name: "garbage", data: []byte("not json")},
		{name: "wrong type for epoch", data: []byte(`{"epoch":"big"}`)},
		{name: "negative epoch overflows uint64", data: []byte(`{"epoch":-1}`)},
		{name: "trailing junk", data: []byte(`{"epoch":1}trailing`)},
		{name: "array not object", data: []byte(`[1,2,3]`)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := UnmarshalLeaseFence(tt.data)
			if err == nil {
				t.Fatalf("UnmarshalLeaseFence(%q) = nil error, want error", tt.data)
			}
			var de *FenceDecodeError
			if !errors.As(err, &de) {
				t.Fatalf("UnmarshalLeaseFence(%q) err = %T, want *FenceDecodeError", tt.data, err)
			}
		})
	}
}

func TestFenceRecordSubjectAndID(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x31)
	tests := []struct {
		name   string
		epoch  uint64
		wantID string
	}{
		{name: "zero epoch", epoch: 0, wantID: "0"},
		{name: "one", epoch: 1, wantID: "1"},
		{name: "large", epoch: 1 << 40, wantID: "1099511627776"},
		{name: "max uint64", epoch: 1<<64 - 1, wantID: "18446744073709551615"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := NewFenceRecord(sid, LeaseFence{Epoch: tt.epoch})
			if got := rec.Subject(); got != FenceSubject(sid) {
				t.Errorf("Subject() = %q, want %q", got, FenceSubject(sid))
			}
			if got := rec.IdempotencyID(); got != tt.wantID {
				t.Errorf("IdempotencyID() = %q, want %q", got, tt.wantID)
			}
			if rec.Fence() != (LeaseFence{Epoch: tt.epoch}) {
				t.Errorf("Fence() did not return the wrapped fence")
			}
			if IsEventSubject(rec.Subject()) {
				t.Errorf("fence subject %q classified as an event subject", rec.Subject())
			}
		})
	}
}

// TestFenceRecordIsJournalRecord asserts the fence record joins the sealed sum so
// the serializer's switch over JournalRecord stays exhaustive.
func TestFenceRecordIsJournalRecord(t *testing.T) {
	t.Parallel()
	var r JournalRecord = NewFenceRecord(fixedUUID(0x51), LeaseFence{Epoch: 3})
	if r.Subject() == "" {
		t.Errorf("fence record Subject() is empty")
	}
	if r.IdempotencyID() != "3" {
		t.Errorf("fence record IdempotencyID() = %q, want \"3\"", r.IdempotencyID())
	}
}
