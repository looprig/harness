package journal

import (
	"errors"
	"testing"

	"github.com/ciram-co/looprig/pkg/uuid"
)

// fixedUUID builds a deterministic non-zero uuid from a single seed byte so the
// table tests round-trip stable, readable ids.
func fixedUUID(seed byte) uuid.UUID {
	var u uuid.UUID
	for i := range u {
		u[i] = seed
	}
	return u
}

func TestStreamName(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0xab)
	got := StreamName(sid)
	// NATS stream names may not contain '.', ' ', '*' or '>'; dashes are fine.
	for _, r := range got {
		switch r {
		case '.', ' ', '*', '>':
			t.Fatalf("StreamName(%s) = %q contains forbidden rune %q", sid, got, r)
		}
	}
	if got == "" {
		t.Fatalf("StreamName returned empty string")
	}
	// Stable, deterministic for the same id.
	if again := StreamName(sid); again != got {
		t.Fatalf("StreamName not deterministic: %q != %q", got, again)
	}
}

func TestSessionEventSubjectRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sid  uuid.UUID
	}{
		{name: "non-zero session", sid: fixedUUID(0x01)},
		{name: "zero session", sid: uuid.UUID{}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			subj := SessionEventSubject(tt.sid)
			kind, sid, lid, err := ParseSubject(subj)
			if err != nil {
				t.Fatalf("ParseSubject(%q) err = %v", subj, err)
			}
			if kind != SubjectSessionEvent {
				t.Errorf("kind = %v, want SubjectSessionEvent", kind)
			}
			if sid != tt.sid {
				t.Errorf("sid = %s, want %s", sid, tt.sid)
			}
			if !lid.IsZero() {
				t.Errorf("lid = %s, want zero for a session subject", lid)
			}
			if !IsEventSubject(subj) {
				t.Errorf("IsEventSubject(%q) = false, want true", subj)
			}
		})
	}
}

func TestLoopEventSubjectRoundTrip(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x02)
	lid := fixedUUID(0x03)
	subj := LoopEventSubject(sid, lid)
	kind, gotSID, gotLID, err := ParseSubject(subj)
	if err != nil {
		t.Fatalf("ParseSubject(%q) err = %v", subj, err)
	}
	if kind != SubjectLoopEvent {
		t.Errorf("kind = %v, want SubjectLoopEvent", kind)
	}
	if gotSID != sid {
		t.Errorf("sid = %s, want %s", gotSID, sid)
	}
	if gotLID != lid {
		t.Errorf("lid = %s, want %s", gotLID, lid)
	}
	if !IsEventSubject(subj) {
		t.Errorf("IsEventSubject(%q) = false, want true", subj)
	}
}

func TestLoopCommandSubjectRoundTrip(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x04)
	lid := fixedUUID(0x05)
	subj := LoopCommandSubject(sid, lid)
	kind, gotSID, gotLID, err := ParseSubject(subj)
	if err != nil {
		t.Fatalf("ParseSubject(%q) err = %v", subj, err)
	}
	if kind != SubjectLoopCommand {
		t.Errorf("kind = %v, want SubjectLoopCommand", kind)
	}
	if gotSID != sid {
		t.Errorf("sid = %s, want %s", gotSID, sid)
	}
	if gotLID != lid {
		t.Errorf("lid = %s, want %s", gotLID, lid)
	}
	if IsEventSubject(subj) {
		t.Errorf("IsEventSubject(%q) = true, want false for a command subject", subj)
	}
}

func TestFenceSubjectRoundTrip(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x06)
	subj := FenceSubject(sid)
	kind, gotSID, gotLID, err := ParseSubject(subj)
	if err != nil {
		t.Fatalf("ParseSubject(%q) err = %v", subj, err)
	}
	if kind != SubjectFence {
		t.Errorf("kind = %v, want SubjectFence", kind)
	}
	if gotSID != sid {
		t.Errorf("sid = %s, want %s", gotSID, sid)
	}
	if !gotLID.IsZero() {
		t.Errorf("lid = %s, want zero for a fence subject", gotLID)
	}
	if IsEventSubject(subj) {
		t.Errorf("IsEventSubject(%q) = true, want false for a fence subject", subj)
	}
}

func TestParseSubjectErrors(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x07)
	lid := fixedUUID(0x08)
	tests := []struct {
		name string
		subj string
	}{
		{name: "empty", subj: ""},
		{name: "wrong prefix", subj: "other.session." + sid.String() + ".session"},
		{name: "too few tokens", subj: "urvi.session"},
		{name: "unknown session-leaf", subj: "urvi.session." + sid.String() + ".bogus"},
		{name: "bad session uuid", subj: "urvi.session.not-a-uuid.session"},
		{name: "loop subject missing leaf", subj: "urvi.session." + sid.String() + ".loop." + lid.String()},
		{name: "loop subject unknown leaf", subj: "urvi.session." + sid.String() + ".loop." + lid.String() + ".bogus"},
		{name: "loop subject bad loop uuid", subj: "urvi.session." + sid.String() + ".loop.not-a-uuid.event"},
		{name: "wildcard token", subj: "urvi.session.*.session"},
		{name: "trailing token after fence", subj: "urvi.session." + sid.String() + ".fence.extra"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kind, _, _, err := ParseSubject(tt.subj)
			if err == nil {
				t.Fatalf("ParseSubject(%q) = kind %v, want error", tt.subj, kind)
			}
			var se *SubjectParseError
			if !errors.As(err, &se) {
				t.Fatalf("ParseSubject(%q) err = %T, want *SubjectParseError", tt.subj, err)
			}
		})
	}
}

func TestIsEventSubjectClassification(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x09)
	lid := fixedUUID(0x0a)
	tests := []struct {
		name string
		subj string
		want bool
	}{
		{name: "session event included", subj: SessionEventSubject(sid), want: true},
		{name: "loop event included", subj: LoopEventSubject(sid, lid), want: true},
		{name: "loop command excluded", subj: LoopCommandSubject(sid, lid), want: false},
		{name: "fence excluded", subj: FenceSubject(sid), want: false},
		{name: "garbage excluded", subj: "not.a.subject", want: false},
		{name: "empty excluded", subj: "", want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsEventSubject(tt.subj); got != tt.want {
				t.Errorf("IsEventSubject(%q) = %v, want %v", tt.subj, got, tt.want)
			}
		})
	}
}
