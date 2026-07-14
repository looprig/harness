package sessionruntime

import (
	"errors"
	"reflect"
	"testing"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

func restoredHustleStarted(definition hustle.DefinitionDescriptor, runID hustle.RunID, cause identity.Cause) event.HustleStarted {
	return event.HustleStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: drainUUID(0x01)}, Cause: cause, EventVisibility: event.Internal},
		Run:    event.HustleRunDescriptor{Definition: definition, RunID: runID},
	}
}

func restoredHustleCompleted(start event.HustleStarted) event.HustleCompleted {
	return event.HustleCompleted{Header: start.Header, Run: start.Run}
}

func TestFoldRestoredHustleAuditClassifiesUnmatchedStarts(t *testing.T) {
	t.Parallel()
	alpha := testHustleDefinition(t, "alpha").Descriptor()
	beta := testHustleDefinition(t, "beta").Descriptor()
	alphaOne := restoredHustleStarted(alpha, hustle.RunID(drainUUID(0x11)), identity.Cause{})
	alphaTwo := restoredHustleStarted(alpha, hustle.RunID(drainUUID(0x12)), identity.Cause{})
	betaOne := restoredHustleStarted(beta, hustle.RunID(drainUUID(0x13)), identity.Cause{})
	completed := restoredHustleStarted(alpha, hustle.RunID(drainUUID(0x14)), identity.Cause{})

	tests := []struct {
		name   string
		events []event.Event
		want   []restoredHustleInterrupted
	}{
		{name: "empty audit is empty", want: []restoredHustleInterrupted{}},
		{
			name:   "matched terminals are not interrupted",
			events: []event.Event{completed, restoredHustleCompleted(completed)},
			want:   []restoredHustleInterrupted{},
		},
		{
			name:   "unmatched starts are counted by definition identity in canonical order",
			events: []event.Event{betaOne, alphaOne, alphaTwo},
			want: []restoredHustleInterrupted{
				{Name: "alpha", ModelSource: hustle.ModelSourceCurrentLoop, Runs: 2},
				{Name: "beta", ModelSource: hustle.ModelSourceCurrentLoop, Runs: 1},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := foldRestoredHustleAudit(tt.events)
			if err != nil {
				t.Fatalf("foldRestoredHustleAudit() error = %v", err)
			}
			if !reflect.DeepEqual(got.Interrupted, tt.want) {
				t.Errorf("Interrupted = %#v, want %#v", got.Interrupted, tt.want)
			}
		})
	}
}

func TestFoldRestoredHustleAuditFailsClosed(t *testing.T) {
	t.Parallel()
	definition := testHustleDefinition(t, "audit").Descriptor()
	otherDefinition := testHustleDefinition(t, "other").Descriptor()
	start := restoredHustleStarted(definition, hustle.RunID(drainUUID(0x21)), identity.Cause{CommandID: drainUUID(0x22)})
	terminal := restoredHustleCompleted(start)

	tests := []struct {
		name   string
		events []event.Event
		kind   restoredHustleAuditErrorKind
	}{
		{name: "duplicate start", events: []event.Event{start, start}, kind: restoredHustleAuditDuplicateStart},
		{name: "terminal without start", events: []event.Event{terminal}, kind: restoredHustleAuditTerminalWithoutStart},
		{name: "duplicate terminal", events: []event.Event{start, terminal, terminal}, kind: restoredHustleAuditTerminalWithoutStart},
		{name: "definition attribution changed", events: []event.Event{start, func() event.HustleCompleted { value := terminal; value.Run.Definition = otherDefinition; return value }()}, kind: restoredHustleAuditAttributionMismatch},
		{name: "cause attribution changed", events: []event.Event{start, func() event.HustleCompleted { value := terminal; value.Cause.CommandID = drainUUID(0x23); return value }()}, kind: restoredHustleAuditAttributionMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := foldRestoredHustleAudit(tt.events)
			var target *restoredHustleAuditError
			if !errors.As(err, &target) || target.Kind != tt.kind {
				t.Fatalf("foldRestoredHustleAudit() error = %T %v, want kind %q", err, err, tt.kind)
			}
		})
	}
}

func TestIncrementRestoredHustleCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   uint64
		want    uint64
		wantErr bool
	}{
		{name: "zero becomes one", value: 0, want: 1},
		{name: "maximum fails closed", value: ^uint64(0), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := incrementRestoredHustleCount(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("incrementRestoredHustleCount() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("incrementRestoredHustleCount() = %d, want %d", got, tt.want)
			}
		})
	}
}
