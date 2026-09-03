package event

import "testing"

// TestSessionResidencyReleasedIsNonterminalPublicSessionEvent pins the shape of the
// nonterminal residency-release record against the terminal SessionStopped it must
// never be confused with. Both are Enduring, session-scoped and Public — the
// difference is carried by the TYPE and by the two fields only this one has, so a
// projection that switches on the type reads them apart.
func TestSessionResidencyReleasedIsNonterminalPublicSessionEvent(t *testing.T) {
	t.Parallel()
	ev := SessionResidencyReleased{Header: fullHeaderSession(), CheckpointSeq: 42, LeaseEpoch: 7}
	if got := ev.Class(); got != Enduring {
		t.Errorf("Class() = %v, want Enduring (the release is a durable replay input)", got)
	}
	if got := ev.Scope(); got != ScopeSession {
		t.Errorf("Scope() = %v, want ScopeSession", got)
	}
	if ev.EndsTurn() {
		t.Error("EndsTurn() = true; residency release is not a turn terminal")
	}
	if got := ev.Visibility(); got != Public {
		t.Errorf("Visibility() = %v, want Public", got)
	}
	if err := ValidateEvent(ev); err != nil {
		t.Errorf("ValidateEvent: %v", err)
	}
	name, _, ok := classify(ev)
	if !ok || name != "SessionResidencyReleased" {
		t.Errorf("classify = (%q, ok=%v), want (\"SessionResidencyReleased\", true)", name, ok)
	}
	if _, isStopped := any(ev).(SessionStopped); isStopped {
		t.Error("SessionResidencyReleased is assignable to SessionStopped")
	}
}

// TestSessionResidencyReleasedCarriesCheckpointAndEpochThroughCodec proves the two
// payload fields survive the durable codec. They are the whole reason the event
// exists: a successor reads which checkpoint the release is anchored to and which
// lease epoch produced it.
func TestSessionResidencyReleasedCarriesCheckpointAndEpochThroughCodec(t *testing.T) {
	t.Parallel()
	ev := SessionResidencyReleased{Header: fullHeaderSession(), CheckpointSeq: 4242, LeaseEpoch: 9}
	data, err := MarshalEvent(ev)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	back, err := UnmarshalEvent(data)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	got, ok := back.(SessionResidencyReleased)
	if !ok {
		t.Fatalf("UnmarshalEvent returned %T, want SessionResidencyReleased", back)
	}
	if got.CheckpointSeq != ev.CheckpointSeq {
		t.Errorf("CheckpointSeq = %d, want %d", got.CheckpointSeq, ev.CheckpointSeq)
	}
	if got.LeaseEpoch != ev.LeaseEpoch {
		t.Errorf("LeaseEpoch = %d, want %d", got.LeaseEpoch, ev.LeaseEpoch)
	}
}
