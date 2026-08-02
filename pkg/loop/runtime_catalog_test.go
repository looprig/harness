package loop

import (
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
				Credential: CredentialGatewayBacked, ModelAlias: "sonnet", SmallModel: "sonnet-small",
				Target: runtimeModel("sonnet-target", model.EffortMedium), Effort: model.EffortMedium,
			},
		},
		{
			name: "explicit complete tuple", harness: "codex", alias: "o3", effort: model.EffortHigh,
			want: Resolved{
				SubagentType: "worker", AgentHarness: "codex", Profile: "codex-profile",
				Credential: CredentialGatewayBacked, ModelAlias: "o3", SmallModel: "o3",
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
