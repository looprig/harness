package journal

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/inventivepotter/urvi/internal/agent/loop/command"
	"github.com/inventivepotter/urvi/internal/agent/loop/event"
	"github.com/inventivepotter/urvi/internal/agent/loop/identity"
	"github.com/nats-io/nats.go"
)

// TestNewSessionJournalNilJetStream asserts the constructor fails closed when handed
// a nil JetStream context: it returns a *StreamSetupError unwrapping to the
// errNilJetStream sentinel rather than dereferencing nil at the first management call.
func TestNewSessionJournalNilJetStream(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x01)

	got, err := NewSessionJournal(nil, sid)
	if err == nil {
		t.Fatalf("NewSessionJournal(nil, sid) err = nil, want error")
	}
	if got != nil {
		t.Errorf("NewSessionJournal(nil, sid) journal = %v, want nil", got)
	}
	var setupErr *StreamSetupError
	if !errors.As(err, &setupErr) {
		t.Fatalf("error %v is not *StreamSetupError", err)
	}
	if setupErr.Stream != StreamName(sid) {
		t.Errorf("StreamSetupError.Stream = %q, want %q", setupErr.Stream, StreamName(sid))
	}
	if !errors.Is(err, errNilJetStream) {
		t.Errorf("error %v does not unwrap to errNilJetStream", err)
	}
}

// foreignRecord is a JournalRecord whose concrete type is outside the sealed sum the
// serializer encodes. It exists only to drive marshalRecord's default arm: an
// in-package record can never reach it (the sum is sealed by the unexported marker),
// so this is the only way to prove the default fails closed with a typed error
// instead of panicking.
type foreignRecord struct {
	subject string
	id      string
}

func (foreignRecord) isJournalRecord()        {}
func (r foreignRecord) Subject() string       { return r.subject }
func (r foreignRecord) IdempotencyID() string { return r.id }

// TestMarshalRecord covers marshalRecord's dispatch over the sealed sum: each in-sum
// record kind round-trips to its codec's output, and a foreign record (outside the
// sum) hits the default arm and yields a typed *RecordKindError.
func TestMarshalRecord(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x11)
	lid := fixedUUID(0x12)
	evID := fixedUUID(0x13)
	cmdID := fixedUUID(0x14)

	ev := event.SessionStarted{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: sid},
			EventID:     evID,
		},
	}
	wantEvent, err := event.MarshalEvent(ev)
	if err != nil {
		t.Fatalf("fixture event.MarshalEvent: %v", err)
	}

	cmd := command.Interrupt{Header: command.Header{CommandID: cmdID}}
	wantCommand, err := command.MarshalCommand(cmd)
	if err != nil {
		t.Fatalf("fixture command.MarshalCommand: %v", err)
	}

	fence := LeaseFence{Epoch: 7}
	wantFence, err := MarshalLeaseFence(fence)
	if err != nil {
		t.Fatalf("fixture MarshalLeaseFence: %v", err)
	}

	tests := []struct {
		name    string
		rec     JournalRecord
		want    []byte
		wantErr bool
	}{
		{
			name: "event record encodes via event codec",
			rec:  NewEventRecord(ev),
			want: wantEvent,
		},
		{
			name: "command record encodes via command codec",
			rec:  NewCommandRecord(sid, lid, cmd),
			want: wantCommand,
		},
		{
			name: "fence record encodes via fence codec",
			rec:  NewFenceRecord(sid, fence),
			want: wantFence,
		},
		{
			name:    "foreign record hits default arm with typed error",
			rec:     foreignRecord{subject: FenceSubject(sid), id: "x"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := marshalRecord(tt.rec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("marshalRecord() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var kindErr *RecordKindError
				if !errors.As(err, &kindErr) {
					t.Fatalf("error %v is not *RecordKindError", err)
				}
				if kindErr.Subject != tt.rec.Subject() {
					t.Errorf("RecordKindError.Subject = %q, want %q", kindErr.Subject, tt.rec.Subject())
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("marshalRecord() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWithAppendTimeout asserts the option sets a positive timeout and ignores a
// non-positive one (the default is retained), per the journal-owns-its-invariants
// contract.
func TestWithAppendTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		give time.Duration
		want time.Duration
	}{
		{name: "positive value is applied", give: 3 * time.Second, want: 3 * time.Second},
		{name: "zero is ignored (default retained)", give: 0, want: defaultAppendTimeout},
		{name: "negative is ignored (default retained)", give: -1 * time.Second, want: defaultAppendTimeout},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := journalOptions{appendTimeout: defaultAppendTimeout}
			WithAppendTimeout(tt.give)(&o)
			if o.appendTimeout != tt.want {
				t.Errorf("appendTimeout = %v, want %v", o.appendTimeout, tt.want)
			}
		})
	}
}

// TestStreamConfigDurabilityContract pins the per-session stream's durability
// contract field by field: this is the keep-everything guarantee, so a regression
// flipping retention to WorkQueue, enabling discard/expiry, or dropping the explicit
// dedup window MUST fail here.
func TestStreamConfigDurabilityContract(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x21)
	cfg := streamConfig(sid)

	if cfg == nil {
		t.Fatal("streamConfig returned nil")
	}
	if cfg.Name != StreamName(sid) {
		t.Errorf("Name = %q, want %q", cfg.Name, StreamName(sid))
	}
	wantSubjects := []string{streamSubjectFilter(sid)}
	if !reflect.DeepEqual(cfg.Subjects, wantSubjects) {
		t.Errorf("Subjects = %v, want %v", cfg.Subjects, wantSubjects)
	}
	// The subject filter is exactly the session-rooted wildcard.
	if want := "urvi.session." + sid.String() + ".>"; cfg.Subjects[0] != want {
		t.Errorf("Subjects[0] = %q, want %q", cfg.Subjects[0], want)
	}
	if cfg.Retention != nats.LimitsPolicy {
		t.Errorf("Retention = %v, want LimitsPolicy (keep everything)", cfg.Retention)
	}
	if cfg.MaxAge != 0 {
		t.Errorf("MaxAge = %v, want 0 (no age expiry)", cfg.MaxAge)
	}
	if cfg.MaxMsgs != -1 {
		t.Errorf("MaxMsgs = %d, want -1 (unlimited)", cfg.MaxMsgs)
	}
	if cfg.MaxBytes != -1 {
		t.Errorf("MaxBytes = %d, want -1 (unlimited)", cfg.MaxBytes)
	}
	if cfg.MaxMsgsPerSubject != -1 {
		t.Errorf("MaxMsgsPerSubject = %d, want -1 (no per-subject cap)", cfg.MaxMsgsPerSubject)
	}
	if cfg.Replicas != 1 {
		t.Errorf("Replicas = %d, want 1 (embedded single node)", cfg.Replicas)
	}
	if cfg.Duplicates != dedupWindow {
		t.Errorf("Duplicates = %v, want %v (explicit dedup window)", cfg.Duplicates, dedupWindow)
	}
	// Discard policy must stay the (default) DiscardOld semantics under Limits, never
	// a configuration that drops records to bound the stream.
	if cfg.Discard != nats.DiscardOld {
		t.Errorf("Discard = %v, want DiscardOld default (no aggressive discard)", cfg.Discard)
	}
}
