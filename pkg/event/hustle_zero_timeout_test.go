package event

import (
	"reflect"
	"strings"
	"testing"
)

func TestHustleStartedZeroTimeoutReplays(t *testing.T) {
	t.Parallel()
	run := exhaustiveHustleRun(ModelRuntime{})
	run.Definition.TimeoutNanos = 0
	original := HustleStarted{Header: exhaustiveHustleHeader(), Run: run}
	raw, err := MarshalEvent(original)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	decoded, err := UnmarshalEvent(raw)
	if err != nil {
		t.Fatalf("UnmarshalEvent: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("round trip = %#v, want %#v", decoded, original)
	}
	bad := strings.Replace(string(raw), `"TimeoutNanos":0`, `"TimeoutNanos":-1`, 1)
	if bad == string(raw) {
		t.Fatalf("fixture has no TimeoutNanos:0 member: %s", raw)
	}
	if _, err := UnmarshalEvent([]byte(bad)); err == nil {
		t.Fatal("UnmarshalEvent accepted a negative timeout")
	}
}
