package sessionruntime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

func restoreRuntimeCatalog(t *testing.T) loop.RuntimeCatalog {
	t.Helper()
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex",
		Credential: loop.CredentialGatewayBacked, Default: true, DefaultModel: "luna", SmallModel: "small",
		Models: []loop.RuntimeModelOption{
			{Alias: "luna", Target: model.Model{Provider: "provider", Name: "luna-target"}, DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortHigh}},
			{Alias: "small", Target: model.Model{Provider: "provider", Name: "small-target"}, DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow}},
		},
	}})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog: %v", err)
	}
	return catalog
}

func restoreRuntimeStarted(key model.ModelKey) event.LoopStarted {
	return event.LoopStarted{
		Header:       event.Header{AgentName: "worker", Coordinates: identity.Coordinates{LoopID: mustUUID()}},
		Runtime:      event.ModelRuntime{Key: key, Effort: model.EffortHigh},
		AgentRuntime: &event.AgentRuntime{Harness: "codex", Profile: "acp/codex", CredentialMode: "gateway-backed", ModelAlias: "luna", SmallModelAlias: "small"},
	}
}

func TestRestoreRuntimeBindingNativeLegacyUnchanged(t *testing.T) {
	t.Parallel()
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), mustUUID(), mustUUID())
	got, err := restoreRuntimeBinding(event.LoopStarted{}, bound, restoredInference{}, loop.RuntimeCatalog{}, false, false)
	if err != nil {
		t.Fatalf("restoreRuntimeBinding: %v", err)
	}
	if got.Engine() != loop.EngineNative {
		t.Fatalf("engine = %v, want native", got.Engine())
	}
}

func TestRestoreRuntimeBindingAdapterResolvesAndOverridesNativeDefinition(t *testing.T) {
	t.Parallel()
	catalog := restoreRuntimeCatalog(t)
	started := restoreRuntimeStarted(model.ModelKey{Provider: "provider", Model: "luna-target"})
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), mustUUID(), started.LoopID)
	ri := foldLoopInference([]event.Event{started})
	got, err := restoreRuntimeBinding(started, bound, ri, catalog, true, false)
	if err != nil {
		t.Fatalf("restoreRuntimeBinding: %v", err)
	}
	if got.Engine() != loop.EngineAdapter || got.RuntimeProfile() != "acp/codex" || got.Model().Key() != started.Runtime.Key {
		t.Fatalf("bound runtime = engine %v profile %q key %#v", got.Engine(), got.RuntimeProfile(), got.Model().Key())
	}
}

func TestRestoreRuntimeBindingRejectsDriftAndUnavailableCatalog(t *testing.T) {
	t.Parallel()
	base := restoreRuntimeStarted(model.ModelKey{Provider: "provider", Model: "different"})
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), mustUUID(), base.LoopID)
	for name, tt := range map[string]struct {
		catalog    loop.RuntimeCatalog
		hasCatalog bool
		want       RestoreRuntimeMismatchKind
	}{
		"target mismatch":            {catalog: restoreRuntimeCatalog(t), hasCatalog: true, want: RestoreRuntimeTargetMismatch},
		"missing harness or profile": {hasCatalog: true, want: RestoreRuntimeUnavailable},
		"missing catalog":            {hasCatalog: false, want: RestoreRuntimeUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := restoreRuntimeBinding(base, bound, foldLoopInference([]event.Event{base}), tt.catalog, tt.hasCatalog, false)
			var mismatch *RestoreRuntimeMismatchError
			if !errors.As(err, &mismatch) || mismatch.Kind != tt.want {
				t.Fatalf("err = %T %v, want runtime mismatch %q", err, err, tt.want)
			}
			if strings.Contains(err.Error(), "different") || strings.Contains(err.Error(), "luna") {
				t.Fatalf("runtime mismatch leaked selector values: %q", err)
			}
		})
	}
}

func TestRestoreRuntimeBindingNonNativeWithoutAgentRuntimeFailsClosed(t *testing.T) {
	t.Parallel()
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineForeignCodex, "system"), mustUUID(), mustUUID())
	_, err := restoreRuntimeBinding(event.LoopStarted{}, bound, restoredInference{}, loop.RuntimeCatalog{}, false, false)
	var mismatch *RestoreRuntimeMismatchError
	if !errors.As(err, &mismatch) || mismatch.Kind != RestoreRuntimeMissing {
		t.Fatalf("err = %T %v, want missing runtime mismatch", err, err)
	}
}

func TestRestoredAdapterUsesRegistryAndFoldedAgentSessionID(t *testing.T) {
	t.Parallel()
	catalog := restoreRuntimeCatalog(t)
	started := restoreRuntimeStarted(model.ModelKey{Provider: "provider", Model: "luna-target"})
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), mustUUID(), started.LoopID)
	bound, err := restoreRuntimeBinding(started, bound, foldLoopInference([]event.Event{started}), catalog, true, false)
	if err != nil {
		t.Fatalf("restoreRuntimeBinding: %v", err)
	}
	builder := &fakeForeignBuilder{backend: newFakeBackend()}
	var registry foreign.BuilderRegistry
	if err := registry.Register("acp/codex", builder.build, builder.buildRestored); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}
	s := &Session{sessionCtx: context.Background(), sessionID: mustUUID(), newID: uuid.New, factory: event.NewFactory(uuid.New, time.Now), loops: make(map[uuid.UUID]*loopHandle), foreignRegistry: &registry}
	ri := foldLoopInference([]event.Event{started, event.LoopAgentSessionBound{Header: started.Header, ACPSessionID: "bound-session"}})
	if err := s.attachRestoredLoop(started, loop.Provenance{}, bound, tool.Bindings{SessionID: s.sessionID, LoopID: started.LoopID}, foldLoop([]event.Event{}), ri, "legacy-session"); err != nil {
		t.Fatalf("attachRestoredLoop: %v", err)
	}
	if builder.restoreSeed.AgentSessionID != "bound-session" {
		t.Fatalf("AgentSessionID = %q, want bound-session", builder.restoreSeed.AgentSessionID)
	}
	missingSID := &Session{sessionCtx: context.Background(), sessionID: mustUUID(), newID: uuid.New, factory: event.NewFactory(uuid.New, time.Now), loops: make(map[uuid.UUID]*loopHandle), foreignRegistry: &registry}
	if err := missingSID.attachRestoredLoop(started, loop.Provenance{}, bound, tool.Bindings{SessionID: missingSID.sessionID, LoopID: started.LoopID}, foldLoop([]event.Event{}), restoredInference{}, ""); err == nil {
		t.Fatal("attachRestoredLoop succeeded without an AgentSessionID or legacy foreign SID")
	} else {
		var restoreErr *RestoreError
		if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreForeignSIDMissing {
			t.Fatalf("missing session id error = %T %v, want RestoreForeignSIDMissing", err, err)
		}
	}
}
