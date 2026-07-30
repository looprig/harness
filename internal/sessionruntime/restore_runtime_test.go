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
		AgentType: "worker", AgentHarness: "codex", Profile: "acp/codex",
		Credential: loop.CredentialGatewayBacked, Default: true, DefaultModel: "luna", SmallModel: "small",
		Models: []loop.RuntimeModelOption{
			{Alias: "luna", Target: model.Model{Provider: "provider", Name: "luna-target"}, DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow, model.EffortHigh}},
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

func TestRestoreRuntimeBindingAcceptsConcreteTargetAlias(t *testing.T) {
	t.Parallel()
	catalog := restoreRuntimeCatalog(t)
	started := restoreRuntimeStarted(model.ModelKey{Provider: "provider", Model: "luna-target"})
	started.AgentRuntime.ModelAlias = "luna@high"
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), mustUUID(), started.LoopID)
	got, err := restoreRuntimeBinding(started, bound, foldLoopInference([]event.Event{started}), catalog, true, false)
	if err != nil {
		t.Fatalf("restoreRuntimeBinding: %v", err)
	}
	if identity := got.RuntimeIdentity(); identity.ModelAlias != "luna@high" {
		t.Fatalf("RuntimeIdentity().ModelAlias = %q, want concrete alias luna@high", identity.ModelAlias)
	}
}

func TestRestoreRuntimeBindingPreservesNativeHarnessManagedSelection(t *testing.T) {
	t.Parallel()

	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		AgentType: "worker", AgentHarness: "codex", Profile: "acp/codex",
		Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged, Default: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	started := event.LoopStarted{
		Header: event.Header{
			AgentName: "worker",
			Coordinates: identity.Coordinates{
				SessionID: mustUUID(),
				LoopID:    mustUUID(),
			},
			EventID: mustUUID(),
		},
		AgentRuntime: &event.AgentRuntime{
			Harness: "codex", Profile: "acp/codex", CredentialMode: "native-auth",
			Source: "native", SelectionKind: "harness-managed",
		},
	}
	wire, err := event.MarshalEvent(started)
	if err != nil {
		t.Fatalf("MarshalEvent() error = %v", err)
	}
	decoded, err := event.UnmarshalEvent(wire)
	if err != nil {
		t.Fatalf("UnmarshalEvent() error = %v", err)
	}
	var ok bool
	started, ok = decoded.(event.LoopStarted)
	if !ok {
		t.Fatalf("decoded event = %T, want event.LoopStarted", decoded)
	}
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), mustUUID(), started.LoopID)
	ri := foldLoopInference([]event.Event{started})
	if ri.HasRuntime || ri.Runtime != (event.ModelRuntime{}) {
		t.Fatalf("foldLoopInference() persisted runtime = has=%v runtime=%+v, want no runtime", ri.HasRuntime, ri.Runtime)
	}
	got, err := restoreRuntimeBinding(started, bound, ri, catalog, true, false)
	if err != nil {
		t.Fatalf("restoreRuntimeBinding() error = %v", err)
	}
	if got.Engine() != loop.EngineAdapter || got.RuntimeProfile() != "acp/codex" {
		t.Fatalf("restored engine/profile = %v/%q, want adapter/acp/codex", got.Engine(), got.RuntimeProfile())
	}
	runtime := got.RuntimeIdentity()
	if runtime.Source != loop.RuntimeSourceNative || runtime.SelectionKind != loop.RuntimeSelectionHarnessManaged || runtime.ModelAlias != "" || runtime.TargetModel != "" || runtime.Effort != model.EffortNone {
		t.Fatalf("restored managed identity = %+v, want native/harness-managed without model/effort", runtime)
	}
}

func TestRestoreRuntimeBindingRejectsNativeHarnessManagedCredentialMismatch(t *testing.T) {
	t.Parallel()

	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		AgentType: "worker", AgentHarness: "codex", Profile: "acp/codex",
		Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged, Default: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	started := event.LoopStarted{
		Header: event.Header{AgentName: "worker", Coordinates: identity.Coordinates{LoopID: mustUUID()}},
		AgentRuntime: &event.AgentRuntime{
			Harness: "codex", Profile: "acp/codex", CredentialMode: "gateway-backed",
			Source: "native", SelectionKind: "harness-managed",
		},
	}
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), mustUUID(), started.LoopID)
	_, err = restoreRuntimeBinding(started, bound, foldLoopInference([]event.Event{started}), catalog, true, false)
	var mismatch *RestoreRuntimeMismatchError
	if !errors.As(err, &mismatch) || mismatch.Kind != RestoreRuntimeCredentialMismatch {
		t.Fatalf("restoreRuntimeBinding() error = %T %v, want credential mismatch", err, err)
	}
}

func TestRestoreRuntimeBindingRejectsNativeHarnessManagedPersistedRuntime(t *testing.T) {
	t.Parallel()

	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{{
		AgentType: "worker", AgentHarness: "codex", Profile: "acp/codex",
		Credential: loop.CredentialNativeAuth, Source: loop.RuntimeSourceNative,
		SelectionKind: loop.RuntimeSelectionHarnessManaged, Default: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	started := event.LoopStarted{
		Header: event.Header{AgentName: "worker", Coordinates: identity.Coordinates{LoopID: mustUUID()}},
		AgentRuntime: &event.AgentRuntime{
			Harness: "codex", Profile: "acp/codex", CredentialMode: "native-auth",
			Source: "native", SelectionKind: "harness-managed",
		},
	}
	bound := bindCfg(engineCfg(&stubLLM{}, loop.EngineNative, "system"), mustUUID(), started.LoopID)
	ri := foldLoopInference([]event.Event{started})
	ri.Runtime = event.ModelRuntime{Key: model.ModelKey{Provider: "provider", Model: "should-not-persist"}, Effort: model.EffortMedium}
	ri.HasRuntime = true
	_, err = restoreRuntimeBinding(started, bound, ri, catalog, true, false)
	var mismatch *RestoreRuntimeMismatchError
	if !errors.As(err, &mismatch) || mismatch.Kind != RestoreRuntimeTargetMismatch {
		t.Fatalf("restoreRuntimeBinding() error = %T %v, want target mismatch", err, err)
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
	if err := s.attachRestoredLoop(started, loop.Provenance{}, bound, tool.Bindings{SessionID: s.sessionID, LoopID: started.LoopID}, foldLoop([]event.Event{}), ri, nil, "legacy-session"); err != nil {
		t.Fatalf("attachRestoredLoop: %v", err)
	}
	if builder.restoreSeed.AgentSessionID != "bound-session" {
		t.Fatalf("AgentSessionID = %q, want bound-session", builder.restoreSeed.AgentSessionID)
	}
	missingSID := &Session{sessionCtx: context.Background(), sessionID: mustUUID(), newID: uuid.New, factory: event.NewFactory(uuid.New, time.Now), loops: make(map[uuid.UUID]*loopHandle), foreignRegistry: &registry}
	if err := missingSID.attachRestoredLoop(started, loop.Provenance{}, bound, tool.Bindings{SessionID: missingSID.sessionID, LoopID: started.LoopID}, foldLoop([]event.Event{}), restoredInference{}, nil, ""); err == nil {
		t.Fatal("attachRestoredLoop succeeded without an AgentSessionID or legacy foreign SID")
	} else {
		var restoreErr *RestoreError
		if !errors.As(err, &restoreErr) || restoreErr.Kind != RestoreForeignSIDMissing {
			t.Fatalf("missing session id error = %T %v, want RestoreForeignSIDMissing", err, err)
		}
	}
}
