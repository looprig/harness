package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"

	model "github.com/looprig/inference/model"
)

func TestEngineAdapterIsAValidClosedEngine(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t)
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	bound, err = OverrideBoundRuntime(bound, "adapter-profile", testModel(), model.EffortHigh)
	if err != nil {
		t.Fatalf("OverrideBoundRuntime() error = %v", err)
	}
	if got := bound.Engine(); got != EngineAdapter {
		t.Fatalf("Engine() = %v, want EngineAdapter", got)
	}
	if got := bound.RuntimeProfile(); got != "adapter-profile" {
		t.Fatalf("RuntimeProfile() = %q, want adapter-profile", got)
	}
}

func TestOverrideBoundRuntimeSelectionRecordsAliasAndCanonicalNone(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t)
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	selected, err := OverrideBoundRuntimeSelection(bound, "acp/codex", "gpt-5.6-luna", testModel(), model.EffortNone)
	if err != nil {
		t.Fatalf("OverrideBoundRuntimeSelection() error = %v", err)
	}
	identity := selected.RuntimeIdentity()
	if identity.ModelAlias != "gpt-5.6-luna" || identity.Effort != model.EffortNone {
		t.Fatalf("RuntimeIdentity() = %+v, want alias and none effort", identity)
	}
	if identity.Digest() == "" {
		t.Fatal("RuntimeIdentity().Digest() = empty, want digest")
	}
	expected := sha256.Sum256([]byte(`{"domain":"loop/runtime-identity/v1","profile":"acp/codex","model_alias":"gpt-5.6-luna","target_provider":"lmstudio","target_model":"m","effort":"none"}`))
	if got, want := identity.Digest(), hex.EncodeToString(expected[:]); got != want {
		t.Fatalf("RuntimeIdentity().Digest() = %q, want canonical none digest %q", got, want)
	}

	legacy, err := OverrideBoundRuntime(bound, "acp/codex", testModel(), model.EffortNone)
	if err != nil {
		t.Fatalf("OverrideBoundRuntime() error = %v", err)
	}
	if legacy.RuntimeIdentity().ModelAlias != "" {
		t.Fatalf("legacy helper recorded alias %q, want empty", legacy.RuntimeIdentity().ModelAlias)
	}
	if legacy.RuntimeIdentity().Digest() == selected.RuntimeIdentity().Digest() {
		t.Fatal("alias change did not change runtime identity digest")
	}
}

func TestOverrideBoundRuntimeManagedRecordsNativeSelectionWithoutModelIdentity(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t)
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	managed, err := OverrideBoundRuntimeManaged(bound, "acp/codex")
	if err != nil {
		t.Fatalf("OverrideBoundRuntimeManaged() error = %v", err)
	}
	if managed.Engine() != EngineAdapter || managed.RuntimeProfile() != "acp/codex" {
		t.Fatalf("managed engine/profile = %v/%q, want adapter/acp/codex", managed.Engine(), managed.RuntimeProfile())
	}
	identity := managed.RuntimeIdentity()
	if identity.Source != RuntimeSourceNative || identity.SelectionKind != RuntimeSelectionHarnessManaged {
		t.Fatalf("managed identity source/selection = %q/%q, want native/harness-managed", identity.Source, identity.SelectionKind)
	}
	if identity.ModelAlias != "" || identity.TargetProvider != "" || identity.TargetModel != "" || identity.Effort != model.EffortNone {
		t.Fatalf("managed identity contains concrete selection: %+v", identity)
	}
	if identity.Digest() == "" {
		t.Fatal("managed identity digest is empty")
	}
}

func TestOverrideBoundRuntimeSelectionKeepsExplicitNativeRuntimeOnNativeEngine(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t)
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	target := testModel()
	target.Name = "native-runtime-model"
	selected, err := OverrideBoundRuntimeSelectionWithIdentity(bound, "looprig/native", "shared", target, model.EffortMedium, RuntimeSourceNative, RuntimeSelectionExplicit)
	if err != nil {
		t.Fatalf("OverrideBoundRuntimeSelectionWithIdentity() error = %v", err)
	}
	if selected.Engine() != EngineNative {
		t.Fatalf("Engine() = %v, want EngineNative", selected.Engine())
	}
	identity := selected.RuntimeIdentity()
	if identity.Source != RuntimeSourceNative || identity.SelectionKind != RuntimeSelectionExplicit || identity.ModelAlias != "shared" {
		t.Fatalf("RuntimeIdentity() = %+v, want explicit native selection", identity)
	}
}

func TestOverrideBoundRuntimeSelectionKeepsNativeACPOnAdapterEngine(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t)
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	target := testModel()
	target.Name = "native-acp-model"
	selected, err := OverrideBoundRuntimeSelectionWithIdentity(bound, "acp/codex", "native", target, model.EffortMedium, RuntimeSourceNative, RuntimeSelectionExplicit)
	if err != nil {
		t.Fatalf("OverrideBoundRuntimeSelectionWithIdentity() error = %v", err)
	}
	if selected.Engine() != EngineAdapter {
		t.Fatalf("Engine() = %v, want EngineAdapter for native-auth ACP", selected.Engine())
	}
}

func TestRuntimeIdentityManagedDigestOmitsModelAndEffortIdentity(t *testing.T) {
	t.Parallel()

	managed := RuntimeIdentity{Profile: "acp/codex", Source: RuntimeSourceNative, SelectionKind: RuntimeSelectionHarnessManaged}
	managedNone := managed
	managedNone.Effort = model.Effort("none")
	managedConcrete := managed
	managedConcrete.ModelAlias = "placeholder"
	managedConcrete.TargetProvider = "provider"
	managedConcrete.TargetModel = "model"
	managedConcrete.Effort = model.EffortHigh
	if managed.Digest() != managedNone.Digest() || managed.Digest() != managedConcrete.Digest() {
		t.Fatalf("managed digest changed with concrete/none representation: base=%q none=%q concrete=%q", managed.Digest(), managedNone.Digest(), managedConcrete.Digest())
	}
	expected := sha256.Sum256([]byte(`{"domain":"loop/runtime-identity/v1","profile":"acp/codex","source":"native","selection_kind":"harness-managed"}`))
	if got, want := managed.Digest(), hex.EncodeToString(expected[:]); got != want {
		t.Fatalf("managed digest = %q, want model/effort-free canonical digest %q", got, want)
	}
}

func TestOverrideBoundRuntimeSelectionRejectsInvalidProfilesAndAliases(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t)
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	for _, test := range []struct {
		name    string
		profile RuntimeProfileName
		alias   ModelAlias
	}{
		{name: "path profile", profile: "/tmp/child", alias: "model"},
		{name: "backslash profile", profile: `acp\\codex`, alias: "model"},
		{name: "dot dot profile", profile: "acp/../codex", alias: "model"},
		{name: "double slash profile", profile: "acp//other", alias: "model"},
		{name: "whitespace alias", profile: "acp/codex", alias: "model alias"},
		{name: "path alias", profile: "acp/codex", alias: "../model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := OverrideBoundRuntimeSelection(bound, test.profile, test.alias, testModel(), model.EffortHigh); err == nil {
				t.Fatal("OverrideBoundRuntimeSelection() error = nil, want error")
			}
		})
	}
}

func TestOverrideBoundRuntimeAcceptsEffortNone(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t)
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	overridden, err := OverrideBoundRuntime(bound, "adapter-profile", testModel(), model.EffortNone)
	if err != nil {
		t.Fatalf("OverrideBoundRuntime() error = %v, want nil", err)
	}
	if got := overridden.Effort(); got != model.EffortNone {
		t.Fatalf("Effort() = %q, want none", got)
	}
	if got := overridden.Model().Sampling.Effort; got != model.EffortNone {
		t.Fatalf("Model().Sampling.Effort = %q, want none", got)
	}
}

func TestOverrideBoundRuntimePinsTupleAcrossModesAndSelection(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t,
		WithModes(
			Mode{Name: "plan", Model: modelWithEffort(model.EffortLow), Effort: model.EffortLow, Instructions: "plan"},
			Mode{Name: "build", Model: modelWithEffort(model.EffortMedium), Effort: model.EffortMedium, Instructions: "build"},
		),
		WithInitialMode("plan"),
	)
	original, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	originalModes := original.Modes()
	target := testModel()
	target.Name = "pinned-runtime-model"

	overridden, err := OverrideBoundRuntime(original, "adapter-profile", target, model.EffortHigh)
	if err != nil {
		t.Fatalf("OverrideBoundRuntime() error = %v", err)
	}
	if got := overridden.Engine(); got != EngineAdapter {
		t.Fatalf("Engine() = %v, want EngineAdapter", got)
	}
	if got := overridden.RuntimeProfile(); got != "adapter-profile" {
		t.Fatalf("RuntimeProfile() = %q, want adapter-profile", got)
	}

	wantModel := target
	wantModel.Sampling.Effort = model.EffortHigh
	for _, name := range []ModeName{"", "plan", "build"} {
		mode, ok := overridden.Mode(name)
		if !ok {
			t.Fatalf("Mode(%q) missing", name)
		}
		if !reflect.DeepEqual(mode.Model, wantModel) {
			t.Errorf("Mode(%q).Model = %+v, want %+v", name, mode.Model, wantModel)
		}
		if mode.Effort != model.EffortHigh {
			t.Errorf("Mode(%q).Effort = %q, want high", name, mode.Effort)
		}
	}

	selected, err := SelectBoundMode(overridden, "build")
	if err != nil {
		t.Fatalf("SelectBoundMode() error = %v", err)
	}
	if got := selected.Model(); !reflect.DeepEqual(got, wantModel) {
		t.Fatalf("selected Model() = %+v, want %+v", got, wantModel)
	}
	if got := selected.Effort(); got != model.EffortHigh {
		t.Fatalf("selected Effort() = %q, want high", got)
	}

	if original.Engine() != EngineNative || original.RuntimeProfile() != "" {
		t.Fatalf("original runtime changed: engine=%v profile=%q", original.Engine(), original.RuntimeProfile())
	}
	if !reflect.DeepEqual(original.Modes(), originalModes) {
		t.Fatal("OverrideBoundRuntime mutated the original modes")
	}
}

func TestOverrideBoundRuntimeRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t)
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	invalidTarget := testModel()
	invalidTarget.BaseURL = "http://example.com"

	tests := []struct {
		name    string
		bound   BoundDefinition
		profile RuntimeProfileName
		target  model.Model
		effort  model.Effort
	}{
		{name: "nil bound", bound: nil, profile: "profile", target: testModel(), effort: model.EffortHigh},
		{name: "empty profile", bound: bound, profile: "", target: testModel(), effort: model.EffortHigh},
		{name: "invalid profile", bound: bound, profile: "profile with spaces", target: testModel(), effort: model.EffortHigh},
		{name: "zero target", bound: bound, profile: "profile", target: model.Model{}, effort: model.EffortHigh},
		{name: "invalid target", bound: bound, profile: "profile", target: invalidTarget, effort: model.EffortHigh},
		{name: "invalid effort", bound: bound, profile: "profile", target: testModel(), effort: model.Effort("invalid")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := OverrideBoundRuntime(tt.bound, tt.profile, tt.target, tt.effort)
			if err == nil {
				t.Fatal("OverrideBoundRuntime() error = nil, want error")
			}
			var bindErr *BindError
			if !errors.As(err, &bindErr) {
				t.Fatalf("error = %T %v, want *BindError", err, err)
			}
		})
	}
}

func TestBoundRuntimeIdentityDigestIncludesProfileAndCatalog(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t)
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	catalog, err := NewRuntimeCatalog(testCatalogEntries())
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}
	changedCatalogEntries := testCatalogEntries()
	changedCatalogEntries[0].Models[0].Target.Name = "different-target"
	changedCatalog, err := NewRuntimeCatalog(changedCatalogEntries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(changed) error = %v", err)
	}
	endpointOnlyEntries := testCatalogEntries()
	endpointOnlyEntries[0].Models[0].Target.BaseURL = "https://runtime.example.invalid"
	endpointOnlyCatalog, err := NewRuntimeCatalog(endpointOnlyEntries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(endpoint) error = %v", err)
	}
	if endpointOnlyCatalog.Digest() != catalog.Digest() {
		t.Fatalf("catalog digest changed for endpoint-only change: %q != %q", endpointOnlyCatalog.Digest(), catalog.Digest())
	}

	routed, err := OverrideBoundRuntime(bound, "profile-a", testModel(), model.EffortHigh)
	if err != nil {
		t.Fatalf("OverrideBoundRuntime() error = %v", err)
	}
	routed, err = OverrideBoundRuntimeCatalog(routed, catalog)
	if err != nil {
		t.Fatalf("OverrideBoundRuntimeCatalog() error = %v", err)
	}
	if got := routed.RuntimeCatalogDigest(); got != catalog.Digest() {
		t.Fatalf("RuntimeCatalogDigest() = %q, want %q", got, catalog.Digest())
	}
	baseDigest := routed.RuntimeIdentity().Digest()
	if baseDigest == "" {
		t.Fatal("RuntimeIdentity().Digest() = empty, want digest")
	}
	identity := routed.RuntimeIdentity()
	if identity.TargetProvider != testModel().Provider || identity.TargetModel != testModel().Name || identity.Effort != model.EffortHigh {
		t.Fatalf("RuntimeIdentity() = %+v, want selected provider/model/effort", identity)
	}

	modelChangedTarget := testModel()
	modelChangedTarget.Name = "different-runtime-model"
	modelChanged, err := OverrideBoundRuntime(bound, "profile-a", modelChangedTarget, model.EffortHigh)
	if err != nil {
		t.Fatalf("OverrideBoundRuntime(model changed) error = %v", err)
	}
	if got := modelChanged.RuntimeIdentity().Digest(); got == baseDigest {
		t.Fatal("runtime model change did not change identity digest")
	}

	effortChanged, err := OverrideBoundRuntime(bound, "profile-a", testModel(), model.EffortMax)
	if err != nil {
		t.Fatalf("OverrideBoundRuntime(effort changed) error = %v", err)
	}
	if got := effortChanged.RuntimeIdentity().Digest(); got == baseDigest {
		t.Fatal("runtime effort change did not change identity digest")
	}

	profileChanged, err := OverrideBoundRuntime(bound, "profile-b", testModel(), model.EffortHigh)
	if err != nil {
		t.Fatalf("OverrideBoundRuntime(profile changed) error = %v", err)
	}
	profileChanged, err = OverrideBoundRuntimeCatalog(profileChanged, catalog)
	if err != nil {
		t.Fatalf("OverrideBoundRuntimeCatalog(profile changed) error = %v", err)
	}
	if got := profileChanged.RuntimeIdentity().Digest(); got == baseDigest {
		t.Fatal("runtime profile change did not change identity digest")
	}

	catalogChanged, err := OverrideBoundRuntimeCatalog(routed, changedCatalog)
	if err != nil {
		t.Fatalf("OverrideBoundRuntimeCatalog(catalog changed) error = %v", err)
	}
	if got := catalogChanged.RuntimeIdentity().Digest(); got == baseDigest {
		t.Fatal("catalog digest change did not change identity digest")
	}

	endpointChanged, err := OverrideBoundRuntime(bound, "profile-a", func() model.Model {
		model := testModel()
		model.BaseURL = "https://endpoint-change.example.invalid"
		return model
	}(), model.EffortHigh)
	if err != nil {
		t.Fatalf("OverrideBoundRuntime(endpoint changed) error = %v", err)
	}
	endpointChanged, err = OverrideBoundRuntimeCatalog(endpointChanged, endpointOnlyCatalog)
	if err != nil {
		t.Fatalf("OverrideBoundRuntimeCatalog(endpoint changed) error = %v", err)
	}
	if got := endpointChanged.RuntimeIdentity().Digest(); got != baseDigest {
		t.Fatalf("endpoint change entered runtime identity digest: %q != %q", got, baseDigest)
	}
}
