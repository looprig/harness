package sessionruntime

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
)

// modeCfg builds a mode-bearing definition so the loop controller has real modes to switch
// between (plan/build differ by effort + instructions; swap carries a distinct model).
func modeCfg(client inference.Client) loop.Definition {
	return mustDefine(
		loop.WithName("agent"),
		loop.WithInference(client, validModel("base")),
		loop.WithSystem("base"),
		loop.WithModes(
			loop.Mode{Name: "plan", Effort: inference.EffortLow, Instructions: "plan-i"},
			loop.Mode{Name: "build", Effort: inference.EffortHigh, Instructions: "build-i"},
			loop.Mode{Name: "swap", Model: validModel("swapped")},
		),
		loop.WithInitialMode("plan"),
		loop.WithDrainTimeout(100*time.Millisecond),
	)
}

func (r *recordingSub) waitFor(t *testing.T, pred func([]event.Event) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		evs := append([]event.Event(nil), r.events...)
		r.mu.Unlock()
		if pred(evs) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("event condition not met within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLoopControllerSetModeLiveAndEvent(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), modeCfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	rec, sub := observe(t, s)
	t.Cleanup(func() { _ = sub.Close() })

	ctrl, ok := s.LoopController(s.ActiveLoopID())
	if !ok {
		t.Fatal("LoopController not found")
	}
	if ctrl.Mode() != "plan" || ctrl.Model().Name != "base" {
		t.Fatalf("initial mode/model = %q/%q, want plan/base", ctrl.Mode(), ctrl.Model().Name)
	}

	if err := ctrl.SetMode(context.Background(), "swap"); err != nil {
		t.Fatalf("SetMode(swap): %v", err)
	}
	// Handle view reflects the committed selection immediately after the ack.
	if ctrl.Mode() != "swap" || ctrl.Model().Name != "swapped" {
		t.Fatalf("post-change mode/model = %q/%q, want swap/swapped", ctrl.Mode(), ctrl.Model().Name)
	}
	rec.waitFor(t, func(evs []event.Event) bool {
		for _, e := range evs {
			if mc, ok := e.(event.LoopModeChanged); ok && mc.PreviousMode == "plan" && mc.Mode == "swap" {
				return !mc.EventHeader().EventID.IsZero() && mc.LoopID == s.ActiveLoopID()
			}
		}
		return false
	})
}

func TestLoopControllerChangeLiveAndEvent(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), modeCfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	rec, sub := observe(t, s)
	t.Cleanup(func() { _ = sub.Close() })

	ctrl, _ := s.LoopController(s.ActiveLoopID())
	if err := ctrl.Change(context.Background(), loop.ChangeModel(validModel("routed")), loop.ChangeEffort(inference.EffortMax)); err != nil {
		t.Fatalf("Change: %v", err)
	}
	if ctrl.Model().Name != "routed" || ctrl.Model().Sampling.Effort != inference.EffortMax {
		t.Fatalf("post-change model/effort = %q/%q, want routed/max", ctrl.Model().Name, ctrl.Model().Sampling.Effort)
	}
	// Mode is unchanged by a direct inference change.
	if ctrl.Mode() != "plan" {
		t.Fatalf("mode = %q, want plan (unchanged by inference change)", ctrl.Mode())
	}
	rec.waitFor(t, func(evs []event.Event) bool {
		for _, e := range evs {
			if ic, ok := e.(event.LoopInferenceChanged); ok {
				return ic.Model.Name == "routed" && ic.Effort == inference.EffortMax && !ic.EventHeader().EventID.IsZero()
			}
		}
		return false
	})
}

func TestLoopControllerRefusals(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), modeCfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	ctrl, _ := s.LoopController(s.ActiveLoopID())

	tests := []struct {
		name string
		call func() error
		kind loop.ChangeErrorKind
	}{
		{name: "invalid mode", call: func() error { return ctrl.SetMode(context.Background(), "nope") }, kind: loop.ChangeInvalidMode},
		{name: "no changes", call: func() error { return ctrl.Change(context.Background()) }, kind: loop.ChangeNoChanges},
		{name: "invalid model", call: func() error {
			return ctrl.Change(context.Background(), loop.ChangeModel(inference.Model{Name: ""}))
		}, kind: loop.ChangeInvalidModel},
		{name: "invalid effort", call: func() error {
			return ctrl.Change(context.Background(), loop.ChangeEffort(inference.Effort("turbo")))
		}, kind: loop.ChangeInvalidEffort},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			var ce *loop.ChangeError
			if !errors.As(err, &ce) || ce.Kind != tt.kind {
				t.Fatalf("err = %v, want ChangeError{%s}", err, tt.kind)
			}
		})
	}
	// A refused change leaves the live view at the initial selection.
	if ctrl.Mode() != "plan" || ctrl.Model().Name != "base" {
		t.Fatalf("after refusals mode/model = %q/%q, want unchanged plan/base", ctrl.Mode(), ctrl.Model().Name)
	}
}

func TestLoopControllerExitedLoopRejected(t *testing.T) {
	t.Parallel()
	s, err := newTestSession(context.Background(), modeCfg(&stubLLM{chunks: []content.Chunk{textChunk("ok")}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctrl, _ := s.LoopController(s.ActiveLoopID())
	// Shut the session (and its loop) down; the loop actor exits, so a change can no longer
	// be delivered — the controller's send escapes on the loop's Done.
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	err = ctrl.SetMode(context.Background(), "build")
	var ce *loop.ChangeError
	if !errors.As(err, &ce) || ce.Kind != loop.ChangeLoopExited {
		t.Fatalf("SetMode on exited loop err = %v, want ChangeLoopExited", err)
	}
}

// recordingClient records each request the seeded loop issues, then streams a fixed text
// response so the turn completes. It is the observation seam for the actor's seeded
// effective config (model + system) in the restore-vs-liveView guard.
type recordingClient struct {
	mu   sync.Mutex
	reqs []inference.Request
}

func (r *recordingClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("recordingClient.Invoke not used")
}

func (r *recordingClient) Stream(_ context.Context, req inference.Request) (*inference.StreamReader[content.Chunk], error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	done := false
	next := func() (content.Chunk, error) {
		if !done {
			done = true
			return textChunk("ok"), nil
		}
		return nil, io.EOF
	}
	return inference.NewStreamReader(next, nil), nil
}

func (r *recordingClient) first() (inference.Request, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		return inference.Request{}, false
	}
	return r.reqs[0], true
}

// nopLoopPublisher satisfies loopruntime's event publisher, dropping every event (the guard
// observes the request the seeded loop issues, not its events).
type nopLoopPublisher struct{}

func (nopLoopPublisher) PublishEvent(context.Context, event.Event) error        { return nil }
func (nopLoopPublisher) PublishEventChecked(context.Context, event.Event) error { return nil }

func inferModelWithEffort(name string, eff inference.Effort) inference.Model {
	m := validModel(name)
	m.Sampling.Effort = eff
	return m
}

// TestRestoreSeedingAgreesWithLiveView is the drift guard between the two restore-resolution
// paths: for EVERY case — including the base mode reached via "" — the loop actor's seeded
// effective config (observed via its first-turn request's model + system) must match what
// internal/sessionruntime liveViewFor reports (mode + model). This locks NewRestored's
// configForMode-based seeding and liveViewFor's exact bound.Mode resolution together, so a
// future divergence (like the C1 base-mode remap) fails a test.
func TestRestoreSeedingAgreesWithLiveView(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ri   restoredInference
	}{
		{name: "no fold resumes at initial mode", ri: restoredInference{}},
		{name: "base mode via empty name", ri: restoredInference{HasMode: true, Mode: ""}},
		{name: "named mode build", ri: restoredInference{HasMode: true, Mode: "build"}},
		{name: "swap mode distinct model", ri: restoredInference{HasMode: true, Mode: "swap"}},
		{
			name: "inference override on a mode",
			ri:   restoredInference{HasMode: true, Mode: "plan", HasInference: true, Model: inferModelWithEffort("routed", inference.EffortHigh), Effort: inference.EffortHigh},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &recordingClient{}
			sid, lid := mustSessionID(t), mustSessionID(t)
			bound := bindCfg(modeCfg(client), sid, lid)

			// The two paths under test: liveViewFor (session) and the seeded actor (loop).
			wantMode, wantModel := liveViewFor(bound, tt.ri)

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			l, err := loopruntime.NewRestored(ctx, sid, lid, loop.Provenance{}, nopLoopPublisher{}, bound, restoredStateFrom(foldResult{}, tt.ri))
			if err != nil {
				t.Fatalf("NewRestored: %v", err)
			}
			cid, _ := uuid.New()
			l.Commands <- command.UserInput{Header: command.Header{CommandID: cid}, Blocks: []content.Block{&content.TextBlock{Text: "go"}}}

			var req inference.Request
			deadline := time.Now().Add(2 * time.Second)
			for {
				if r, ok := client.first(); ok {
					req = r
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no first-turn request observed from the seeded loop")
				}
				time.Sleep(5 * time.Millisecond)
			}

			// Model + effort the seeded actor stamped on the request == liveViewFor's model.
			if req.Model.Name != wantModel.Name || req.Model.Sampling.Effort != wantModel.Sampling.Effort {
				t.Fatalf("seeded model/effort = %q/%q, liveViewFor = %q/%q",
					req.Model.Name, req.Model.Sampling.Effort, wantModel.Name, wantModel.Sampling.Effort)
			}
			// System prompt reflects liveViewFor's mode (locks mode agreement across the paths).
			bm, ok := bound.Mode(wantMode)
			if !ok {
				t.Fatalf("liveViewFor mode %q not resolvable on bound", wantMode)
			}
			if wantSystem := loop.EffectiveSystem(bound.System(), bm.Instructions); req.System != wantSystem {
				t.Fatalf("seeded system = %q, want %q (mode %q)", req.System, wantSystem, wantMode)
			}
		})
	}
}

// TestFoldLoopInferenceLastWriteWins proves the restore fold reproduces the live
// precedence: last mode wins, a later inference change overrides the mode's model/effort,
// and a mode change AFTER an inference change resets it.
func TestFoldLoopInferenceLastWriteWins(t *testing.T) {
	t.Parallel()
	modeChanged := func(prev, next string) event.Event {
		return event.LoopModeChanged{Header: event.Header{Coordinates: identity.Coordinates{LoopID: [16]byte{1}}}, PreviousMode: prev, Mode: next}
	}
	infChanged := func(name string, eff inference.Effort) event.Event {
		return event.LoopInferenceChanged{Header: event.Header{Coordinates: identity.Coordinates{LoopID: [16]byte{1}}}, Model: validModel(name), Effort: eff}
	}

	tests := []struct {
		name         string
		events       []event.Event
		wantMode     loop.ModeName
		hasMode      bool
		wantModel    string
		wantEffort   inference.Effort
		hasInference bool
	}{
		{name: "no changes", events: nil, hasMode: false, hasInference: false},
		{
			name:     "last mode wins",
			events:   []event.Event{modeChanged("", "plan"), modeChanged("plan", "build")},
			wantMode: "build", hasMode: true, hasInference: false,
		},
		{
			name:         "inference after mode overrides",
			events:       []event.Event{modeChanged("", "build"), infChanged("routed", inference.EffortHigh)},
			wantMode:     "build",
			hasMode:      true,
			wantModel:    "routed",
			wantEffort:   inference.EffortHigh,
			hasInference: true,
		},
		{
			name:         "mode after inference resets inference",
			events:       []event.Event{infChanged("routed", inference.EffortHigh), modeChanged("", "plan")},
			wantMode:     "plan",
			hasMode:      true,
			hasInference: false,
		},
		{
			name:         "inference only, no mode change",
			events:       []event.Event{infChanged("routed", inference.EffortMax)},
			hasMode:      false,
			wantModel:    "routed",
			wantEffort:   inference.EffortMax,
			hasInference: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := foldLoopInference(tt.events)
			if got.HasMode != tt.hasMode || got.Mode != tt.wantMode {
				t.Fatalf("mode = %q (has %v), want %q (has %v)", got.Mode, got.HasMode, tt.wantMode, tt.hasMode)
			}
			if got.HasInference != tt.hasInference {
				t.Fatalf("hasInference = %v, want %v", got.HasInference, tt.hasInference)
			}
			if tt.hasInference && (got.Model.Name != tt.wantModel || got.Effort != tt.wantEffort) {
				t.Fatalf("inference = %q/%q, want %q/%q", got.Model.Name, got.Effort, tt.wantModel, tt.wantEffort)
			}
		})
	}
}

// TestFoldLoopInferenceSeedsInitialMode proves restore honors a mode-selective spawn: the
// baseline mode is seeded from LoopStarted.InitialMode (the spawn records the selected
// mode there and emits NO LoopModeChanged), an empty InitialMode means the definition
// default, and a later LoopModeChanged overrides the baseline. Composed with
// TestRestoreSeedingAgreesWithLiveView (which proves a restoredInference resolves to the
// right model/effort/system), this proves a child spawned in a named non-default mode
// restores under that mode's config.
func TestFoldLoopInferenceSeedsInitialMode(t *testing.T) {
	t.Parallel()
	started := func(mode string) event.Event {
		return event.LoopStarted{Header: event.Header{Coordinates: identity.Coordinates{LoopID: [16]byte{1}}}, InitialMode: mode}
	}
	modeChanged := func(prev, next string) event.Event {
		return event.LoopModeChanged{Header: event.Header{Coordinates: identity.Coordinates{LoopID: [16]byte{1}}}, PreviousMode: prev, Mode: next}
	}
	infChanged := func(name string, eff inference.Effort) event.Event {
		return event.LoopInferenceChanged{Header: event.Header{Coordinates: identity.Coordinates{LoopID: [16]byte{1}}}, Model: validModel(name), Effort: eff}
	}
	tests := []struct {
		name         string
		events       []event.Event
		wantMode     loop.ModeName
		hasMode      bool
		hasInference bool
	}{
		{name: "selected initial mode seeds baseline", events: []event.Event{started("review")}, wantMode: "review", hasMode: true},
		{name: "empty initial mode is definition default", events: []event.Event{started("")}, hasMode: false},
		{name: "later mode change overrides start mode", events: []event.Event{started("review"), modeChanged("review", "build")}, wantMode: "build", hasMode: true},
		{name: "inference override keeps start mode", events: []event.Event{started("review"), infChanged("routed", inference.EffortHigh)}, wantMode: "review", hasMode: true, hasInference: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := foldLoopInference(tt.events)
			if got.HasMode != tt.hasMode || got.Mode != tt.wantMode {
				t.Fatalf("mode = %q (has %v), want %q (has %v)", got.Mode, got.HasMode, tt.wantMode, tt.hasMode)
			}
			if got.HasInference != tt.hasInference {
				t.Fatalf("hasInference = %v, want %v", got.HasInference, tt.hasInference)
			}
		})
	}
}
