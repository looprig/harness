package event

import (
	"errors"
	"strings"
	"testing"
)

func extHeader() Header { return fullHeaderLoop() }

func extTools(names ...string) []ExternalToolIdentity {
	out := make([]ExternalToolIdentity, 0, len(names))
	for _, n := range names {
		out = append(out, ExternalToolIdentity{Name: n, SchemaDigest: sampleDigest()})
	}
	return out
}

// TestValidateLoopExternalToolsetChanged is the durable boundary: everything in this
// record is third-party supplied, so each guard is fail-closed.
//
// Mutation checks, per case:
//   - dropping the Source/Generation required checks → "missing source"/"missing
//     generation" stop erroring.
//   - dropping the length caps → "over-long *" stop erroring, letting a hostile server
//     append unbounded strings to the journal.
//   - dropping the isLowerHex check → "malformed digest"/"uppercase digest"/"truncated
//     digest" stop erroring.
//   - dropping the duplicate-name check → "duplicate tool name" stops erroring.
//   - dropping the count cap → "too many tools" stops erroring.
func TestValidateLoopExternalToolsetChanged(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		event   LoopExternalToolsetChanged
		wantErr bool
		field   FieldName
	}{
		{
			name:  "valid record",
			event: LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1", Tools: extTools("search", "fetch")},
		},
		{
			name:  "empty toolset is valid (a cleared slot)",
			event: LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1"},
		},
		{
			name:    "missing source",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Generation: "g1"},
			wantErr: true, field: FieldSource,
		},
		{
			name:    "over-long source",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: strings.Repeat("s", maxExternalSourceLen+1), Generation: "g1"},
			wantErr: true, field: FieldSource,
		},
		{
			name:    "missing generation",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp"},
			wantErr: true, field: FieldGeneration,
		},
		{
			name:    "over-long generation",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: strings.Repeat("g", maxExternalGenerationLen+1)},
			wantErr: true, field: FieldGeneration,
		},
		{
			name:    "empty tool name",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1", Tools: []ExternalToolIdentity{{Name: "", SchemaDigest: sampleDigest()}}},
			wantErr: true, field: FieldTools,
		},
		{
			name:    "over-long tool name",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1", Tools: []ExternalToolIdentity{{Name: strings.Repeat("t", maxExternalToolNameLen+1), SchemaDigest: sampleDigest()}}},
			wantErr: true, field: FieldTools,
		},
		{
			name:    "malformed digest",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1", Tools: []ExternalToolIdentity{{Name: "t", SchemaDigest: "not-a-digest"}}},
			wantErr: true, field: FieldTools,
		},
		{
			name:    "uppercase digest rejected",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1", Tools: []ExternalToolIdentity{{Name: "t", SchemaDigest: strings.ToUpper(sampleDigest())}}},
			wantErr: true, field: FieldTools,
		},
		{
			name:    "truncated digest rejected",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1", Tools: []ExternalToolIdentity{{Name: "t", SchemaDigest: sampleDigest()[:63]}}},
			wantErr: true, field: FieldTools,
		},
		{
			name:    "missing digest rejected",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1", Tools: []ExternalToolIdentity{{Name: "t"}}},
			wantErr: true, field: FieldTools,
		},
		{
			name:    "duplicate tool name",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1", Tools: extTools("dup", "dup")},
			wantErr: true, field: FieldTools,
		},
		{
			name:    "too many tools",
			event:   LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1", Tools: manyTools(maxExternalTools + 1)},
			wantErr: true, field: FieldTools,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateEvent(tt.event)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateEvent() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			var invalid *InvalidEventError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %v, want *InvalidEventError", err)
			}
			if invalid.Field != tt.field {
				t.Errorf("Field = %q, want %q", invalid.Field, tt.field)
			}
		})
	}
}

func manyTools(n int) []ExternalToolIdentity {
	out := make([]ExternalToolIdentity, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ExternalToolIdentity{Name: "t" + string(rune('a'+i%26)) + strings.Repeat("x", i/26+1), SchemaDigest: sampleDigest()})
	}
	return out
}

// TestLoopExternalToolsetChangedIsEnduringLoopScoped pins the classification the runtime
// and restore depend on: an Enduring, loop-scoped record. If it were Ephemeral it would
// never be journaled and the audit trail would silently vanish.
func TestLoopExternalToolsetChangedIsEnduringLoopScoped(t *testing.T) {
	t.Parallel()
	ev := LoopExternalToolsetChanged{Header: extHeader(), Source: "mcp", Generation: "g1"}
	if ev.Class() != Enduring {
		t.Errorf("Class() = %v, want Enduring", ev.Class())
	}
	if err := ValidateEvent(ev); err != nil {
		t.Fatalf("a fully loop-stamped record must validate: %v", err)
	}
	// Loop-scoped: a record missing its LoopID must fail closed.
	bare := LoopExternalToolsetChanged{Header: fullHeaderSession(), Source: "mcp", Generation: "g1"}
	if err := ValidateEvent(bare); err == nil {
		t.Error("a record without a LoopID must not validate (loop-scoped profile)")
	}
}
