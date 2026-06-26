package event

import (
	"encoding/json"
	"testing"
)

func TestLoopStartedForeignSIDRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		foreign   string
		wantField bool // is "foreign_sid" present in JSON?
	}{
		{name: "native leaves empty (omitted)", foreign: "", wantField: false},
		{name: "foreign sid present", foreign: "11111111-1111-1111-1111-111111111111", wantField: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := LoopStarted{ForeignSID: tt.foreign}
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !json.Valid(b) {
				t.Fatal("invalid json")
			}
			if has := contains(b, "foreign_sid"); has != tt.wantField {
				t.Fatalf("foreign_sid present=%v, want %v (%s)", has, tt.wantField, b)
			}
			var out LoopStarted
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if out.ForeignSID != tt.foreign {
				t.Fatalf("ForeignSID = %q, want %q", out.ForeignSID, tt.foreign)
			}
		})
	}
}

// replay compat: a legacy record with no foreign_sid key decodes to "".
func TestLoopStartedLegacyRecordDecodesEmpty(t *testing.T) {
	t.Parallel()
	var out LoopStarted
	if err := json.Unmarshal([]byte(`{"parent_tool_use_id":"x"}`), &out); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if out.ForeignSID != "" {
		t.Fatalf("legacy ForeignSID = %q, want empty", out.ForeignSID)
	}
}

func contains(b []byte, s string) bool  { return string(b) != "" && bytesIndex(b, s) >= 0 }
func bytesIndex(b []byte, s string) int { return indexString(string(b), s) }
func indexString(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
