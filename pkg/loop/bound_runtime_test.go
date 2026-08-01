package loop

import (
	"context"
	"errors"
	"reflect"
	"testing"

	model "github.com/looprig/inference/model"
)

func TestEngineAdapterIsAValidClosedEngine(t *testing.T) {
	t.Parallel()

	definition := mustDefinition(t, WithEngine(EngineAdapter))
	bound, err := definition.Bind(context.Background(), validToolBindings(t))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if got := bound.Engine(); got != EngineAdapter {
		t.Fatalf("Engine() = %v, want EngineAdapter", got)
	}
	if got := bound.RuntimeProfile(); got != "" {
		t.Fatalf("RuntimeProfile() = %q, want empty native/default profile", got)
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
		{name: "zero effort", bound: bound, profile: "profile", target: testModel(), effort: model.EffortNone},
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
