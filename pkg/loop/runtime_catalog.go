package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/identity"
	model "github.com/looprig/inference/model"
)

// AgentHarnessName is the stable, model-facing alias of a child execution
// harness. It is intentionally not an executable name or a connector path.
type AgentHarnessName string

// ModelAlias is the stable, harness-facing alias of a cataloged model target.
// It is not a provider model id and carries no credential or endpoint data.
type ModelAlias string

// RuntimeProfileName is the stable, secret-free key used by a backend builder
// to select the concrete runtime implementation.
type RuntimeProfileName string

// RuntimeModelOption describes one model alias admitted by one catalog entry.
type RuntimeModelOption struct {
	Alias ModelAlias
	// Credential optionally overrides the entry credential for this model.
	// It lets one harness expose its own native-auth catalogue alongside
	// product-owned gateway targets while each resolved child still has one
	// immutable credential mode. Empty inherits RuntimeCatalogEntry.Credential.
	Credential CredentialMode
	// NativeSmallModel is the connector-native small-model identifier used by
	// native-auth runtimes. It is intentionally bounded and secret-free.
	NativeSmallModel string
	Target           model.Model
	DefaultEffort    model.Effort
	Efforts          []model.Effort
}

// RuntimeCatalogEntry describes one role/harness runtime combination.
type RuntimeCatalogEntry struct {
	SubagentType    identity.AgentName
	AgentHarness    AgentHarnessName
	Profile         RuntimeProfileName
	Credential      CredentialMode
	Default         bool
	DefaultModel    ModelAlias
	SmallModel      ModelAlias
	NeedsSmallModel bool
	Models          []RuntimeModelOption
}

// Resolved is the immutable runtime tuple selected from a RuntimeCatalog.
// Target is a defensive copy of the cataloged model descriptor.
type Resolved struct {
	SubagentType identity.AgentName
	AgentHarness AgentHarnessName
	Profile      RuntimeProfileName
	Credential   CredentialMode
	// ModelAlias is the stable model-facing selector accepted by Subagent.
	ModelAlias ModelAlias
	// TargetAlias is the concrete alias sent to a gateway or ACP launcher. It
	// is derived from the selected effort for gateway-backed runtimes and is
	// always bare for native-auth runtimes.
	TargetAlias      ModelAlias
	NativeSmallModel string
	SmallModel       ModelAlias
	Target           model.Model
	Effort           model.Effort
}

// RuntimeCatalogErrorKind identifies a catalog construction or resolution
// failure. Error messages contain categories and fields, not untrusted values.
type RuntimeCatalogErrorKind string

const (
	RuntimeCatalogInvalidCredential    RuntimeCatalogErrorKind = "invalid_credential" // #nosec G101 -- bounded error category, not a credential
	RuntimeCatalogInvalidIdentifier    RuntimeCatalogErrorKind = "invalid_identifier"
	RuntimeCatalogInvalidModel         RuntimeCatalogErrorKind = "invalid_model"
	RuntimeCatalogMissingDefaultModel  RuntimeCatalogErrorKind = "missing_default_model"
	RuntimeCatalogInvalidDefaultModel  RuntimeCatalogErrorKind = "invalid_default_model"
	RuntimeCatalogDuplicateAlias       RuntimeCatalogErrorKind = "duplicate_alias"
	RuntimeCatalogDuplicateHarness     RuntimeCatalogErrorKind = "duplicate_harness"
	RuntimeCatalogDefaultHarnessCount  RuntimeCatalogErrorKind = "default_harness_count"
	RuntimeCatalogInvalidEffort        RuntimeCatalogErrorKind = "invalid_effort"
	RuntimeCatalogDuplicateEffort      RuntimeCatalogErrorKind = "duplicate_effort"
	RuntimeCatalogInvalidDefaultEffort RuntimeCatalogErrorKind = "invalid_default_effort"
	RuntimeCatalogInvalidSmallModel    RuntimeCatalogErrorKind = "invalid_small_model"
	RuntimeCatalogNativeAliasConflict  RuntimeCatalogErrorKind = "native_alias_conflict"
	RuntimeCatalogDerivedAliasConflict RuntimeCatalogErrorKind = "derived_alias_conflict"
	RuntimeCatalogUnknownAgent         RuntimeCatalogErrorKind = "unknown_agent"
	RuntimeCatalogUnknownHarness       RuntimeCatalogErrorKind = "unknown_harness"
	RuntimeCatalogUnknownModel         RuntimeCatalogErrorKind = "unknown_model"
	RuntimeCatalogIncompatibleEffort   RuntimeCatalogErrorKind = "incompatible_effort"
)

// RuntimeCatalogError reports a closed, deterministic catalog failure.
type RuntimeCatalogError struct {
	Kind  RuntimeCatalogErrorKind
	Field string
}

func (e *RuntimeCatalogError) Error() string {
	message := "loop: runtime catalog: " + string(e.Kind)
	if e.Field != "" {
		message += " (" + e.Field + ")"
	}
	return message
}

// RuntimeCatalog is an immutable, parent-scoped set of permitted child
// runtimes. Its fields are intentionally private; all returned nested values
// are defensive copies.
type RuntimeCatalog struct {
	entries []RuntimeCatalogEntry
	digest  string
}

// NewRuntimeCatalog validates, sorts, and defensively copies entries into an
// immutable catalog. An empty catalog is valid and represents a parent with no
// optional runtime entries.
func NewRuntimeCatalog(entries []RuntimeCatalogEntry) (RuntimeCatalog, error) {
	cloned := make([]RuntimeCatalogEntry, len(entries))
	defaultCounts := make(map[identity.AgentName]int)
	seenHarnesses := make(map[runtimeHarnessKey]struct{}, len(entries))

	for i, source := range entries {
		entry := cloneRuntimeCatalogEntry(source)
		if err := validateRuntimeCatalogEntry(entry); err != nil {
			return RuntimeCatalog{}, err
		}
		key := runtimeHarnessKey{agent: entry.SubagentType, harness: entry.AgentHarness}
		if _, exists := seenHarnesses[key]; exists {
			return RuntimeCatalog{}, &RuntimeCatalogError{Kind: RuntimeCatalogDuplicateHarness, Field: "AgentHarness"}
		}
		seenHarnesses[key] = struct{}{}
		if entry.Default {
			defaultCounts[entry.SubagentType]++
		}
		cloned[i] = entry
	}

	for _, entry := range cloned {
		if defaultCounts[entry.SubagentType] != 1 {
			return RuntimeCatalog{}, &RuntimeCatalogError{Kind: RuntimeCatalogDefaultHarnessCount, Field: "Default"}
		}
	}
	if err := validateNativeAliasOwnership(cloned); err != nil {
		return RuntimeCatalog{}, err
	}
	if err := validateDerivedAliasOwnership(cloned); err != nil {
		return RuntimeCatalog{}, err
	}

	sort.Slice(cloned, func(i, j int) bool {
		left, right := cloned[i], cloned[j]
		if left.SubagentType != right.SubagentType {
			return left.SubagentType < right.SubagentType
		}
		if left.AgentHarness != right.AgentHarness {
			return left.AgentHarness < right.AgentHarness
		}
		return left.Profile < right.Profile
	})
	for i := range cloned {
		sort.Slice(cloned[i].Models, func(left, right int) bool {
			return cloned[i].Models[left].Alias < cloned[i].Models[right].Alias
		})
	}

	catalog := RuntimeCatalog{entries: cloned}
	catalog.digest = digestRuntimeCatalog(cloned)
	return catalog, nil
}

// EntriesFor returns the sorted entries admitted for agent. The returned
// entries and all nested values are independent copies. Unknown agents return
// nil rather than a partial or global catalog.
func (c RuntimeCatalog) EntriesFor(agent identity.AgentName) []RuntimeCatalogEntry {
	if agent == "" {
		return nil
	}
	var result []RuntimeCatalogEntry
	for _, entry := range c.entries {
		if entry.SubagentType == agent {
			result = append(result, cloneRuntimeCatalogEntry(entry))
		}
	}
	return result
}

// HasEntries reports whether this parent has any optional runtime choices.
// It is intentionally a narrow query so native/no-choice parents can preserve
// the absence of an adapter runtime without exposing the catalog backing slice.
func (c RuntimeCatalog) HasEntries() bool { return len(c.entries) != 0 }

// Resolve selects a runtime tuple for agent. Empty harness, alias, and effort
// selectors use the deterministic default at that level. Explicit selectors
// are checked only within the already-selected parent-scoped entry; no global
// or format fallback is ever attempted. Its zero effort retains the legacy
// meaning of an omitted effort selector.
func (c RuntimeCatalog) Resolve(agent identity.AgentName, harness AgentHarnessName, alias ModelAlias, effort model.Effort) (Resolved, error) {
	return c.ResolveWithExplicitEffort(agent, harness, alias, effort, false)
}

// ResolveWithExplicitEffort selects a runtime tuple while preserving whether
// the caller supplied the effort selector. This distinction matters because
// EffortNone is model.Effort's zero value: omitted effort uses DefaultEffort,
// while explicit none is valid only when the model advertises none.
func (c RuntimeCatalog) ResolveWithExplicitEffort(agent identity.AgentName, harness AgentHarnessName, alias ModelAlias, effort model.Effort, explicitEffort bool) (Resolved, error) {
	if agent == "" {
		return Resolved{}, &RuntimeCatalogError{Kind: RuntimeCatalogUnknownAgent, Field: "SubagentType"}
	}

	var selected *RuntimeCatalogEntry
	for i := range c.entries {
		entry := &c.entries[i]
		if entry.SubagentType != agent {
			continue
		}
		if harness == "" {
			if entry.Default {
				selected = entry
				break
			}
			continue
		}
		if entry.AgentHarness == harness {
			selected = entry
			break
		}
	}
	if selected == nil {
		if harness == "" {
			return Resolved{}, &RuntimeCatalogError{Kind: RuntimeCatalogUnknownAgent, Field: "SubagentType"}
		}
		return Resolved{}, &RuntimeCatalogError{Kind: RuntimeCatalogUnknownHarness, Field: "AgentHarness"}
	}

	selectedModel := (*RuntimeModelOption)(nil)
	for i := range selected.Models {
		candidate := &selected.Models[i]
		if alias == "" {
			if candidate.Alias == selected.DefaultModel {
				selectedModel = candidate
				break
			}
			continue
		}
		if candidate.Alias == alias {
			selectedModel = candidate
			break
		}
	}
	if selectedModel == nil {
		return Resolved{}, &RuntimeCatalogError{Kind: RuntimeCatalogUnknownModel, Field: "ModelAlias"}
	}

	selectedEffort := selectedModel.DefaultEffort
	if explicitEffort {
		if !containsEffort(selectedModel.Efforts, effort) {
			return Resolved{}, &RuntimeCatalogError{Kind: RuntimeCatalogIncompatibleEffort, Field: "Effort"}
		}
		selectedEffort = effort
	} else if effort != model.EffortNone {
		if !containsEffort(selectedModel.Efforts, effort) {
			return Resolved{}, &RuntimeCatalogError{Kind: RuntimeCatalogIncompatibleEffort, Field: "Effort"}
		}
		selectedEffort = effort
	}

	return Resolved{
		SubagentType:     selected.SubagentType,
		AgentHarness:     selected.AgentHarness,
		Profile:          selected.Profile,
		Credential:       effectiveModelCredential(*selected, *selectedModel),
		ModelAlias:       selectedModel.Alias,
		TargetAlias:      concreteRuntimeAlias(*selectedModel, effectiveModelCredential(*selected, *selectedModel), selectedEffort),
		NativeSmallModel: selectedModel.NativeSmallModel,
		SmallModel:       selected.SmallModel,
		Target:           selectedModel.Target.Clone(),
		Effort:           selectedEffort,
	}, nil
}

// ResolveTargetAlias resolves a durable or trusted runtime target alias back
// to its model-facing catalog selector. New gateway-backed records use the
// concrete per-effort alias; the bare model alias is also accepted so legacy
// records remain restorable. This method is intentionally not used by
// model-facing Subagent preparation or controller validation.
func (c RuntimeCatalog) ResolveTargetAlias(agent identity.AgentName, harness AgentHarnessName, targetAlias ModelAlias, effort model.Effort) (Resolved, error) {
	if agent == "" {
		return Resolved{}, &RuntimeCatalogError{Kind: RuntimeCatalogUnknownAgent, Field: "SubagentType"}
	}

	var selected *RuntimeCatalogEntry
	for i := range c.entries {
		entry := &c.entries[i]
		if entry.SubagentType != agent {
			continue
		}
		if harness == "" {
			if entry.Default {
				selected = entry
				break
			}
			continue
		}
		if entry.AgentHarness == harness {
			selected = entry
			break
		}
	}
	if selected == nil {
		if harness == "" {
			return Resolved{}, &RuntimeCatalogError{Kind: RuntimeCatalogUnknownAgent, Field: "SubagentType"}
		}
		return Resolved{}, &RuntimeCatalogError{Kind: RuntimeCatalogUnknownHarness, Field: "AgentHarness"}
	}

	for i := range selected.Models {
		option := &selected.Models[i]
		credential := effectiveModelCredential(*selected, *option)
		if targetAlias != option.Alias && targetAlias != concreteRuntimeAlias(*option, credential, effort) {
			continue
		}
		return c.ResolveWithExplicitEffort(agent, harness, option.Alias, effort, true)
	}
	return Resolved{}, &RuntimeCatalogError{Kind: RuntimeCatalogUnknownModel, Field: "ModelAlias"}
}

// Digest returns the deterministic SHA-256 identity of the catalog. The
// canonical projection includes only stable catalog and model identity data;
// raw endpoints and all credential-bearing material are deliberately omitted.
func (c RuntimeCatalog) Digest() string {
	if c.digest != "" {
		return c.digest
	}
	return digestRuntimeCatalog(c.entries)
}

type runtimeHarnessKey struct {
	agent   identity.AgentName
	harness AgentHarnessName
}

type runtimeAliasOwner struct {
	harness    AgentHarnessName
	credential CredentialMode
}

func effectiveModelCredential(entry RuntimeCatalogEntry, option RuntimeModelOption) CredentialMode {
	if option.Credential != "" {
		return option.Credential
	}
	return entry.Credential
}

func validateRuntimeCatalogEntry(entry RuntimeCatalogEntry) error {
	if err := validateCatalogIdentifier(string(entry.SubagentType), true); err != nil {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier, Field: "SubagentType"}
	}
	if err := validateCatalogIdentifier(string(entry.AgentHarness), false); err != nil {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier, Field: "AgentHarness"}
	}
	if err := validateRuntimeProfile(string(entry.Profile)); err != nil {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier, Field: "Profile"}
	}
	if entry.Credential != CredentialGatewayBacked && entry.Credential != CredentialNativeAuth {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidCredential, Field: "Credential"}
	}
	if len(entry.Models) == 0 {
		return &RuntimeCatalogError{Kind: RuntimeCatalogMissingDefaultModel, Field: "Models"}
	}
	if entry.DefaultModel == "" {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidDefaultModel, Field: "DefaultModel"}
	}
	if err := validateCatalogIdentifier(string(entry.DefaultModel), false); err != nil {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier, Field: "DefaultModel"}
	}
	if entry.SmallModel != "" {
		if err := validateCatalogIdentifier(string(entry.SmallModel), false); err != nil {
			return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier, Field: "SmallModel"}
		}
	}

	aliases := make(map[ModelAlias]struct{}, len(entry.Models))
	defaultModelFound := false
	if entry.NeedsSmallModel && entry.SmallModel == "" {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidSmallModel, Field: "SmallModel"}
	}
	smallModelFound := entry.SmallModel == ""
	for i := range entry.Models {
		option := &entry.Models[i]
		if err := validateCatalogIdentifier(string(option.Alias), false); err != nil {
			return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier, Field: "Models.Alias"}
		}
		if option.Credential != "" && option.Credential != CredentialGatewayBacked && option.Credential != CredentialNativeAuth {
			return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidCredential, Field: "Models.Credential"}
		}
		if option.NativeSmallModel != "" {
			if err := validateCatalogIdentifier(option.NativeSmallModel, false); err != nil {
				return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier, Field: "Models.NativeSmallModel"}
			}
		}
		if _, exists := aliases[option.Alias]; exists {
			return &RuntimeCatalogError{Kind: RuntimeCatalogDuplicateAlias, Field: "Models.Alias"}
		}
		aliases[option.Alias] = struct{}{}
		if option.Alias == entry.DefaultModel {
			defaultModelFound = true
		}
		if option.Alias == entry.SmallModel {
			smallModelFound = true
		}
		if err := option.Target.Validate(); err != nil {
			return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidModel, Field: "Models.Target"}
		}
		if err := option.Target.Key().Validate(); err != nil {
			return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidModel, Field: "Models.Target"}
		}
		if !option.Target.Sampling.Effort.Valid() {
			return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidModel, Field: "Models.Target.Sampling.Effort"}
		}
		if !option.DefaultEffort.Valid() {
			return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidEffort, Field: "DefaultEffort"}
		}
		efforts := make(map[model.Effort]struct{}, len(option.Efforts))
		for _, effort := range option.Efforts {
			if !effort.Valid() {
				return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidEffort, Field: "Efforts"}
			}
			if _, exists := efforts[effort]; exists {
				return &RuntimeCatalogError{Kind: RuntimeCatalogDuplicateEffort, Field: "Efforts"}
			}
			efforts[effort] = struct{}{}
		}
		if len(option.Efforts) > 0 {
			if _, advertised := efforts[option.DefaultEffort]; !advertised {
				return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidDefaultEffort, Field: "DefaultEffort"}
			}
		}
		sort.Slice(option.Efforts, func(left, right int) bool {
			return effortRank(option.Efforts[left]) < effortRank(option.Efforts[right])
		})
	}
	if !defaultModelFound {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidDefaultModel, Field: "DefaultModel"}
	}
	if entry.NeedsSmallModel && !smallModelFound {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidSmallModel, Field: "SmallModel"}
	}
	return nil
}

func validateNativeAliasOwnership(entries []RuntimeCatalogEntry) error {
	owners := make(map[ModelAlias][]runtimeAliasOwner)
	for _, entry := range entries {
		for _, option := range entry.Models {
			owners[option.Alias] = append(owners[option.Alias], runtimeAliasOwner{
				harness: entry.AgentHarness, credential: effectiveModelCredential(entry, option),
			})
		}
		if entry.SmallModel != "" {
			owners[entry.SmallModel] = append(owners[entry.SmallModel], runtimeAliasOwner{
				harness: entry.AgentHarness, credential: entry.Credential,
			})
		}
	}
	for _, aliasOwners := range owners {
		for i := range aliasOwners {
			for j := i + 1; j < len(aliasOwners); j++ {
				if aliasOwners[i].harness != aliasOwners[j].harness &&
					(aliasOwners[i].credential != CredentialGatewayBacked || aliasOwners[j].credential != CredentialGatewayBacked) {
					return &RuntimeCatalogError{Kind: RuntimeCatalogNativeAliasConflict, Field: "Models.Alias"}
				}
			}
		}
	}
	return nil
}

func validateDerivedAliasOwnership(entries []RuntimeCatalogEntry) error {
	configured := make(map[ModelAlias]struct{})
	derived := make(map[ModelAlias]struct{})
	for _, entry := range entries {
		for _, option := range entry.Models {
			configured[option.Alias] = struct{}{}
			if effectiveModelCredential(entry, option) != CredentialGatewayBacked {
				continue
			}
			for _, effort := range option.Efforts {
				if effort == option.DefaultEffort {
					continue
				}
				alias := concreteRuntimeAlias(option, CredentialGatewayBacked, effort)
				if err := validateCatalogIdentifier(string(alias), false); err != nil {
					return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier, Field: "Models.TargetAlias"}
				}
				derived[alias] = struct{}{}
			}
		}
	}
	for alias := range derived {
		if _, exists := configured[alias]; exists {
			return &RuntimeCatalogError{Kind: RuntimeCatalogDerivedAliasConflict, Field: "Models.Alias"}
		}
	}
	return nil
}

func concreteRuntimeAlias(option RuntimeModelOption, credential CredentialMode, effort model.Effort) ModelAlias {
	if credential == CredentialNativeAuth || effort == option.DefaultEffort {
		return option.Alias
	}
	return ModelAlias(string(option.Alias) + "@" + catalogEffortString(effort))
}

func validateCatalogIdentifier(value string, allowInternalSpaces bool) error {
	const maxIdentifierBytes = 128
	if value == "" || len(value) > maxIdentifierBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier}
	}
	for _, r := range value {
		if r == '/' || r == '\\' || r == ':' || r == 0 || unicode.IsControl(r) || (!allowInternalSpaces && unicode.IsSpace(r)) {
			return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier}
		}
	}
	return nil
}

func validateRuntimeProfile(value string) error {
	const maxIdentifierBytes = 128
	if value == "" || len(value) > maxIdentifierBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier}
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier}
		}
		if err := validateCatalogIdentifier(segment, false); err != nil {
			return &RuntimeCatalogError{Kind: RuntimeCatalogInvalidIdentifier}
		}
	}
	return nil
}

func cloneRuntimeCatalogEntry(entry RuntimeCatalogEntry) RuntimeCatalogEntry {
	models := entry.Models
	entry.Models = make([]RuntimeModelOption, len(models))
	for i, option := range models {
		option.Target = option.Target.Clone()
		option.Efforts = append([]model.Effort(nil), option.Efforts...)
		entry.Models[i] = option
	}
	return entry
}

func containsEffort(efforts []model.Effort, wanted model.Effort) bool {
	for _, effort := range efforts {
		if effort == wanted {
			return true
		}
	}
	return false
}

func effortRank(effort model.Effort) int {
	switch effort {
	case model.EffortNone:
		return 0
	case model.EffortLow:
		return 1
	case model.EffortMedium:
		return 2
	case model.EffortHigh:
		return 3
	case model.EffortMax:
		return 4
	default:
		return 5
	}
}

type runtimeCatalogDigest struct {
	Entries []runtimeCatalogEntryDigest `json:"entries"`
}

type runtimeCatalogEntryDigest struct {
	SubagentType    string                     `json:"subagent_type"`
	AgentHarness    string                     `json:"agent_harness"`
	Profile         string                     `json:"profile"`
	Credential      CredentialMode             `json:"credential"`
	Default         bool                       `json:"default"`
	DefaultModel    string                     `json:"default_model"`
	SmallModel      string                     `json:"small_model,omitempty"`
	NeedsSmallModel bool                       `json:"needs_small_model,omitempty"`
	Models          []runtimeModelOptionDigest `json:"models"`
}

type runtimeModelOptionDigest struct {
	Alias            string              `json:"alias"`
	Credential       CredentialMode      `json:"credential,omitempty"`
	NativeSmallModel string              `json:"native_small_model,omitempty"`
	Provider         string              `json:"provider"`
	APIFormat        string              `json:"api_format"`
	Name             string              `json:"name"`
	Origin           model.Origin        `json:"origin"`
	Capabilities     model.Capabilities  `json:"capabilities"`
	Limits           model.ContextLimits `json:"limits"`
	DefaultEffort    string              `json:"default_effort"`
	Efforts          []string            `json:"efforts"`
	Temperature      *float64            `json:"temperature,omitempty"`
	TopP             *float64            `json:"top_p,omitempty"`
	MaxTokens        *int                `json:"max_tokens,omitempty"`
	Stop             []string            `json:"stop,omitempty"`
	SamplingEffort   string              `json:"sampling_effort"`
}

func digestRuntimeCatalog(entries []RuntimeCatalogEntry) string {
	projection := runtimeCatalogDigest{Entries: make([]runtimeCatalogEntryDigest, len(entries))}
	for i, entry := range entries {
		row := runtimeCatalogEntryDigest{
			SubagentType:    string(entry.SubagentType),
			AgentHarness:    string(entry.AgentHarness),
			Profile:         string(entry.Profile),
			Credential:      entry.Credential,
			Default:         entry.Default,
			DefaultModel:    string(entry.DefaultModel),
			SmallModel:      string(entry.SmallModel),
			NeedsSmallModel: entry.NeedsSmallModel,
			Models:          make([]runtimeModelOptionDigest, len(entry.Models)),
		}
		for j, option := range entry.Models {
			row.Models[j] = runtimeModelOptionDigest{
				Alias:            string(option.Alias),
				Credential:       option.Credential,
				NativeSmallModel: option.NativeSmallModel,
				Provider:         string(option.Target.Provider),
				APIFormat:        string(option.Target.APIFormat),
				Name:             option.Target.Name,
				Origin:           option.Target.Origin,
				Capabilities:     option.Target.Caps,
				Limits:           option.Target.Limits,
				DefaultEffort:    catalogEffortString(option.DefaultEffort),
				Efforts:          catalogEffortStrings(option.Efforts),
				Temperature:      option.Target.Sampling.Temperature,
				TopP:             option.Target.Sampling.TopP,
				MaxTokens:        option.Target.Sampling.MaxTokens,
				Stop:             append([]string(nil), option.Target.Sampling.Stop...),
				SamplingEffort:   catalogEffortString(option.Target.Sampling.Effort),
			}
		}
		projection.Entries[i] = row
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		// The projection consists only of fixed JSON values. Keep Digest total if
		// a future field accidentally violates that contract.
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func catalogEffortString(effort model.Effort) string {
	if effort == model.EffortNone {
		return "none"
	}
	return string(effort)
}

func catalogEffortStrings(efforts []model.Effort) []string {
	result := make([]string, len(efforts))
	for i, effort := range efforts {
		result[i] = catalogEffortString(effort)
	}
	return result
}
