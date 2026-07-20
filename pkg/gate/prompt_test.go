package gate

import "testing"

func TestApprovalControlsExposeExactlyThreeExactActions(t *testing.T) {
	controls := ApprovalControls()
	want := []Control{
		{Action: "Approve", Label: "Approve"},
		{Action: "Approve always for this workspace", Label: "Approve always for this workspace"},
		{Action: "Deny", Label: "Deny"},
	}
	if len(controls) != len(want) {
		t.Fatalf("ApprovalControls() = %#v, want exactly three actions", controls)
	}
	for i, control := range controls {
		if control != want[i] {
			t.Fatalf("ApprovalControls()[%d] = %#v, want %#v", i, control, want[i])
		}
	}
}

func TestApprovalControlsReturnsIndependentSlices(t *testing.T) {
	first := ApprovalControls()
	first[0].Action = "mutated"
	if second := ApprovalControls(); second[0].Action != string(ApprovalApprove) {
		t.Fatal("ApprovalControls() shares backing storage across calls")
	}
}
