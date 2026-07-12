package event

import (
	"encoding/json"
	"testing"
)

// TestLoopStartedDisplayMetadataRoundTrip proves DisplayName/Description survive
// encode->decode and are omitted from JSON when empty (omitzero legacy compat).
func TestLoopStartedDisplayMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		displayName    string
		description    string
		wantDisplayKey bool
		wantDescKey    bool
	}{
		{name: "both empty omitted", displayName: "", description: "", wantDisplayKey: false, wantDescKey: false},
		{name: "display name only", displayName: "Planner", description: "", wantDisplayKey: true, wantDescKey: false},
		{name: "description only", displayName: "", description: "plans the work", wantDisplayKey: false, wantDescKey: true},
		{name: "both present", displayName: "Planner", description: "plans the work", wantDisplayKey: true, wantDescKey: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := LoopStarted{DisplayName: tt.displayName, Description: tt.description}
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !json.Valid(b) {
				t.Fatalf("invalid json: %s", b)
			}
			if has := contains(b, "display_name"); has != tt.wantDisplayKey {
				t.Fatalf("display_name present=%v, want %v (%s)", has, tt.wantDisplayKey, b)
			}
			if has := contains(b, "description"); has != tt.wantDescKey {
				t.Fatalf("description present=%v, want %v (%s)", has, tt.wantDescKey, b)
			}
			var out LoopStarted
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if out.DisplayName != tt.displayName {
				t.Fatalf("DisplayName = %q, want %q", out.DisplayName, tt.displayName)
			}
			if out.Description != tt.description {
				t.Fatalf("Description = %q, want %q", out.Description, tt.description)
			}
		})
	}
}

// TestLoopStartedDecodesDisplayMetadata proves a legacy journal record with neither
// field decodes to "" without error, and that a record carrying both fields decodes
// them faithfully.
func TestLoopStartedDecodesDisplayMetadata(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		record          string
		wantDisplayName string
		wantDescription string
	}{
		{
			name:            "legacy record without fields decodes empty",
			record:          `{"parent_tool_use_id":"x"}`,
			wantDisplayName: "",
			wantDescription: "",
		},
		{
			name:            "both fields present decode faithfully",
			record:          `{"display_name":"Planner","description":"plans the work"}`,
			wantDisplayName: "Planner",
			wantDescription: "plans the work",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out LoopStarted
			if err := json.Unmarshal([]byte(tt.record), &out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if out.DisplayName != tt.wantDisplayName {
				t.Fatalf("DisplayName = %q, want %q", out.DisplayName, tt.wantDisplayName)
			}
			if out.Description != tt.wantDescription {
				t.Fatalf("Description = %q, want %q", out.Description, tt.wantDescription)
			}
		})
	}
}
