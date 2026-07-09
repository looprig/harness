package serve

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/event"
)

// TestStatusEventMarshalJSON pins the custom serialization of a StatusEvent: the
// codec-safe {journal_seq, event} shape where "event" is the durable envelope
// (event.MarshalEvent output), a nil Event omits the "event" key, and an
// Ephemeral/unknown event propagates the marshal failure instead of panicking or
// silently emitting an empty record.
func TestStatusEventMarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		se          StatusEvent
		wantErr     bool
		wantSeq     uint64
		wantEventOK bool   // whether the "event" key should be present
		wantType    string // expected codec "type" tag when event present
	}{
		{
			name:        "nil event omits event key",
			se:          StatusEvent{JournalSeq: 5},
			wantSeq:     5,
			wantEventOK: false,
		},
		{
			name:        "enduring event carries codec type tag",
			se:          StatusEvent{JournalSeq: 9, Event: event.TurnDone{}},
			wantSeq:     9,
			wantEventOK: true,
			wantType:    "TurnDone",
		},
		{
			name:    "ephemeral event propagates marshal error",
			se:      StatusEvent{JournalSeq: 3, Event: event.TokenDelta{}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw, err := json.Marshal(tt.se)
			if (err != nil) != tt.wantErr {
				t.Fatalf("json.Marshal(StatusEvent) err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			var probe struct {
				JournalSeq uint64          `json:"journal_seq"`
				Event      json.RawMessage `json:"event"`
			}
			if derr := json.Unmarshal(raw, &probe); derr != nil {
				t.Fatalf("decode marshaled StatusEvent: %v (raw %s)", derr, raw)
			}
			if probe.JournalSeq != tt.wantSeq {
				t.Errorf("journal_seq = %d, want %d", probe.JournalSeq, tt.wantSeq)
			}

			hasEventKey := strings.Contains(string(raw), `"event"`)
			if hasEventKey != tt.wantEventOK {
				t.Fatalf("event key present = %v, want %v (raw %s)", hasEventKey, tt.wantEventOK, raw)
			}
			if !tt.wantEventOK {
				return
			}

			// The "event" value must be the codec envelope carrying the type tag —
			// i.e. exactly what event.MarshalEvent produces, not a struct dump.
			var typeProbe struct {
				Type string `json:"type"`
			}
			if derr := json.Unmarshal(probe.Event, &typeProbe); derr != nil {
				t.Fatalf("decode event envelope: %v (raw %s)", derr, probe.Event)
			}
			if typeProbe.Type != tt.wantType {
				t.Errorf("event type tag = %q, want %q", typeProbe.Type, tt.wantType)
			}
			want, merr := event.MarshalEvent(tt.se.Event)
			if merr != nil {
				t.Fatalf("event.MarshalEvent baseline err = %v", merr)
			}
			if string(probe.Event) != string(want) {
				t.Errorf("event bytes = %s, want %s (must equal event.MarshalEvent output)", probe.Event, want)
			}
		})
	}
}
