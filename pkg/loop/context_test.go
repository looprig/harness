package loop

import (
	"errors"
	"math"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	model "github.com/looprig/inference/model"
)

func TestResolveContextLimits(t *testing.T) {
	t.Parallel()
	modelKey := model.ModelKey{Provider: "provider", Model: "model"}
	tests := []struct {
		name        string
		limits      model.ContextLimits
		reserved    content.TokenCount
		margin      content.TokenCount
		want        ResolvedContextLimits
		wantUnknown bool
	}{
		{name: "window only", limits: model.ContextLimits{WindowTokens: 100}, reserved: 20, margin: 5, want: ResolvedContextLimits{ReservedOutput: 20, RawInputLimit: 80, InputLimit: 75}},
		{name: "reserved clamped to max output", limits: model.ContextLimits{WindowTokens: 100, MaxOutputTokens: 10}, reserved: 20, margin: 5, want: ResolvedContextLimits{ReservedOutput: 10, RawInputLimit: 90, InputLimit: 85}},
		{name: "minimum nonzero input cap", limits: model.ContextLimits{WindowTokens: 100, MaxInputTokens: 60}, reserved: 20, margin: 5, want: ResolvedContextLimits{ReservedOutput: 20, RawInputLimit: 60, InputLimit: 55}},
		{name: "unknown window known input subtracts margin last", limits: model.ContextLimits{MaxInputTokens: 60}, reserved: 20, margin: 5, want: ResolvedContextLimits{ReservedOutput: 20, RawInputLimit: 60, InputLimit: 55}},
		{name: "unknown output preserves reservation", limits: model.ContextLimits{WindowTokens: 100}, reserved: 20, want: ResolvedContextLimits{ReservedOutput: 20, RawInputLimit: 80, InputLimit: 80}},
		{name: "unknown denominator", limits: model.ContextLimits{}, reserved: 20, wantUnknown: true},
		{name: "reservation consumes window", limits: model.ContextLimits{WindowTokens: 20}, reserved: 20, wantUnknown: true},
		{name: "margin consumes raw limit", limits: model.ContextLimits{MaxInputTokens: 5}, reserved: 1, margin: 5, wantUnknown: true},
		{name: "reservation exceeds unknown max and known window", limits: model.ContextLimits{WindowTokens: 5}, reserved: 6, wantUnknown: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveContextLimits(modelKey, tt.limits, tt.reserved, tt.margin)
			if tt.wantUnknown {
				var target *ContextLimitUnknownError
				if !errors.As(err, &target) || target.Model != modelKey {
					t.Fatalf("error = %T %v, want ContextLimitUnknownError for %v", err, err, modelKey)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ResolveContextLimits() = %#v, %v; want %#v", got, err, tt.want)
			}
		})
	}
}

func TestOccupancyBasisPoints(t *testing.T) {
	t.Parallel()
	maximum := content.TokenCount(math.MaxUint64)
	tests := []struct {
		name    string
		used    content.TokenCount
		limit   content.TokenCount
		want    event.BasisPoints
		wantErr bool
	}{
		{name: "zero used", used: 0, limit: 100, want: 0},
		{name: "one basis point", used: 1, limit: 10_000, want: 1},
		{name: "fraction floors", used: 1, limit: 3, want: 3_333},
		{name: "exact full scale", used: 100, limit: 100, want: event.FullScaleBasisPoints},
		{name: "over limit clamps", used: 101, limit: 100, want: event.FullScaleBasisPoints},
		{name: "maximum avoids overflow", used: maximum - 1, limit: maximum, want: 9_999},
		{name: "zero denominator", used: 0, limit: 0, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := OccupancyBasisPoints(tt.used, tt.limit)
			if (err != nil) != tt.wantErr || (!tt.wantErr && got != tt.want) {
				t.Fatalf("OccupancyBasisPoints() = %d, %v; want %d, wantErr %v", got, err, tt.want, tt.wantErr)
			}
			if tt.wantErr {
				var target *OccupancyError
				if !errors.As(err, &target) {
					t.Fatalf("error = %T, want *OccupancyError", err)
				}
			}
		})
	}
}
