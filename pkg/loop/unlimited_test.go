package loop

import (
	"context"
	"errors"
	"testing"
)

func TestUnlimitedToolLimitsSurviveDefine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   ToolLimits
		want ToolLimits
	}{
		{"both unlimited", ToolLimits{Iterations: Unlimited, Calls: Unlimited}, ToolLimits{Iterations: Unlimited, Calls: Unlimited, Parallel: 8, CaptureBytes: DefaultToolResultCaptureBytes}},
		{"iterations unlimited", ToolLimits{Iterations: Unlimited}, ToolLimits{Iterations: Unlimited, Calls: 100, Parallel: 8, CaptureBytes: DefaultToolResultCaptureBytes}},
		{"calls unlimited", ToolLimits{Iterations: 3, Calls: Unlimited}, ToolLimits{Iterations: 3, Calls: Unlimited, Parallel: 8, CaptureBytes: DefaultToolResultCaptureBytes}},
		{"zero defaults", ToolLimits{}, ToolLimits{Iterations: 25, Calls: 100, Parallel: 8, CaptureBytes: DefaultToolResultCaptureBytes}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := mustDefinition(t, WithToolLimits(tt.in))
			b, err := d.Bind(context.Background(), validToolBindings(t))
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			if got := b.ToolLimits(); got != tt.want {
				t.Fatalf("ToolLimits = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestModeOverrideToUnlimited(t *testing.T) {
	t.Parallel()
	d := mustDefinition(t,
		WithToolLimits(ToolLimits{Iterations: 3, Calls: 7}),
		WithModes(Mode{Name: "long", ToolLimits: ToolLimits{Iterations: Unlimited, Calls: Unlimited}}, Mode{Name: "inherit"}),
		WithInitialMode("long"))
	b, err := d.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	for _, tt := range []struct {
		name ModeName
		want ToolLimits
	}{
		{"long", ToolLimits{Iterations: Unlimited, Calls: Unlimited}},
		{"inherit", ToolLimits{Iterations: 3, Calls: 7}},
	} {
		got, ok := b.Mode(tt.name)
		if !ok || got.ToolLimits.Iterations != tt.want.Iterations || got.ToolLimits.Calls != tt.want.Calls {
			t.Fatalf("Mode(%q) = %+v, %t, want %+v", tt.name, got.ToolLimits, ok, tt.want)
		}
	}
}

func TestBelowUnlimitedRefused(t *testing.T) {
	t.Parallel()
	for _, limits := range []ToolLimits{{Iterations: -2}, {Calls: -2}, {Parallel: Unlimited}} {
		_, err := Define(WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithToolLimits(limits))
		var de *DefinitionError
		if !errors.As(err, &de) || de.Kind != DefinitionInvalidToolLimits {
			t.Fatalf("Define(%+v) = %v, want invalid limits", limits, err)
		}
		_, err = Define(WithName("agent"), WithInference(&fakeLLM{}, testModel()), WithModes(Mode{Name: "m", ToolLimits: limits}), WithInitialMode("m"))
		if !errors.As(err, &de) || de.Kind != DefinitionInvalidMode {
			t.Fatalf("Define(mode %+v) = %v, want invalid mode", limits, err)
		}
	}
}
