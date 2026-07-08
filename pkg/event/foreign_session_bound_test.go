package event

import (
	"encoding/json"
	"testing"
)

func TestForeignSessionBoundRoundTrip(t *testing.T) {
	t.Parallel()
	in := ForeignSessionBound{ForeignSID: "0199a213-81c0-7800-8aa1-bbab2a035a53"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ForeignSessionBound
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ForeignSID != in.ForeignSID {
		t.Fatalf("ForeignSID = %q, want %q", out.ForeignSID, in.ForeignSID)
	}
	if in.Class() != Enduring || in.Scope() != ScopeLoop || in.EndsTurn() {
		t.Fatalf("ForeignSessionBound class/scope/terminal mismatch")
	}
}
