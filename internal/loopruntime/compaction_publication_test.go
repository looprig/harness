package loopruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

func TestPublishCompactionStartedBeforeInference(t *testing.T) {
	t.Parallel()

	publishErr := errors.New("checked publication failed")
	mintErr := errors.New("event id mint failed")
	inferenceErr := errors.New("inference failed")
	createdAt := time.Date(2026, 7, 13, 12, 0, 0, 123, time.UTC)
	eventID := uuid.UUID{0x91}
	sessionID := uuid.UUID{0x92}
	loopID := uuid.UUID{0x93}
	throughID := uuid.UUID{0x94}
	attemptID := event.CompactAttemptID(uuid.UUID{0x95})

	tests := []struct {
		name           string
		started        event.CompactionStarted
		mintErr        error
		publishErr     error
		inferenceErr   error
		wantErr        error
		wantPublished  int
		wantInferences int
	}{
		{
			name: "valid start is stamped checked published before inference",
			started: event.CompactionStarted{
				AttemptID: attemptID,
				Reason:    event.CompactionReasonManual,
				Basis:     event.ContextBasis{Revision: 2, ThroughEventID: throughID},
			},
			wantPublished:  1,
			wantInferences: 1,
		},
		{
			name: "invalid start prevents publication and inference",
			started: event.CompactionStarted{
				AttemptID: attemptID,
				Basis:     event.ContextBasis{Revision: 2, ThroughEventID: throughID},
			},
			wantInferences: 0,
		},
		{
			name: "event id mint failure prevents publication and inference",
			started: event.CompactionStarted{
				AttemptID: attemptID,
				Reason:    event.CompactionReasonAutomatic,
				Basis:     event.ContextBasis{Revision: 2, ThroughEventID: throughID},
			},
			mintErr:        mintErr,
			wantErr:        mintErr,
			wantInferences: 0,
		},
		{
			name: "checked publication failure prevents inference",
			started: event.CompactionStarted{
				AttemptID: attemptID,
				Reason:    event.CompactionReasonManual,
				Basis:     event.ContextBasis{Revision: 2, ThroughEventID: throughID},
			},
			publishErr:     publishErr,
			wantErr:        publishErr,
			wantInferences: 0,
		},
		{
			name: "inference failure is returned after successful publication",
			started: event.CompactionStarted{
				AttemptID: attemptID,
				Reason:    event.CompactionReasonManual,
				Basis:     event.ContextBasis{Revision: 2, ThroughEventID: throughID},
			},
			inferenceErr:   inferenceErr,
			wantErr:        inferenceErr,
			wantPublished:  1,
			wantInferences: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			publisher := &recordingPublisher{checkedErr: tt.publishErr}
			factory := event.NewFactory(func() (uuid.UUID, error) {
				if tt.mintErr != nil {
					return uuid.UUID{}, tt.mintErr
				}
				return eventID, nil
			}, func() time.Time { return createdAt })
			inferences := 0
			err := publishCompactionStartedBeforeInference(
				context.Background(), publisher, factory, sessionID, loopID, tt.started,
				func(context.Context) error {
					inferences++
					return tt.inferenceErr
				},
			)
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && tt.name != "invalid start prevents publication and inference" && err != nil {
				t.Errorf("error = %v, want nil", err)
			}
			if tt.name == "invalid start prevents publication and inference" {
				var invalid *event.InvalidEventError
				if !errors.As(err, &invalid) {
					t.Errorf("error = %T %v, want *event.InvalidEventError", err, err)
				}
			}
			gotEvents := publisher.events()
			if len(gotEvents) != tt.wantPublished {
				t.Fatalf("published %d events, want %d", len(gotEvents), tt.wantPublished)
			}
			if inferences != tt.wantInferences {
				t.Errorf("inference calls = %d, want %d", inferences, tt.wantInferences)
			}
			if len(gotEvents) == 1 {
				got := gotEvents[0].(event.CompactionStarted)
				if got.EventID != eventID || !got.CreatedAt.Equal(createdAt) {
					t.Errorf("stamp = %v/%v, want %v/%v", got.EventID, got.CreatedAt, eventID, createdAt)
				}
				if got.SessionID != sessionID || got.LoopID != loopID || !got.TurnID.IsZero() {
					t.Errorf("coordinates = %+v, want loop-scoped %v/%v", got.Coordinates, sessionID, loopID)
				}
			}
		})
	}
}
