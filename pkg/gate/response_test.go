package gate

import (
	"encoding/json"
	"testing"

	"github.com/looprig/core/uuid"
)

// TestGateResponseClassifierSourceJSONRoundTrip proves ResponseFromClassifier
// is a real member of the closed ResponseSourceKind domain and survives the
// same JSON round trip every other source kind already does (see
// TestGateJSONRoundTrip in gate_test.go, which exercises ResponseFromPolicy).
// It carries no special encoding: the classifier provenance is ordinary
// stored data on GateResponse/GateResolved, distinguished only by WHO is
// permitted to construct it (internal/sessionruntime's private response
// path), never by its wire shape.
func TestGateResponseClassifierSourceJSONRoundTrip(t *testing.T) {
	t.Parallel()
	response := GateResponse{
		GateID: ID(uuid.MustParse("123e4567-e89b-12d3-a456-426614174099")),
		Action: string(ApprovalApprove),
		Source: ResponseSource{Kind: ResponseFromClassifier, Reason: "risk-classifier@rev-1"},
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var roundTrip GateResponse
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if roundTrip.Source.Kind != ResponseFromClassifier || roundTrip.Source.Reason != "risk-classifier@rev-1" {
		t.Fatalf("response source = %+v, want classifier provenance", roundTrip.Source)
	}
}
