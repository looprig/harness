package loop

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	model "github.com/looprig/inference/model"
)

func TestNewRuntimeCatalogInvariants(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func([]RuntimeCatalogEntry)
		wantKind  RuntimeCatalogErrorKind
		wantNoErr bool
	}{
		{name: "empty catalog is allowed", wantNoErr: true},
		{name: "empty credential", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Credential = "" }, wantKind: RuntimeCatalogInvalidCredential},
		{name: "unknown credential", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Credential = "credential-file" }, wantKind: RuntimeCatalogInvalidCredential},
		{name: "empty subagent type", mutate: func(entries []RuntimeCatalogEntry) { entries[0].SubagentType = "" }, wantKind: RuntimeCatalogInvalidIdentifier},
		{name: "empty harness", mutate: func(entries []RuntimeCatalogEntry) { entries[0].AgentHarness = "" }, wantKind: RuntimeCatalogInvalidIdentifier},
		{name: "empty profile", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Profile = "" }, wantKind: RuntimeCatalogInvalidIdentifier},
		{name: "path-like harness", mutate: func(entries []RuntimeCatalogEntry) { entries[0].AgentHarness = "/tmp/child" }, wantKind: RuntimeCatalogInvalidIdentifier},
		{name: "whitespace model alias", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Models[0].Alias = "model alias" }, wantKind: RuntimeCatalogInvalidIdentifier},
		{name: "invalid target model", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Models[0].Target.Name = "" }, wantKind: RuntimeCatalogInvalidModel},
		{name: "missing models", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Models = nil }, wantKind: RuntimeCatalogMissingDefaultModel},
		{name: "missing default model", mutate: func(entries []RuntimeCatalogEntry) { entries[0].DefaultModel = "missing" }, wantKind: RuntimeCatalogInvalidDefaultModel},
		{name: "duplicate aliases", mutate: func(entries []RuntimeCatalogEntry) {
			entries[0].Models = append(entries[0].Models, entries[0].Models[0])
		}, wantKind: RuntimeCatalogDuplicateAlias},
		{name: "duplicate harness entry", mutate: func(entries []RuntimeCatalogEntry) { entries[1].AgentHarness = entries[0].AgentHarness }, wantKind: RuntimeCatalogDuplicateHarness},
		{name: "two default harnesses", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Default = true }, wantKind: RuntimeCatalogDefaultHarnessCount},
		{name: "no default harness", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Default = false; entries[1].Default = false }, wantKind: RuntimeCatalogDefaultHarnessCount},
		{name: "invalid effort xhigh", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Models[0].Efforts = []model.Effort{"xhigh"} }, wantKind: RuntimeCatalogInvalidEffort},
		{name: "invalid effort ultra", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Models[0].Efforts = []model.Effort{"ultra"} }, wantKind: RuntimeCatalogInvalidEffort},
		{name: "invalid default effort", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Models[0].DefaultEffort = "xhigh" }, wantKind: RuntimeCatalogInvalidEffort},
		{name: "duplicate effort", mutate: func(entries []RuntimeCatalogEntry) {
			entries[0].Models[0].Efforts = []model.Effort{model.EffortLow, model.EffortLow}
		}, wantKind: RuntimeCatalogDuplicateEffort},
		{name: "default effort not advertised", mutate: func(entries []RuntimeCatalogEntry) { entries[0].Models[0].DefaultEffort = model.EffortMedium }, wantKind: RuntimeCatalogInvalidDefaultEffort},
		{name: "unknown required small model", mutate: func(entries []RuntimeCatalogEntry) {
			entries[0].NeedsSmallModel = true
			entries[0].SmallModel = "missing"
		}, wantKind: RuntimeCatalogInvalidSmallModel},
		{name: "native alias shared across harnesses", mutate: func(entries []RuntimeCatalogEntry) {
			entries[0].Credential = CredentialNativeAuth
			entries[1].Credential = CredentialNativeAuth
			entries[1].Models[0].Alias = entries[0].Models[0].Alias
			entries[1].DefaultModel = entries[1].Models[0].Alias
		}, wantKind: RuntimeCatalogNativeAliasConflict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var entries []RuntimeCatalogEntry
			if tt.name != "empty catalog is allowed" {
				entries = testCatalogEntries()
				if tt.mutate != nil {
					tt.mutate(entries)
				}
			}
			catalog, err := NewRuntimeCatalog(entries)
			if tt.wantNoErr {
				if err != nil {
					t.Fatalf("NewRuntimeCatalog() error = %v", err)
				}
				if got := catalog.EntriesFor("anything"); got != nil {
					t.Fatalf("empty EntriesFor() = %#v, want nil", got)
				}
				return
			}
			if err == nil {
				t.Fatal("NewRuntimeCatalog() error = nil, want error")
			}
			var catalogErr *RuntimeCatalogError
			if !errors.As(err, &catalogErr) {
				t.Fatalf("error = %T %v, want *RuntimeCatalogError", err, err)
			}
			if catalogErr.Kind != tt.wantKind {
				t.Fatalf("error kind = %q, want %q", catalogErr.Kind, tt.wantKind)
			}
		})
	}
}

func TestRuntimeCatalogAcceptsSafeProfileSegmentsAndRejectsPathLikeProfiles(t *testing.T) {
	t.Parallel()

	valid := testCatalogEntries()
	valid[0].Profile = "acp/codex"
	valid[1].Profile = "acp/claude-code"
	for _, profiles := range [][2]RuntimeProfileName{
		{"acp/codex", "acp/claude-code"},
		{"acp/other", "vendor/runtime-v2"},
		{"acp/claude-code/extra", "vendor/runtime/experimental"},
	} {
		entries := testCatalogEntries()
		entries[0].Profile, entries[1].Profile = profiles[0], profiles[1]
		if _, err := NewRuntimeCatalog(entries); err != nil {
			t.Fatalf("safe profiles %q and %q rejected: %v", profiles[0], profiles[1], err)
		}
	}

	for _, profile := range []RuntimeProfileName{
		"/acp/codex",
		"acp/codex/",
		"/tmp/child",
		`acp\\codex`,
		"acp/../codex",
		"acp/./codex",
		"acp//codex",
		"acp:codex",
		"acp codex",
		"C:/child",
		".",
		"..",
	} {
		entries := testCatalogEntries()
		entries[0].Profile = profile
		if _, err := NewRuntimeCatalog(entries); err == nil {
			t.Errorf("profile %q accepted, want rejection", profile)
		}
	}
}

func TestRuntimeCatalogSortsEntriesAndModels(t *testing.T) {
	entries := testCatalogEntries()
	entries[0].Models = append(entries[0].Models, RuntimeModelOption{
		Alias: "aaa", Target: runtimeModel("aaa-target", model.EffortNone), DefaultEffort: model.EffortNone,
		Efforts: []model.Effort{model.EffortNone},
	})
	entries[0].DefaultModel = "o3"
	entries[0].Models[0], entries[0].Models[1] = entries[0].Models[1], entries[0].Models[0]
	entries[0], entries[1] = entries[1], entries[0]

	catalog, err := NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	got := catalog.EntriesFor("worker")
	if len(got) != 2 {
		t.Fatalf("EntriesFor(worker) length = %d, want 2", len(got))
	}
	if got[0].AgentHarness != "claude-code" || got[1].AgentHarness != "codex" {
		t.Fatalf("sorted harnesses = %q, %q", got[0].AgentHarness, got[1].AgentHarness)
	}
	if got[1].Models[0].Alias != "aaa" || got[1].Models[1].Alias != "o3" {
		t.Fatalf("sorted model aliases = %q, %q", got[1].Models[0].Alias, got[1].Models[1].Alias)
	}
}

func TestRuntimeCatalogResolveDefaultsAndRejectsIncompatibleSelectors(t *testing.T) {
	catalog, err := NewRuntimeCatalog(testCatalogEntries())
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	tests := []struct {
		name    string
		agent   identity.AgentName
		harness AgentHarnessName
		alias   ModelAlias
		effort  model.Effort
		want    Resolved
		wantErr RuntimeCatalogErrorKind
	}{
		{
			name: "all omitted use defaults", want: Resolved{
				SubagentType: "worker", AgentHarness: "claude-code", Profile: "claude-profile",
				Source: RuntimeSourceGateway, SelectionKind: RuntimeSelectionExplicit,
				Credential: CredentialGatewayBacked, ModelAlias: "sonnet", TargetAlias: "sonnet", SmallModel: "sonnet-small",
				Target: runtimeModel("sonnet-target", model.EffortMedium), Effort: model.EffortMedium,
			},
		},
		{
			name: "explicit complete tuple", harness: "codex", alias: "o3", effort: model.EffortHigh,
			want: Resolved{
				SubagentType: "worker", AgentHarness: "codex", Profile: "codex-profile",
				Source: RuntimeSourceGateway, SelectionKind: RuntimeSelectionExplicit,
				Credential: CredentialGatewayBacked, ModelAlias: "o3", TargetAlias: "o3@high", SmallModel: "o3",
				Target: runtimeModel("o3-target", model.EffortLow), Effort: model.EffortHigh,
			},
		},
		{name: "omitted model uses selected harness default", harness: "codex", want: Resolved{AgentHarness: "codex", ModelAlias: "o3", Effort: model.EffortLow}},
		{name: "omitted effort uses selected model default", harness: "codex", alias: "o3", want: Resolved{AgentHarness: "codex", ModelAlias: "o3", Effort: model.EffortLow}},
		{name: "unknown harness does not fall back", harness: "missing", wantErr: RuntimeCatalogUnknownHarness},
		{name: "unknown alias does not fall back", harness: "codex", alias: "sonnet", wantErr: RuntimeCatalogUnknownModel},
		{name: "incompatible effort does not clamp", harness: "codex", alias: "o3", effort: model.EffortMedium, wantErr: RuntimeCatalogIncompatibleEffort},
		{name: "alias checked against default harness", alias: "o3", wantErr: RuntimeCatalogUnknownModel},
		{name: "unknown agent", agent: "missing", wantErr: RuntimeCatalogUnknownAgent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := tt.agent
			if agent == "" {
				agent = "worker"
			}
			got, err := catalog.Resolve(agent, tt.harness, tt.alias, tt.effort)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("Resolve() error = nil, want error")
				}
				var catalogErr *RuntimeCatalogError
				if !errors.As(err, &catalogErr) || catalogErr.Kind != tt.wantErr {
					t.Fatalf("Resolve() error = %T %v, want kind %q", err, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if tt.want.SubagentType != "" {
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("Resolve() = %#v, want %#v", got, tt.want)
				}
				return
			}
			if got.AgentHarness != tt.want.AgentHarness || got.ModelAlias != tt.want.ModelAlias || got.Effort != tt.want.Effort {
				t.Fatalf("Resolve() selectors = %q/%q/%q, want %q/%q/%q", got.AgentHarness, got.ModelAlias, got.Effort, tt.want.AgentHarness, tt.want.ModelAlias, tt.want.Effort)
			}
		})
	}
}

func TestRuntimeCatalogDerivesConcreteGatewayTargetAliasesWithoutChangingSelectors(t *testing.T) {
	t.Parallel()

	catalog, err := NewRuntimeCatalog(testCatalogEntries())
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	defaultTarget, err := catalog.Resolve("worker", "claude-code", "sonnet", model.EffortMedium)
	if err != nil {
		t.Fatalf("Resolve(default) error = %v", err)
	}
	if defaultTarget.ModelAlias != "sonnet" || defaultTarget.TargetAlias != "sonnet" {
		t.Fatalf("default resolved aliases = model %q, target %q; want sonnet/sonnet", defaultTarget.ModelAlias, defaultTarget.TargetAlias)
	}

	highTarget, err := catalog.Resolve("worker", "claude-code", "sonnet", model.EffortHigh)
	if err != nil {
		t.Fatalf("Resolve(high) error = %v", err)
	}
	if highTarget.ModelAlias != "sonnet" || highTarget.TargetAlias != "sonnet@high" {
		t.Fatalf("high resolved aliases = model %q, target %q; want sonnet/sonnet@high", highTarget.ModelAlias, highTarget.TargetAlias)
	}

	nativeEntries := testCatalogEntries()
	nativeEntries[0].Credential = CredentialNativeAuth
	nativeEntries[0].Models[0].Credential = CredentialNativeAuth
	nativeCatalog, err := NewRuntimeCatalog(nativeEntries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(native) error = %v", err)
	}
	nativeTarget, err := nativeCatalog.Resolve("worker", "codex", "o3", model.EffortHigh)
	if err != nil {
		t.Fatalf("Resolve(native high) error = %v", err)
	}
	if nativeTarget.ModelAlias != "o3" || nativeTarget.TargetAlias != "o3" {
		t.Fatalf("native resolved aliases = model %q, target %q; want o3/o3", nativeTarget.ModelAlias, nativeTarget.TargetAlias)
	}
}

func TestRuntimeCatalogRejectsConfiguredAliasesThatCollideWithDerivedGatewayAliases(t *testing.T) {
	t.Parallel()

	entries := testCatalogEntries()
	entries[1].Models = append(entries[1].Models, RuntimeModelOption{
		Alias:         "sonnet@high",
		Target:        runtimeModel("collision-target", model.EffortHigh),
		DefaultEffort: model.EffortHigh,
		Efforts:       []model.Effort{model.EffortHigh},
	})
	_, err := NewRuntimeCatalog(entries)
	if err == nil {
		t.Fatal("NewRuntimeCatalog() error = nil, want derived alias collision")
	}
	var catalogErr *RuntimeCatalogError
	if !errors.As(err, &catalogErr) || catalogErr.Kind != RuntimeCatalogDerivedAliasConflict {
		t.Fatalf("error = %T %v, want kind %q", err, err, RuntimeCatalogDerivedAliasConflict)
	}
}

func TestRuntimeCatalogResolvesConcreteAndLegacyTargetAliases(t *testing.T) {
	t.Parallel()

	catalog, err := NewRuntimeCatalog(testCatalogEntries())
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	for _, alias := range []ModelAlias{"sonnet@high", "sonnet"} {
		resolved, err := catalog.ResolveTargetAlias("worker", "claude-code", alias, model.EffortHigh)
		if err != nil {
			t.Fatalf("ResolveTargetAlias(%q) error = %v", alias, err)
		}
		if resolved.ModelAlias != "sonnet" || resolved.TargetAlias != "sonnet@high" || resolved.Effort != model.EffortHigh {
			t.Fatalf("ResolveTargetAlias(%q) = model %q, target %q, effort %q; want sonnet/sonnet@high/high", alias, resolved.ModelAlias, resolved.TargetAlias, resolved.Effort)
		}
	}
}

func TestRuntimeCatalogResolveWithExplicitEffortDistinguishesNoneFromOmitted(t *testing.T) {
	t.Parallel()

	entries := testCatalogEntries()
	entries[1].Models[0].DefaultEffort = model.EffortHigh
	entries[1].Models[0].Efforts = []model.Effort{model.EffortNone, model.EffortHigh}
	catalog, err := NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	omitted, err := catalog.Resolve("worker", "claude-code", "sonnet", model.EffortNone)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if omitted.Effort != model.EffortHigh {
		t.Fatalf("omitted effort = %q, want high", omitted.Effort)
	}

	explicit, err := catalog.ResolveWithExplicitEffort("worker", "claude-code", "sonnet", model.EffortNone, true)
	if err != nil {
		t.Fatalf("ResolveWithExplicitEffort() error = %v", err)
	}
	if explicit.Effort != model.EffortNone {
		t.Fatalf("explicit none effort = %q, want none", explicit.Effort)
	}

	entries = testCatalogEntries()
	catalog, err = NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() without none error = %v", err)
	}
	if _, err := catalog.ResolveWithExplicitEffort("worker", "claude-code", "sonnet", model.EffortNone, true); err == nil {
		t.Fatal("explicit none accepted for a model that does not advertise none")
	}
}

func TestRuntimeCatalogDefensiveCopiesAndDigest(t *testing.T) {
	entries := testCatalogEntries()
	originalTarget := entries[1].Models[0].Target
	catalog, err := NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}
	digest := catalog.Digest()
	if len(digest) != 64 {
		t.Fatalf("Digest() length = %d, want 64", len(digest))
	}

	entries[0].Models[0].Alias = "changed"
	entries[0].Models[0].Efforts[0] = model.EffortMax
	entries[0].Models[0].Target.Name = "changed-target"
	if got := catalog.Digest(); got != digest {
		t.Fatalf("Digest() changed after input mutation: %q != %q", got, digest)
	}

	returned := catalog.EntriesFor("worker")
	returned[0].Models[0].Alias = "changed-again"
	returned[0].Models[0].Target.Name = "changed-again-target"
	returned[0].Models[0].Efforts[0] = model.EffortMax
	if got := catalog.Digest(); got != digest {
		t.Fatalf("Digest() changed after lookup mutation: %q != %q", got, digest)
	}

	resolved, err := catalog.Resolve("worker", "claude-code", "sonnet", "")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	resolved.Target.Name = "changed-resolved-target"
	if got, err := catalog.Resolve("worker", "claude-code", "sonnet", ""); err != nil || got.Target.Name != originalTarget.Name {
		t.Fatalf("resolved target was not defensive: got=%q err=%v", got.Target.Name, err)
	}

	other, err := NewRuntimeCatalog(testCatalogEntries())
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(other) error = %v", err)
	}
	if other.Digest() != digest {
		t.Fatalf("equal catalogs have different digests: %q != %q", other.Digest(), digest)
	}
	changed := testCatalogEntries()
	changed[0].Models[0].Target.Name = "different-target"
	third, err := NewRuntimeCatalog(changed)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(changed) error = %v", err)
	}
	if third.Digest() == digest {
		t.Fatal("catalog digest did not change for a different target identity")
	}
	if strings.Contains(digest, "secret") || strings.Contains(digest, "https") {
		t.Fatalf("Digest() contains raw catalog material: %q", digest)
	}

	mutations := []struct {
		name   string
		mutate func(*model.Model)
	}{
		{name: "provider", mutate: func(target *model.Model) { target.Provider = "different-provider" }},
		{name: "API format", mutate: func(target *model.Model) { target.APIFormat = "different-api-format" }},
		{name: "temperature", mutate: func(target *model.Model) { value := 0.25; target.Sampling.Temperature = &value }},
		{name: "top-p", mutate: func(target *model.Model) { value := 0.75; target.Sampling.TopP = &value }},
		{name: "max tokens", mutate: func(target *model.Model) { value := 321; target.Sampling.MaxTokens = &value }},
		{name: "stop", mutate: func(target *model.Model) { target.Sampling.Stop = []string{"<stop>"} }},
		{name: "effort", mutate: func(target *model.Model) { target.Sampling.Effort = model.EffortHigh }},
	}
	for _, mutation := range mutations {
		entries := testCatalogEntries()
		mutation.mutate(&entries[0].Models[0].Target)
		changedCatalog, err := NewRuntimeCatalog(entries)
		if err != nil {
			t.Fatalf("NewRuntimeCatalog(%s) error = %v", mutation.name, err)
		}
		if changedCatalog.Digest() == digest {
			t.Errorf("catalog digest did not change for %s mutation", mutation.name)
		}
	}

	changedWiring := testCatalogEntries()
	changedWiring[0].Source = RuntimeSourceNative
	changedWiring[0].Credential = CredentialNativeAuth
	changedWiring[0].Models[0].Source = RuntimeSourceNative
	changedWiring[0].Models[0].Credential = CredentialNativeAuth
	wiringCatalog, err := NewRuntimeCatalog(changedWiring)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(changed wiring) error = %v", err)
	}
	if wiringCatalog.Digest() == digest {
		t.Fatal("catalog digest did not change for source and credential wiring")
	}
}

func TestRuntimeCatalogDescriptionCloning(t *testing.T) {
	entries := testCatalogEntries()
	entries[0].Description = "Codex harness guidance"
	entries[0].Models[0].Description = "Use o3 for difficult implementation work"

	catalog, err := NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}
	entries[0].Description = "changed input harness description"
	entries[0].Models[0].Description = "changed input model description"

	got := catalog.EntriesFor("worker")
	codex := runtimeCatalogEntryForHarness(t, got, "codex")
	if codex.Description != "Codex harness guidance" {
		t.Fatalf("entry description = %q, want original description", codex.Description)
	}
	if codex.Models[0].Description != "Use o3 for difficult implementation work" {
		t.Fatalf("model description = %q, want original description", codex.Models[0].Description)
	}

	codex.Description = "changed returned harness description"
	codex.Models[0].Description = "changed returned model description"
	again := runtimeCatalogEntryForHarness(t, catalog.EntriesFor("worker"), "codex")
	if again.Description != "Codex harness guidance" || again.Models[0].Description != "Use o3 for difficult implementation work" {
		t.Fatalf("returned descriptions mutated catalog: entry=%q model=%q", again.Description, again.Models[0].Description)
	}
}

func TestRuntimeCatalogDescriptionValidation(t *testing.T) {
	tests := []struct {
		name        string
		description string
		wantErr     bool
	}{
		{name: "absent"},
		{name: "single line", description: "Use for focused implementation work."},
		{name: "maximum bytes", description: strings.Repeat("a", 256)},
		{name: "invalid UTF-8", description: string([]byte{0xff}), wantErr: true},
		{name: "blank present", description: "   ", wantErr: true},
		{name: "leading whitespace", description: " guidance", wantErr: true},
		{name: "trailing whitespace", description: "guidance ", wantErr: true},
		{name: "newline", description: "first\nsecond", wantErr: true},
		{name: "carriage return", description: "first\rsecond", wantErr: true},
		{name: "tab", description: "first\tsecond", wantErr: true},
		{name: "unicode line separator", description: "first\u2028second", wantErr: true},
		{name: "over maximum bytes", description: strings.Repeat("a", 257), wantErr: true},
	}

	fields := []struct {
		name   string
		field  string
		mutate func([]RuntimeCatalogEntry, string)
	}{
		{name: "harness", field: "Description", mutate: func(entries []RuntimeCatalogEntry, description string) {
			entries[0].Description = description
		}},
		{name: "model", field: "Models.Description", mutate: func(entries []RuntimeCatalogEntry, description string) {
			entries[0].Models[0].Description = description
		}},
	}

	for _, field := range fields {
		for _, tt := range tests {
			t.Run(field.name+"/"+tt.name, func(t *testing.T) {
				entries := testCatalogEntries()
				field.mutate(entries, tt.description)
				_, err := NewRuntimeCatalog(entries)
				if !tt.wantErr {
					if err != nil {
						t.Fatalf("NewRuntimeCatalog() error = %v", err)
					}
					return
				}
				var catalogErr *RuntimeCatalogError
				if !errors.As(err, &catalogErr) {
					t.Fatalf("NewRuntimeCatalog() error = %T %v, want *RuntimeCatalogError", err, err)
				}
				if catalogErr.Kind != RuntimeCatalogInvalidDescription || catalogErr.Field != field.field {
					t.Fatalf("catalog error = %#v, want kind %q field %q", catalogErr, RuntimeCatalogInvalidDescription, field.field)
				}
			})
		}
	}
}

func TestRuntimeCatalogDescriptionDigest(t *testing.T) {
	entries := testCatalogEntries()
	entries[0].Description = "Codex harness guidance"
	entries[0].Models[0].Description = "Use o3 for difficult implementation work"
	entries[0].Models[0].Target.BaseURL = "https://endpoint.example/v1"

	catalog, err := NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}
	baseDigest := catalog.Digest()

	changedHarness := testCatalogEntries()
	changedHarness[0].Description = "Different Codex harness guidance"
	changedHarness[0].Models[0].Description = entries[0].Models[0].Description
	harnessCatalog, err := NewRuntimeCatalog(changedHarness)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(changed harness) error = %v", err)
	}
	if harnessCatalog.Digest() == baseDigest {
		t.Fatal("catalog digest did not change for harness description")
	}

	changedModel := entries
	changedModel[0].Models[0].Description = "Different o3 model guidance"
	modelCatalog, err := NewRuntimeCatalog(changedModel)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(changed model) error = %v", err)
	}
	if modelCatalog.Digest() == baseDigest {
		t.Fatal("catalog digest did not change for model description")
	}

	encoded, err := runtimeCatalogDigestJSON(catalog.entries)
	if err != nil {
		t.Fatalf("runtimeCatalogDigestJSON() error = %v", err)
	}
	var projection any
	if err := json.Unmarshal(encoded, &projection); err != nil {
		t.Fatalf("unmarshal digest JSON: %v", err)
	}
	if got, want := countJSONKey(projection, "description"), 2; got != want {
		t.Fatalf("digest JSON description count = %d, want %d: %s", got, want, encoded)
	}
	for _, forbidden := range []string{"https://endpoint.example/v1", "base_url", "api_key", "secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("digest JSON contains forbidden provider wiring %q: %s", forbidden, encoded)
		}
	}
}

func TestRuntimeCatalogDigestProjectionOmitsRuntimeWiring(t *testing.T) {
	entries := testCatalogEntries()
	option := &entries[0].Models[0]
	option.NativeSmallModel = "private-native-small-model"
	option.Target.Provider = "private-provider-id"
	option.Target.APIFormat = "private-api-format"
	option.Target.BaseURL = "https://private-endpoint.example/v1"
	option.Target.Name = "private-provider-model-id"
	temperature := 0.25
	option.Target.Sampling.Temperature = &temperature
	option.Target.Sampling.Stop = []string{"private-stop-sequence"}

	catalog, err := NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}
	encoded, err := runtimeCatalogDigestJSON(catalog.entries)
	if err != nil {
		t.Fatalf("runtimeCatalogDigestJSON() error = %v", err)
	}
	var projection any
	if err := json.Unmarshal(encoded, &projection); err != nil {
		t.Fatalf("unmarshal digest JSON: %v", err)
	}
	if got, want := countJSONKey(projection, "configuration_fingerprint"), len(catalog.entries); got != want {
		t.Fatalf("digest JSON configuration fingerprint count = %d, want %d: %s", got, want, encoded)
	}

	for _, forbidden := range []string{
		"source", "credential", "native_small_model",
		"provider", "api_format", "name", "origin", "capabilities", "limits",
		"temperature", "top_p", "max_tokens", "stop", "sampling_effort",
	} {
		if got := countJSONKey(projection, forbidden); got != 0 {
			t.Errorf("digest JSON contains forbidden field %q %d time(s): %s", forbidden, got, encoded)
		}
	}
	for _, forbidden := range []string{
		"gateway-backed", "gateway",
		"private-native-small-model", "private-provider-id", "private-api-format",
		"https://private-endpoint.example/v1", "private-provider-model-id", "private-stop-sequence",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("digest JSON contains forbidden runtime wiring value %q: %s", forbidden, encoded)
		}
	}
}

func runtimeCatalogEntryForHarness(t *testing.T, entries []RuntimeCatalogEntry, harness AgentHarnessName) *RuntimeCatalogEntry {
	t.Helper()
	for i := range entries {
		if entries[i].AgentHarness == harness {
			return &entries[i]
		}
	}
	t.Fatalf("no entry for harness %q", harness)
	return nil
}

func countJSONKey(value any, wanted string) int {
	switch value := value.(type) {
	case []any:
		count := 0
		for _, item := range value {
			count += countJSONKey(item, wanted)
		}
		return count
	case map[string]any:
		count := 0
		for key, item := range value {
			if key == wanted {
				count++
			}
			count += countJSONKey(item, wanted)
		}
		return count
	default:
		return 0
	}
}

func TestRuntimeCatalogNeedsSmallModel(t *testing.T) {
	t.Parallel()

	entries := testCatalogEntries()
	entries[1].NeedsSmallModel = true
	if _, err := NewRuntimeCatalog(entries); err != nil {
		t.Fatalf("catalog with required small model rejected: %v", err)
	}

	entries[1].SmallModel = ""
	if _, err := NewRuntimeCatalog(entries); err == nil {
		t.Fatal("catalog with required empty small model accepted")
	}

	entries = testCatalogEntries()
	entries[1].NeedsSmallModel = true
	entries[1].SmallModel = "missing"
	if _, err := NewRuntimeCatalog(entries); err == nil {
		t.Fatal("catalog with required unknown small model accepted")
	}

	entries = testCatalogEntries()
	entries[1].SmallModel = ""
	if _, err := NewRuntimeCatalog(entries); err != nil {
		t.Fatalf("catalog with optional empty small model rejected: %v", err)
	}
}

func TestRuntimeCatalogNeedsSmallModelAffectsDigest(t *testing.T) {
	t.Parallel()

	base, err := NewRuntimeCatalog(testCatalogEntries())
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(base) error = %v", err)
	}
	entries := testCatalogEntries()
	entries[1].NeedsSmallModel = true
	changed, err := NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(changed) error = %v", err)
	}
	if base.Digest() == changed.Digest() {
		t.Fatal("catalog digest did not change for NeedsSmallModel mutation")
	}
}

func TestRuntimeCatalogResolvesNativeSmallModel(t *testing.T) {
	t.Parallel()

	entries := testCatalogEntries()
	entries[0].Credential = CredentialNativeAuth
	entries[0].Models[0].Credential = CredentialNativeAuth
	entries[0].Models[0].NativeSmallModel = "gpt-5.6-mini"
	catalog, err := NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	resolved, err := catalog.Resolve("worker", "codex", "o3", model.EffortLow)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.NativeSmallModel != "gpt-5.6-mini" {
		t.Fatalf("Resolved.NativeSmallModel = %q, want %q", resolved.NativeSmallModel, "gpt-5.6-mini")
	}
}

func TestRuntimeCatalogNativeSmallModelAffectsDigest(t *testing.T) {
	t.Parallel()

	baseEntries := testCatalogEntries()
	baseEntries[0].Credential = CredentialNativeAuth
	baseEntries[0].Models[0].Credential = CredentialNativeAuth
	base, err := NewRuntimeCatalog(baseEntries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(base) error = %v", err)
	}

	changedEntries := testCatalogEntries()
	changedEntries[0].Credential = CredentialNativeAuth
	changedEntries[0].Models[0].Credential = CredentialNativeAuth
	changedEntries[0].Models[0].NativeSmallModel = "gpt-5.6-mini"
	changed, err := NewRuntimeCatalog(changedEntries)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog(changed) error = %v", err)
	}
	if base.Digest() == changed.Digest() {
		t.Fatal("catalog digest did not change for NativeSmallModel mutation")
	}
}

func TestRuntimeCatalogAllowsSharedGatewayAliasesButNotNativeAliases(t *testing.T) {
	entries := testCatalogEntries()
	entries[0].Credential = CredentialGatewayBacked
	entries[1].Credential = CredentialGatewayBacked
	entries[1].Models[0].Alias = entries[0].Models[0].Alias
	entries[1].DefaultModel = entries[1].Models[0].Alias
	if _, err := NewRuntimeCatalog(entries); err != nil {
		t.Fatalf("shared gateway alias rejected: %v", err)
	}

	entries = testCatalogEntries()
	entries[0].Credential = CredentialNativeAuth
	entries[1].Credential = CredentialNativeAuth
	entries[1].Models[0].Alias = entries[0].Models[0].Alias
	entries[1].DefaultModel = entries[1].Models[0].Alias
	if _, err := NewRuntimeCatalog(entries); err == nil {
		t.Fatal("shared native alias accepted, want error")
	}
}

func TestRuntimeCatalogAllowsNativeAndGatewayModelsWithinOneHarness(t *testing.T) {
	entries := testCatalogEntries()
	entries[0].Models = append(entries[0].Models, RuntimeModelOption{
		Alias: "codex-native", Credential: CredentialNativeAuth,
		Target:        runtimeModel("native-target", model.EffortNone),
		DefaultEffort: model.EffortNone, Efforts: []model.Effort{model.EffortNone},
	})
	catalog, err := NewRuntimeCatalog(entries)
	if err != nil {
		t.Fatalf("mixed gateway/native entry rejected: %v", err)
	}
	resolved, err := catalog.Resolve("worker", "codex", "codex-native", model.EffortNone)
	if err != nil {
		t.Fatalf("native model did not resolve: %v", err)
	}
	if resolved.Credential != CredentialNativeAuth {
		t.Fatalf("resolved credential = %q, want %q", resolved.Credential, CredentialNativeAuth)
	}
	if _, err := catalog.Resolve("worker", "claude-code", "codex-native", model.EffortNone); err == nil {
		t.Fatal("native model resolved under a different harness")
	}

	entries[0].Models[len(entries[0].Models)-1].Alias = entries[1].Models[0].Alias
	if _, err := NewRuntimeCatalog(entries); err == nil {
		t.Fatal("native model sharing a gateway alias across harnesses was accepted")
	}
}

func TestRuntimeCatalogResolvesNativeHarnessManagedEntryWithoutModelIdentity(t *testing.T) {
	t.Parallel()

	entry := RuntimeCatalogEntry{
		SubagentType:  "worker",
		AgentHarness:  "codex",
		Profile:       "acp/codex",
		Credential:    CredentialNativeAuth,
		Source:        RuntimeSourceNative,
		SelectionKind: RuntimeSelectionHarnessManaged,
		Default:       true,
	}
	catalog, err := NewRuntimeCatalog([]RuntimeCatalogEntry{entry})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	resolved, err := catalog.Resolve("worker", "codex", "", model.EffortNone)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Source != RuntimeSourceNative || resolved.SelectionKind != RuntimeSelectionHarnessManaged {
		t.Fatalf("resolved source/selection = %q/%q, want native/harness-managed", resolved.Source, resolved.SelectionKind)
	}
	if resolved.ModelAlias != "" || resolved.TargetAlias != "" || resolved.SmallModel != "" || resolved.NativeSmallModel != "" {
		t.Fatalf("resolved model identity = alias %q target %q small %q native-small %q, want all empty", resolved.ModelAlias, resolved.TargetAlias, resolved.SmallModel, resolved.NativeSmallModel)
	}
	if !reflect.DeepEqual(resolved.Target, model.Model{}) || resolved.Effort != model.EffortNone {
		t.Fatalf("resolved concrete target/effort = %+v/%q, want zero/none", resolved.Target, resolved.Effort)
	}

	if _, err := catalog.ResolveWithExplicitEffort("worker", "codex", "model", model.EffortNone, false); err == nil {
		t.Fatal("Resolve() accepted a model selector for a harness-managed entry")
	}
	if _, err := catalog.ResolveWithExplicitEffort("worker", "codex", "", model.EffortHigh, true); err == nil {
		t.Fatal("Resolve() accepted an effort selector for a harness-managed entry")
	}
}

func TestRuntimeCatalogAllowsSameHarnessWithDistinctGatewayAndNativeSources(t *testing.T) {
	t.Parallel()

	native := RuntimeCatalogEntry{
		SubagentType:  "worker",
		AgentHarness:  "codex",
		Profile:       "acp/codex-native",
		Credential:    CredentialNativeAuth,
		Source:        RuntimeSourceNative,
		SelectionKind: RuntimeSelectionHarnessManaged,
		Default:       true,
	}
	gateway := RuntimeCatalogEntry{
		SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex-gateway",
		Credential: CredentialGatewayBacked, Source: RuntimeSourceGateway,
		DefaultModel: "luna",
		Models: []RuntimeModelOption{{
			Alias: "luna", Target: runtimeModel("luna-target", model.EffortMedium),
			DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium},
		}},
	}
	catalog, err := NewRuntimeCatalog([]RuntimeCatalogEntry{native, gateway})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	managed, err := catalog.Resolve("worker", "codex", "", model.EffortNone)
	if err != nil {
		t.Fatalf("Resolve(managed) error = %v", err)
	}
	if managed.Source != RuntimeSourceNative || managed.SelectionKind != RuntimeSelectionHarnessManaged {
		t.Fatalf("managed source/selection = %q/%q, want native/harness-managed", managed.Source, managed.SelectionKind)
	}
	explicit, err := catalog.Resolve("worker", "codex", "luna", model.EffortNone)
	if err != nil {
		t.Fatalf("Resolve(gateway) error = %v", err)
	}
	if explicit.Source != RuntimeSourceGateway || explicit.SelectionKind != RuntimeSelectionExplicit {
		t.Fatalf("explicit source/selection = %q/%q, want gateway/explicit", explicit.Source, explicit.SelectionKind)
	}
}

func TestRuntimeCatalogResolvesExplicitSourceForSameHarness(t *testing.T) {
	t.Parallel()

	gateway := RuntimeCatalogEntry{
		SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex-gateway",
		Credential: CredentialGatewayBacked, Source: RuntimeSourceGateway, Default: true,
		DefaultModel: "luna",
		Models: []RuntimeModelOption{{
			Alias: "luna", Target: runtimeModel("luna-target", model.EffortHigh),
			DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortHigh},
		}},
	}
	native := RuntimeCatalogEntry{
		SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex-native",
		Credential: CredentialNativeAuth, Source: RuntimeSourceNative,
		SelectionKind: RuntimeSelectionHarnessManaged,
	}
	catalog, err := NewRuntimeCatalog([]RuntimeCatalogEntry{gateway, native})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	explicit, err := catalog.ResolveWithExplicitSource("worker", "codex", RuntimeSourceGateway, "", model.EffortNone, false)
	if err != nil {
		t.Fatalf("ResolveWithExplicitSource(gateway) error = %v", err)
	}
	if explicit.Source != RuntimeSourceGateway || explicit.SelectionKind != RuntimeSelectionExplicit || explicit.ModelAlias != "luna" {
		t.Fatalf("gateway resolution = %+v, want gateway/explicit/luna", explicit)
	}

	managed, err := catalog.ResolveWithExplicitSource("worker", "codex", RuntimeSourceNative, "", model.EffortNone, false)
	if err != nil {
		t.Fatalf("ResolveWithExplicitSource(native) error = %v", err)
	}
	if managed.Source != RuntimeSourceNative || managed.SelectionKind != RuntimeSelectionHarnessManaged || managed.ModelAlias != "" || managed.TargetAlias != "" {
		t.Fatalf("native resolution = %+v, want native/harness-managed without model identity", managed)
	}

	if _, err := catalog.ResolveWithExplicitSource("worker", "codex", RuntimeSourceNative, "luna", model.EffortNone, false); err == nil {
		t.Fatal("ResolveWithExplicitSource(native, model) accepted a model-less combination")
	}
	if _, err := catalog.ResolveWithExplicitSource("worker", "codex", RuntimeSourceNative, "", model.EffortHigh, true); err == nil {
		t.Fatal("ResolveWithExplicitSource(native, effort) accepted a model-less combination")
	}
}

func TestRuntimeCatalogResolvesExplicitSourceUsingSourceLocalDefault(t *testing.T) {
	t.Parallel()

	catalog, err := NewRuntimeCatalog([]RuntimeCatalogEntry{{
		SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex-mixed",
		Credential: CredentialGatewayBacked, Source: RuntimeSourceGateway, Default: true,
		DefaultModel: "gateway",
		Models: []RuntimeModelOption{
			{Alias: "gateway", Source: RuntimeSourceGateway, Credential: CredentialGatewayBacked, Target: runtimeModel("gateway-target", model.EffortHigh), DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortHigh}},
			{Alias: "native-first", Source: RuntimeSourceNative, Credential: CredentialNativeAuth, Target: runtimeModel("native-first-target", model.EffortMedium), DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium}},
			{Alias: "native-second", Source: RuntimeSourceNative, Credential: CredentialNativeAuth, Target: runtimeModel("native-second-target", model.EffortLow), DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow}},
		},
	}})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	resolved, err := catalog.ResolveWithExplicitSource("worker", "codex", RuntimeSourceNative, "", model.EffortNone, false)
	if err != nil {
		t.Fatalf("ResolveWithExplicitSource(native, omitted alias) error = %v", err)
	}
	if resolved.Source != RuntimeSourceNative || resolved.SelectionKind != RuntimeSelectionExplicit || resolved.ModelAlias != "native-first" {
		t.Fatalf("resolved runtime = %+v, want native/explicit/native-first", resolved)
	}
	if resolved.Target.Name != "native-first-target" {
		t.Fatalf("resolved target = %q, want native-first-target", resolved.Target.Name)
	}

	legacy, err := catalog.Resolve("worker", "codex", "", model.EffortNone)
	if err != nil {
		t.Fatalf("Resolve(omitted source) error = %v", err)
	}
	if legacy.Source != RuntimeSourceGateway || legacy.ModelAlias != "gateway" {
		t.Fatalf("Resolve(omitted source) = %+v, want gateway/gateway", legacy)
	}
}

func TestRuntimeCatalogRejectsDefaultModelFromDifferentSource(t *testing.T) {
	t.Parallel()

	_, err := NewRuntimeCatalog([]RuntimeCatalogEntry{{
		SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex-mixed",
		Credential: CredentialGatewayBacked, Source: RuntimeSourceGateway, Default: true,
		DefaultModel: "native",
		Models: []RuntimeModelOption{
			{Alias: "gateway", Source: RuntimeSourceGateway, Credential: CredentialGatewayBacked, Target: runtimeModel("gateway-target", model.EffortHigh), DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortHigh}},
			{Alias: "native", Source: RuntimeSourceNative, Credential: CredentialNativeAuth, Target: runtimeModel("native-target", model.EffortMedium), DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium}},
		},
	}})
	var catalogErr *RuntimeCatalogError
	if !errors.As(err, &catalogErr) || catalogErr.Kind != RuntimeCatalogInvalidDefaultModel {
		t.Fatalf("NewRuntimeCatalog() error = %v, want %s", err, RuntimeCatalogInvalidDefaultModel)
	}
}

func TestRuntimeCatalogExplicitSourceDoesNotFallbackToManagedDefault(t *testing.T) {
	t.Parallel()

	catalog, err := NewRuntimeCatalog([]RuntimeCatalogEntry{{
		SubagentType:  "worker",
		AgentHarness:  "codex",
		Profile:       "acp/codex-native",
		Credential:    CredentialNativeAuth,
		Source:        RuntimeSourceNative,
		SelectionKind: RuntimeSelectionHarnessManaged,
		Default:       true,
	}})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	tests := []struct {
		name    string
		resolve func() (Resolved, error)
	}{
		{
			name: "model resolution",
			resolve: func() (Resolved, error) {
				return catalog.ResolveWithExplicitSource("worker", "codex", RuntimeSourceGateway, "", model.EffortNone, false)
			},
		},
		{
			name: "target resolution",
			resolve: func() (Resolved, error) {
				return catalog.ResolveTargetAliasWithSource("worker", "codex", RuntimeSourceGateway, "", model.EffortNone)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := tt.resolve()
			if !reflect.DeepEqual(resolved, Resolved{}) {
				t.Fatalf("resolved runtime = %+v, want zero value", resolved)
			}
			var catalogErr *RuntimeCatalogError
			if !errors.As(err, &catalogErr) || catalogErr.Kind != RuntimeCatalogUnknownSource {
				t.Fatalf("error = %v, want %s", err, RuntimeCatalogUnknownSource)
			}
		})
	}
}

func TestRuntimeCatalogExplicitSourcePrefersExactEntryForModelAndTargetResolution(t *testing.T) {
	t.Parallel()

	catalog, err := NewRuntimeCatalog([]RuntimeCatalogEntry{
		{
			SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex-mixed",
			Credential: CredentialGatewayBacked, Source: RuntimeSourceGateway, Default: true,
			DefaultModel: "gateway-model",
			Models: []RuntimeModelOption{
				{Alias: "gateway-model", Source: RuntimeSourceGateway, Credential: CredentialGatewayBacked, Target: runtimeModel("gateway-target", model.EffortMedium), DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium}},
				{Alias: "native-mixed", Source: RuntimeSourceNative, Credential: CredentialNativeAuth, Target: runtimeModel("mixed-native-target", model.EffortLow), DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow}},
			},
		},
		{
			SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex-native",
			Credential: CredentialNativeAuth, Source: RuntimeSourceNative,
			DefaultModel: "native-dedicated",
			Models: []RuntimeModelOption{{
				Alias: "native-dedicated", Source: RuntimeSourceNative, Credential: CredentialNativeAuth,
				Target:        runtimeModel("dedicated-native-target", model.EffortHigh),
				DefaultEffort: model.EffortHigh, Efforts: []model.Effort{model.EffortHigh},
			}},
		},
	})
	if err != nil {
		t.Fatalf("NewRuntimeCatalog() error = %v", err)
	}

	resolved, err := catalog.ResolveWithExplicitSource("worker", "codex", RuntimeSourceNative, "", model.EffortNone, false)
	if err != nil {
		t.Fatalf("ResolveWithExplicitSource() error = %v", err)
	}
	if resolved.Profile != "acp/codex-native" || resolved.ModelAlias != "native-dedicated" || resolved.Target.Name != "dedicated-native-target" {
		t.Fatalf("resolved runtime = %+v, want dedicated native entry", resolved)
	}

	resolved, err = catalog.ResolveTargetAliasWithSource("worker", "codex", RuntimeSourceNative, "native-dedicated", model.EffortHigh)
	if err != nil {
		t.Fatalf("ResolveTargetAliasWithSource() error = %v", err)
	}
	if resolved.Profile != "acp/codex-native" || resolved.ModelAlias != "native-dedicated" || resolved.Target.Name != "dedicated-native-target" {
		t.Fatalf("target-resolved runtime = %+v, want dedicated native entry", resolved)
	}
}

func TestRuntimeCatalogRejectsInvalidSelectionMatrix(t *testing.T) {
	t.Parallel()

	managed := func() RuntimeCatalogEntry {
		return RuntimeCatalogEntry{SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex", Credential: CredentialNativeAuth, Source: RuntimeSourceNative, SelectionKind: RuntimeSelectionHarnessManaged, Default: true}
	}
	tests := []struct {
		name  string
		entry RuntimeCatalogEntry
		want  RuntimeCatalogErrorKind
	}{
		{name: "gateway managed", entry: RuntimeCatalogEntry{SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex", Credential: CredentialGatewayBacked, Source: RuntimeSourceGateway, SelectionKind: RuntimeSelectionHarnessManaged, Default: true}, want: RuntimeCatalogInvalidSelectionKind},
		{name: "explicit model-less native", entry: RuntimeCatalogEntry{SubagentType: "worker", AgentHarness: "codex", Profile: "acp/codex", Credential: CredentialNativeAuth, Source: RuntimeSourceNative, SelectionKind: RuntimeSelectionExplicit, Default: true}, want: RuntimeCatalogMissingDefaultModel},
		{name: "managed with model", entry: func() RuntimeCatalogEntry {
			entry := managed()
			entry.Models = []RuntimeModelOption{{Alias: "model", Target: runtimeModel("model", model.EffortMedium), DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium}}}
			return entry
		}(), want: RuntimeCatalogInvalidModel},
		{name: "managed with small model", entry: func() RuntimeCatalogEntry { entry := managed(); entry.SmallModel = "small"; return entry }(), want: RuntimeCatalogInvalidModel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewRuntimeCatalog([]RuntimeCatalogEntry{tt.entry})
			var catalogErr *RuntimeCatalogError
			if !errors.As(err, &catalogErr) || catalogErr.Kind != tt.want {
				t.Fatalf("NewRuntimeCatalog() error = %v, want %s", err, tt.want)
			}
		})
	}
}

func testCatalogEntries() []RuntimeCatalogEntry {
	return []RuntimeCatalogEntry{
		{
			SubagentType: "worker", AgentHarness: "codex", Profile: "codex-profile",
			Credential: CredentialGatewayBacked, Default: false, DefaultModel: "o3", SmallModel: "o3",
			Models: []RuntimeModelOption{{Alias: "o3", Target: runtimeModel("o3-target", model.EffortLow), DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow, model.EffortHigh}}},
		},
		{
			SubagentType: "worker", AgentHarness: "claude-code", Profile: "claude-profile",
			Credential: CredentialGatewayBacked, Default: true, DefaultModel: "sonnet", SmallModel: "sonnet-small",
			Models: []RuntimeModelOption{
				{Alias: "sonnet", Target: runtimeModel("sonnet-target", model.EffortMedium), DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium, model.EffortHigh}},
				{Alias: "sonnet-small", Target: runtimeModel("sonnet-small-target", model.EffortLow), DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow}},
			},
		},
	}
}

func runtimeModel(name string, effort model.Effort) model.Model {
	return model.Model{
		Provider: model.ProviderName("provider"),
		Name:     name,
		Sampling: model.Sampling{Effort: effort},
	}
}
