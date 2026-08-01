package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	model "github.com/looprig/inference/model"
)

// RuntimeIdentity is the secret-free runtime portion of a bound loop's
// configuration identity. The catalog digest is supplied by the composition
// root from its immutable RuntimeCatalog snapshot; raw endpoints, credentials,
// and non-identity model behavior are intentionally absent.
type RuntimeIdentity struct {
	Profile        RuntimeProfileName
	CatalogDigest  string
	ModelAlias     ModelAlias
	TargetProvider model.ProviderName
	TargetModel    string
	Effort         model.Effort
}

// Digest returns a stable SHA-256 identity for the runtime selection. The zero
// identity returns empty so native callers retain the additive legacy shape.
// The composition root's session fingerprint builder is the integration point:
// it should carry this opaque digest as its runtime-identity revision rather
// than hashing a model descriptor, endpoint, or credential.
func (i RuntimeIdentity) Digest() string {
	if i.Profile == "" && i.CatalogDigest == "" && i.ModelAlias == "" && i.TargetProvider == "" && i.TargetModel == "" && i.Effort == model.EffortNone {
		return ""
	}
	projection := struct {
		Domain         string             `json:"domain"`
		Profile        RuntimeProfileName `json:"profile,omitempty"`
		CatalogDigest  string             `json:"catalog_digest,omitempty"`
		ModelAlias     ModelAlias         `json:"model_alias,omitempty"`
		TargetProvider model.ProviderName `json:"target_provider,omitempty"`
		TargetModel    string             `json:"target_model,omitempty"`
		Effort         string             `json:"effort"`
	}{
		Domain:         "loop/runtime-identity/v1",
		Profile:        i.Profile,
		CatalogDigest:  i.CatalogDigest,
		ModelAlias:     i.ModelAlias,
		TargetProvider: i.TargetProvider,
		TargetModel:    i.TargetModel,
		Effort:         runtimeEffortString(i.Effort),
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// SelectBoundMode returns a private bound view whose default accessors resolve the
// selected effective mode. It retains every declared mode for later trusted changes.
func SelectBoundMode(bound BoundDefinition, mode ModeName) (BoundDefinition, error) {
	state, ok := bound.(*boundDefinitionState)
	if !ok || state == nil {
		return nil, &BindError{Kind: BindInvalidDefinition, Name: string(mode), Index: -1}
	}
	if _, exists := state.Mode(mode); !exists {
		return nil, &BindError{Kind: BindInvalidDefinition, Name: string(mode), Index: -1}
	}
	clone := *state
	definition := *state.definition
	definition.initialMode = mode
	clone.definition = &definition
	return &clone, nil
}

// OverrideBoundAccess returns a private bound view whose Access() resolves the
// given gate instead of the definition's own. It is the binding-time seam a
// composition root uses to give ONE bound loop a different combined access gate
// (for example a restricted evaluator for a reviewer role) without mutating the
// immutable definition.
//
// Authority differences between loops are expressed by the CONSUMER passing
// different evaluators — there is no harness-side attenuation, and a bound loop
// without an override always resolves its own definition's gate, never another
// loop's. A nil gate is rejected: overriding to "no gate" would silently turn a
// gated loop into a fail-closed-only loop through a side door; configure the
// definition without WithAccessGate instead.
func OverrideBoundAccess(bound BoundDefinition, access AccessGate) (BoundDefinition, error) {
	state, ok := bound.(*boundDefinitionState)
	if !ok || state == nil {
		return nil, &BindError{Kind: BindInvalidDefinition, Index: -1}
	}
	if nilLike(access) {
		return nil, &BindError{Kind: BindInvalidAccessGate, Index: -1}
	}
	clone := *state
	clone.accessOverride = access
	return &clone, nil
}

// OverrideBoundRuntime returns a private bound view whose engine, runtime
// profile, model, and effort are replaced by an already-validated runtime
// selection. The caller MUST have resolved the selection through its
// parent-scoped RuntimeCatalog; this function does not re-consult policy.
// Every bound mode receives the same model and effort so a later mode
// selection cannot silently un-pin the selected runtime tuple.
func OverrideBoundRuntime(bound BoundDefinition, profile RuntimeProfileName, target model.Model, effort model.Effort) (BoundDefinition, error) {
	return OverrideBoundRuntimeSelection(bound, profile, "", target, effort)
}

// OverrideBoundRuntimeSelection is the binding-time seam for a resolved
// runtime tuple. alias is optional only for compatibility with callers using
// OverrideBoundRuntime; non-empty aliases use the catalog's identifier rules.
func OverrideBoundRuntimeSelection(bound BoundDefinition, profile RuntimeProfileName, alias ModelAlias, target model.Model, effort model.Effort) (BoundDefinition, error) {
	state, ok := bound.(*boundDefinitionState)
	if !ok || state == nil {
		return nil, &BindError{Kind: BindInvalidDefinition, Index: -1}
	}
	if validateRuntimeProfile(string(profile)) != nil {
		return nil, &BindError{Kind: BindInvalidRuntime, Index: -1}
	}
	if alias != "" && validateCatalogIdentifier(string(alias), false) != nil {
		return nil, &BindError{Kind: BindInvalidRuntime, Index: -1}
	}
	if zeroModel(target) {
		return nil, &BindError{Kind: BindInvalidRuntime, Index: -1}
	}
	if err := target.Validate(); err != nil {
		return nil, &BindError{Kind: BindInvalidRuntime, Index: -1, Cause: err}
	}
	if err := target.Key().Validate(); err != nil {
		return nil, &BindError{Kind: BindInvalidRuntime, Index: -1, Cause: err}
	}
	if !target.Sampling.Effort.Valid() || !effort.Valid() {
		return nil, &BindError{Kind: BindInvalidRuntime, Index: -1}
	}

	clone := *state
	definition := *state.definition
	definition.engine = EngineAdapter
	definition.model = cloneModel(target)
	clone.definition = &definition
	clone.runtimeProfile = profile
	clone.runtimeModelAlias = alias
	clone.runtimeTargetProvider = target.Provider
	clone.runtimeTargetModel = target.Name
	clone.runtimeEffort = effort
	clone.modes = make([]BoundMode, len(state.modes))
	for index, mode := range state.modes {
		pinned := cloneBoundMode(mode)
		pinned.Model = cloneModel(target)
		pinned.Model.Sampling.Effort = effort
		pinned.Effort = effort
		clone.modes[index] = pinned
	}
	return &clone, nil
}

func runtimeEffortString(effort model.Effort) string {
	if effort == model.EffortNone {
		return "none"
	}
	return string(effort)
}

// OverrideBoundRuntimeCatalog records the immutable catalog snapshot used to
// authorize a bound runtime. It changes only the runtime identity and leaves
// the selected runtime tuple and all loop behavior untouched.
func OverrideBoundRuntimeCatalog(bound BoundDefinition, catalog RuntimeCatalog) (BoundDefinition, error) {
	state, ok := bound.(*boundDefinitionState)
	if !ok || state == nil {
		return nil, &BindError{Kind: BindInvalidDefinition, Index: -1}
	}
	digest := catalog.Digest()
	if len(digest) != sha256.Size*2 {
		return nil, &BindError{Kind: BindInvalidRuntime, Index: -1}
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, &BindError{Kind: BindInvalidRuntime, Index: -1}
	}
	clone := *state
	clone.runtimeCatalogDigest = digest
	return &clone, nil
}
