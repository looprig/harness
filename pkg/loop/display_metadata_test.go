package loop

import (
	"context"
	"errors"
	"testing"
)

// TestWithDisplayNameAndDescription covers the happy set, empty defaults, and the
// bound-accessor read-through for the presentation-only display metadata options.
func TestWithDisplayNameAndDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		opts            []Option
		wantDisplayName string
		wantDescription string
	}{
		{
			name:            "both set",
			opts:            []Option{WithDisplayName("Planner"), WithDescription("plans the work")},
			wantDisplayName: "Planner",
			wantDescription: "plans the work",
		},
		{
			name:            "display name only",
			opts:            []Option{WithDisplayName("Planner")},
			wantDisplayName: "Planner",
			wantDescription: "",
		},
		{
			name:            "description only",
			opts:            []Option{WithDescription("plans the work")},
			wantDisplayName: "",
			wantDescription: "plans the work",
		},
		{
			name:            "neither set defaults empty",
			opts:            nil,
			wantDisplayName: "",
			wantDescription: "",
		},
		{
			name:            "explicit empty strings",
			opts:            []Option{WithDisplayName(""), WithDescription("")},
			wantDisplayName: "",
			wantDescription: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			def := mustDefinition(t, tt.opts...)
			bound, err := def.Bind(context.Background(), validToolBindings(t))
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			if got := bound.DisplayName(); got != tt.wantDisplayName {
				t.Errorf("DisplayName() = %q, want %q", got, tt.wantDisplayName)
			}
			if got := bound.Description(); got != tt.wantDescription {
				t.Errorf("Description() = %q, want %q", got, tt.wantDescription)
			}
		})
	}
}

// TestWithDisplayMetadataSingleton proves each option is a singleton: setting it
// twice yields a DefinitionDuplicateOption error.
func TestWithDisplayMetadataSingleton(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
	}{
		{name: "display name twice", opts: []Option{WithDisplayName("a"), WithDisplayName("b")}},
		{name: "description twice", opts: []Option{WithDescription("a"), WithDescription("b")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := []Option{WithName("agent"), WithInference(&fakeLLM{}, testModel())}
			_, err := Define(append(base, tt.opts...)...)
			var defErr *DefinitionError
			if !errors.As(err, &defErr) || defErr.Kind != DefinitionDuplicateOption {
				t.Fatalf("Define error = %v, want DefinitionDuplicateOption", err)
			}
		})
	}
}

// TestDisplayMetadataDoesNotAffectPolicyRevision is the critical restore-compat
// invariant: two definitions differing ONLY in display name/description must
// produce the SAME PolicyRevision fingerprint.
func TestDisplayMetadataDoesNotAffectPolicyRevision(t *testing.T) {
	t.Parallel()
	plain := mustDefinition(t)
	labeled := mustDefinition(t, WithDisplayName("Planner"), WithDescription("plans the work"))
	relabeled := mustDefinition(t, WithDisplayName("Reviewer"), WithDescription("reviews the work"))

	if plain.PolicyRevision() != labeled.PolicyRevision() {
		t.Error("PolicyRevision() changed when display metadata was added; must be presentation-only")
	}
	if labeled.PolicyRevision() != relabeled.PolicyRevision() {
		t.Error("PolicyRevision() changed when only display metadata differed; must be presentation-only")
	}
}
