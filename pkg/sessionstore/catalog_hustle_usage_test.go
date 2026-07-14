package sessionstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
	"github.com/looprig/storage/memstore"
)

func catalogHustleDescriptor(t *testing.T, name hustle.Name, source hustle.ModelSource, key inference.ModelKey) hustle.DefinitionDescriptor {
	t.Helper()
	descriptor := replayHustleDefinition(t)
	descriptor.Name = name
	descriptor.ModelSource = source
	if source == hustle.ModelSourceNamed {
		descriptor.NamedModelKey = key
		descriptor.NamedModelPolicyRevision = "model-policy-v1"
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("DefinitionDescriptor.Validate() error = %v", err)
	}
	return descriptor
}

func catalogHustlePair(sid uuid.UUID, descriptor hustle.DefinitionDescriptor, runSeed byte, runtime event.ModelRuntime, usage *content.Usage, failed bool) (event.HustleStarted, event.Event) {
	runID := hustle.RunID(fixedUUID(runSeed))
	header := event.Header{Coordinates: identity.Coordinates{SessionID: sid}, Cause: identity.Cause{CommandID: fixedUUID(runSeed + 1)}, EventVisibility: event.Internal}
	started := event.HustleStarted{Header: header, Run: event.HustleRunDescriptor{Definition: descriptor, RunID: runID}}
	run := started.Run
	run.Runtime = runtime
	if failed {
		return started, event.HustleFailed{Header: header, Run: run, Stage: hustle.StageInference, ReasonCode: hustle.ReasonInference, Usage: usage}
	}
	return started, event.HustleCompleted{Header: header, Run: run, Usage: usage}
}

func TestFoldCatalogHustlesIsBoundedAndDeterministic(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x70)
	current := catalogHustleDescriptor(t, "z-current", hustle.ModelSourceCurrentLoop, inference.ModelKey{})
	namedKey := inference.ModelKey{Provider: "provider", Model: "fixed"}
	named := catalogHustleDescriptor(t, "a-named", hustle.ModelSourceNamed, namedKey)
	runtimeA := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider-a", Model: "model-a"}, Limits: inference.ContextLimits{WindowTokens: 100}}
	runtimeB := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider-b", Model: "model-b"}, Limits: inference.ContextLimits{WindowTokens: 200}}
	namedRuntime := event.ModelRuntime{Key: namedKey, Limits: inference.ContextLimits{WindowTokens: 300}}
	usageA := content.Usage{InputTokens: 10, OutputTokens: 2}
	usageB := content.Usage{InputTokens: 20, OutputTokens: 4, CacheReadTokens: 3}
	currentStartA, currentDoneA := catalogHustlePair(sid, current, 0x71, runtimeA, &usageA, false)
	currentStartB, currentDoneB := catalogHustlePair(sid, current, 0x73, runtimeB, &usageB, false)
	currentStartFailed, currentFailed := catalogHustlePair(sid, current, 0x75, runtimeA, &usageA, true)
	namedStart, namedDone := catalogHustlePair(sid, named, 0x77, namedRuntime, nil, false)
	namedFailedStart, _ := catalogHustlePair(sid, named, 0x79, namedRuntime, nil, false)
	namedFailed := event.HustleFailed{Header: namedFailedStart.Header, Run: namedFailedStart.Run, Stage: hustle.StageQueue, ReasonCode: hustle.ReasonCanceled}
	interrupted, _ := catalogHustlePair(sid, current, 0x7b, runtimeA, &usageA, false)

	want := []HustleUsageAggregate{
		{Name: "a-named", ModelSource: hustle.ModelSourceNamed, NamedModelKey: namedKey, Status: hustle.TerminalStatusCompleted, Runs: 1},
		{Name: "a-named", ModelSource: hustle.ModelSourceNamed, NamedModelKey: namedKey, Status: hustle.TerminalStatusFailed, Runs: 1},
		{Name: "z-current", ModelSource: hustle.ModelSourceCurrentLoop, Status: hustle.TerminalStatusCompleted, Runs: 2, CumulativeUsage: content.Usage{InputTokens: 30, OutputTokens: 6, CacheReadTokens: 3}},
		{Name: "z-current", ModelSource: hustle.ModelSourceCurrentLoop, Status: hustle.TerminalStatusFailed, Runs: 1, CumulativeUsage: usageA},
	}
	tests := []struct {
		name   string
		events []event.Event
	}{
		{name: "one order", events: []event.Event{currentStartA, currentDoneA, namedStart, namedDone, namedFailedStart, namedFailed, currentStartFailed, currentFailed, currentStartB, currentDoneB, interrupted}},
		{name: "different run order", events: []event.Event{interrupted, currentStartB, currentDoneB, currentStartFailed, currentFailed, namedFailedStart, namedFailed, namedStart, namedDone, currentStartA, currentDoneA}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := foldCatalogHustles(tt.events)
			if err != nil {
				t.Fatalf("foldCatalogHustles() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("foldCatalogHustles() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestFoldCatalogHustlesFailsClosed(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x80)
	namedKey := inference.ModelKey{Provider: "provider", Model: "fixed"}
	descriptor := catalogHustleDescriptor(t, "named", hustle.ModelSourceNamed, namedKey)
	runtime := event.ModelRuntime{Key: namedKey, Limits: inference.ContextLimits{WindowTokens: 100}}
	usageMax := content.Usage{InputTokens: content.TokenCount(^uint64(0))}
	usageOne := content.Usage{InputTokens: 1}
	start, terminal := catalogHustlePair(sid, descriptor, 0x81, runtime, nil, false)
	startMax, terminalMax := catalogHustlePair(sid, descriptor, 0x83, runtime, &usageMax, false)
	startOne, terminalOne := catalogHustlePair(sid, descriptor, 0x85, runtime, &usageOne, false)

	tests := []struct {
		name   string
		events []event.Event
	}{
		{name: "terminal without start", events: []event.Event{terminal}},
		{name: "duplicate start", events: []event.Event{start, start}},
		{name: "duplicate terminal", events: []event.Event{start, terminal, terminal}},
		{name: "definition mismatch", events: []event.Event{start, func() event.Event {
			value := terminal.(event.HustleCompleted)
			value.Run.Definition.Name = "other"
			return value
		}()}},
		{name: "cause mismatch", events: []event.Event{start, func() event.Event {
			value := terminal.(event.HustleCompleted)
			value.Cause.CommandID = fixedUUID(0x89)
			return value
		}()}},
		{name: "named runtime mismatch", events: []event.Event{start, func() event.Event {
			value := terminal.(event.HustleCompleted)
			value.Run.Runtime.Key.Model = "other"
			return value
		}()}},
		{name: "usage overflow", events: []event.Event{startMax, terminalMax, startOne, terminalOne}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := foldCatalogHustles(tt.events)
			var target *CatalogHustleError
			if !errors.As(err, &target) {
				t.Fatalf("foldCatalogHustles() error = %T %v, want *CatalogHustleError", err, err)
			}
		})
	}
}

func TestFoldCatalogHustleTerminalRejectsRunCountOverflow(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0xa0)
	descriptor := catalogHustleDescriptor(t, "overflow", hustle.ModelSourceCurrentLoop, inference.ModelKey{})
	runtime := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}}
	start, terminalEvent := catalogHustlePair(sid, descriptor, 0xa1, runtime, nil, false)
	terminal := terminalEvent.(event.HustleCompleted)
	key := catalogHustleKey{name: descriptor.Name, modelSource: descriptor.ModelSource, status: hustle.TerminalStatusCompleted}
	starts := map[hustle.RunID]catalogHustleStart{start.Run.RunID: {descriptor: descriptor, sessionID: sid, cause: start.Cause}}
	aggregates := map[catalogHustleKey]HustleUsageAggregate{key: {Name: descriptor.Name, ModelSource: descriptor.ModelSource, Status: hustle.TerminalStatusCompleted, Runs: ^uint64(0)}}

	tests := []struct {
		name string
	}{
		{name: "maximum run count fails closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := foldCatalogHustleTerminal(starts, aggregates, terminal.Run, terminal.SessionID, terminal.Cause, terminal.Usage, hustle.TerminalStatusCompleted, nil)
			var target *CatalogHustleError
			if !errors.As(err, &target) || target.Kind != CatalogHustleRunCountOverflow {
				t.Fatalf("foldCatalogHustleTerminal() error = %T %v, want run-count overflow", err, err)
			}
		})
	}
}

func TestHustleUsageAggregateCodecValidation(t *testing.T) {
	t.Parallel()
	key := inference.ModelKey{Provider: "provider", Model: "model"}
	valid := HustleUsageAggregate{Name: "named", ModelSource: hustle.ModelSourceNamed, NamedModelKey: key, Status: hustle.TerminalStatusCompleted, Runs: 1, CumulativeUsage: content.Usage{InputTokens: 2, OutputTokens: 1}}
	tests := []struct {
		name    string
		meta    SessionMeta
		wantErr bool
	}{
		{name: "absent field is backward compatible", meta: SessionMeta{}},
		{name: "valid aggregate", meta: SessionMeta{Hustles: []HustleUsageAggregate{valid}}},
		{name: "unknown source", meta: SessionMeta{Hustles: []HustleUsageAggregate{func() HustleUsageAggregate {
			value := valid
			value.ModelSource = hustle.ModelSourceUnknown
			return value
		}()}}, wantErr: true},
		{name: "unknown status", meta: SessionMeta{Hustles: []HustleUsageAggregate{func() HustleUsageAggregate { value := valid; value.Status = hustle.TerminalStatusUnknown; return value }()}}, wantErr: true},
		{name: "zero runs", meta: SessionMeta{Hustles: []HustleUsageAggregate{func() HustleUsageAggregate { value := valid; value.Runs = 0; return value }()}}, wantErr: true},
		{name: "current source carries named key", meta: SessionMeta{Hustles: []HustleUsageAggregate{func() HustleUsageAggregate {
			value := valid
			value.ModelSource = hustle.ModelSourceCurrentLoop
			return value
		}()}}, wantErr: true},
		{name: "named source missing key", meta: SessionMeta{Hustles: []HustleUsageAggregate{func() HustleUsageAggregate { value := valid; value.NamedModelKey = inference.ModelKey{}; return value }()}}, wantErr: true},
		{name: "invalid usage", meta: SessionMeta{Hustles: []HustleUsageAggregate{func() HustleUsageAggregate {
			value := valid
			value.CumulativeUsage = content.Usage{OutputTokens: 1, ReasoningTokens: 2}
			return value
		}()}}, wantErr: true},
		{name: "unsorted", meta: SessionMeta{Hustles: []HustleUsageAggregate{valid, func() HustleUsageAggregate { value := valid; value.Name = "alpha"; return value }()}}, wantErr: true},
		{name: "duplicate", meta: SessionMeta{Hustles: []HustleUsageAggregate{valid, valid}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			encoded, encodeErr := encodeSessionMeta(tt.meta)
			if (encodeErr != nil) != tt.wantErr {
				t.Fatalf("encodeSessionMeta() error = %v, wantErr %v", encodeErr, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			decoded, err := decodeSessionMeta(encoded)
			if err != nil {
				t.Fatalf("decodeSessionMeta() error = %v", err)
			}
			if !reflect.DeepEqual(decoded.Hustles, tt.meta.Hustles) {
				t.Errorf("Hustles = %#v, want %#v", decoded.Hustles, tt.meta.Hustles)
			}
		})
	}
}

func TestDecodeSessionMetaRejectsMalformedHustleJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
	}{
		{name: "unknown aggregate field", data: `{"hustles":[{"name":"x","model_source":1,"status":1,"runs":1,"surprise":true}]}`},
		{name: "duplicate aggregate field", data: `{"hustles":[{"name":"x","Name":"y","model_source":1,"status":1,"runs":1}]}`},
		{name: "unknown model source", data: `{"hustles":[{"name":"x","model_source":99,"status":1,"runs":1}]}`},
		{name: "unknown terminal status", data: `{"hustles":[{"name":"x","model_source":1,"status":99,"runs":1}]}`},
		{name: "unsorted aggregates", data: `{"hustles":[{"name":"z","model_source":1,"status":1,"runs":1},{"name":"a","model_source":1,"status":1,"runs":1}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeSessionMeta([]byte(tt.data)); err == nil {
				t.Fatal("decodeSessionMeta() error = nil, want malformed hustle rejection")
			}
		})
	}
}

func TestCatalogLiveAndRepairShareHustleAggregate(t *testing.T) {
	t.Parallel()
	sid := fixedUUID(0x90)
	descriptor := catalogHustleDescriptor(t, "current", hustle.ModelSourceCurrentLoop, inference.ModelKey{})
	runtime := event.ModelRuntime{Key: inference.ModelKey{Provider: "provider", Model: "model"}, Limits: inference.ContextLimits{WindowTokens: 100}}
	usage := content.Usage{InputTokens: 8, OutputTokens: 2}
	started, terminal := catalogHustlePair(sid, descriptor, 0x91, runtime, &usage, false)
	events := []event.Event{event.SessionStarted{Header: hdr(sid)}, started, terminal}

	tests := []struct {
		name string
		act  func(*Catalog) (SessionMeta, error)
	}{
		{name: "repair", act: func(c *Catalog) (SessionMeta, error) { return c.RepairCatalog(context.Background(), sid) }},
		{name: "live terminal update repairs privileged lifecycle", act: func(c *Catalog) (SessionMeta, error) {
			if err := c.UpdateOnEvent(context.Background(), terminal, 3); err != nil {
				return SessionMeta{}, err
			}
			meta, _, err := c.ReadMeta(context.Background(), sid)
			return meta, err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, err := Open(memstore.New())
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			catalog := store.OpenCatalog(WithCatalogReplayer(&fakeOpener{events: events}), WithCatalogClock(fixedClock(time.Time{})))
			meta, err := tt.act(catalog)
			if err != nil {
				t.Fatalf("act() error = %v", err)
			}
			want := []HustleUsageAggregate{{Name: "current", ModelSource: hustle.ModelSourceCurrentLoop, Status: hustle.TerminalStatusCompleted, Runs: 1, CumulativeUsage: usage}}
			if !reflect.DeepEqual(meta.Hustles, want) {
				t.Errorf("Hustles = %#v, want %#v", meta.Hustles, want)
			}
		})
	}
}

func FuzzDecodeSessionMetaHustles(f *testing.F) {
	valid := SessionMeta{Hustles: []HustleUsageAggregate{{Name: "current", ModelSource: hustle.ModelSourceCurrentLoop, Status: hustle.TerminalStatusCompleted, Runs: 1}}}
	encoded, err := encodeSessionMeta(valid)
	if err != nil {
		f.Fatalf("encodeSessionMeta() error = %v", err)
	}
	f.Add(encoded)
	f.Add([]byte(`{"hustles":[{"name":"x","model_source":99,"status":1,"runs":1}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		meta, err := decodeSessionMeta(data)
		if err != nil {
			return
		}
		if err := validateSessionMeta(meta); err != nil {
			t.Fatalf("decodeSessionMeta accepted invalid meta: %v", err)
		}
	})
}
