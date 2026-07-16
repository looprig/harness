package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

const defaultDrainTimeout = 5 * time.Second

// Option contributes immutable loop-definition data.
type Option func(*definitionOptions) error

// Definition is a concrete immutable loop definition. Its zero value is invalid.
type Definition struct{ state *definitionState }

// InitialFingerprint is the immutable, bind-free view needed by a rig to stamp and
// compare compatibility before any runtime factories execute.
type InitialFingerprint struct {
	Model           model.Model
	EffectiveSystem string
	ToolNames       []string
}

type definitionState struct {
	name                identity.AgentName
	displayName         string
	description         string
	client              inference.Client
	model               model.Model
	system              string
	tools               []tool.Definition
	permissionFactory   PermissionFactory
	middlewares         []tool.ToolMiddleware
	limits              ToolLimits
	engine              Engine
	drainTimeout        time.Duration
	runtimeContext      RuntimeContextProvider
	delegates           []identity.AgentName
	delegation          Delegation
	modes               []Mode
	initialMode         ModeName
	policyRevision      string
	contextCounter      contextcount.ContextCounter
	counterCapability   contextcount.CounterCapability
	inferenceCapability contextcount.InferenceCapability
	contextObservation  ContextObservationPolicy
	compaction          CompactionPolicy
}

type definitionOptions struct {
	definitionState
	seen map[string]struct{}
}

func (o *definitionOptions) singleton(name string) error {
	if _, exists := o.seen[name]; exists {
		return &DefinitionError{Kind: DefinitionDuplicateOption, Field: name}
	}
	o.seen[name] = struct{}{}
	return nil
}

// Define validates and freezes one loop definition.
func Define(opts ...Option) (Definition, error) {
	resolved := &definitionOptions{seen: make(map[string]struct{})}
	for index, opt := range opts {
		if opt == nil {
			return Definition{}, &DefinitionError{Kind: DefinitionNilOption, Field: "options", Value: indexString(index)}
		}
		if err := opt(resolved); err != nil {
			return Definition{}, err
		}
	}
	if strings.TrimSpace(string(resolved.name)) == "" {
		return Definition{}, &DefinitionError{Kind: DefinitionMissingName, Field: "name"}
	}
	if nilLike(resolved.client) {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidClient, Field: "client"}
	}
	if err := resolved.model.Validate(); err != nil {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidModel, Field: "model", Cause: err}
	}
	if err := resolved.model.Key().Validate(); err != nil {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidModel, Field: "model", Cause: err}
	}
	if !resolved.model.Sampling.Effort.Valid() {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidModel, Field: "model.sampling.effort"}
	}
	if invalidLimits(resolved.limits) {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidToolLimits, Field: "tool_limits"}
	}
	if resolved.drainTimeout < 0 {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidDrainTimeout, Field: "drain_timeout"}
	}
	for index, middleware := range resolved.middlewares {
		if middleware == nil {
			return Definition{}, &DefinitionError{Kind: DefinitionInvalidMiddleware, Field: "middlewares", Value: indexString(index)}
		}
	}
	if _, configured := resolved.seen["permission_factory"]; configured && nilLike(resolved.permissionFactory) {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidPermission, Field: "permission_factory"}
	}
	if resolved.engine != EngineNative && resolved.engine != EngineForeignClaude && resolved.engine != EngineForeignCodex {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidEngine, Field: "engine"}
	}
	if _, configured := resolved.seen["runtime_context"]; configured && nilLike(resolved.runtimeContext) {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidRuntimeContext, Field: "runtime_context"}
	}
	if _, configured := resolved.seen["policy_revision"]; configured && strings.TrimSpace(resolved.policyRevision) == "" {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidPolicyRevision, Field: "policy_revision"}
	}
	_, permissionConfigured := resolved.seen["permission_factory"]
	_, runtimeConfigured := resolved.seen["runtime_context"]
	if (permissionConfigured || runtimeConfigured || len(resolved.middlewares) > 0) && strings.TrimSpace(resolved.policyRevision) == "" {
		return Definition{}, &DefinitionError{Kind: DefinitionMissingPolicyRevision, Field: "policy_revision"}
	}
	if resolved.delegation.Style != DelegationSyncOnly && resolved.delegation.Style != DelegationManaged {
		return Definition{}, &DefinitionError{Kind: DefinitionInvalidDelegation, Field: "delegation.style"}
	}
	for index, delegate := range resolved.delegates {
		if strings.TrimSpace(string(delegate)) == "" {
			return Definition{}, &DefinitionError{Kind: DefinitionInvalidDelegate, Field: "delegates", Value: indexString(index)}
		}
	}
	if err := validateDefinitionTools(resolved.tools, "tools"); err != nil {
		return Definition{}, err
	}
	if err := validateModes(resolved.modes, resolved.initialMode); err != nil {
		return Definition{}, err
	}
	if err := validateContextDefinition(resolved); err != nil {
		return Definition{}, err
	}

	state := resolved.definitionState
	state.model = cloneModel(state.model)
	state.tools = append([]tool.Definition(nil), state.tools...)
	state.middlewares = append([]tool.ToolMiddleware(nil), state.middlewares...)
	state.delegates = dedupeDelegates(state.delegates)
	state.modes = cloneModes(state.modes)
	state.limits = defaultLimits(state.limits)
	if state.drainTimeout == 0 {
		state.drainTimeout = defaultDrainTimeout
	}
	return Definition{state: &state}, nil
}

func validateContextDefinition(resolved *definitionOptions) error {
	_, hasCounter := resolved.seen["context_counter"]
	_, hasCapability := resolved.seen["inference_capability"]
	_, hasObservation := resolved.seen["context_observation"]
	_, hasCompaction := resolved.seen["compaction"]
	if hasObservation && hasCompaction {
		return &DefinitionError{Kind: DefinitionConflictingContextPolicy, Field: "context_policy"}
	}
	if (hasCapability || hasObservation || hasCompaction) && !hasCounter {
		return &DefinitionError{Kind: DefinitionMissingContextCounter, Field: "context_counter"}
	}
	if hasCounter && !hasCapability {
		return &DefinitionError{Kind: DefinitionMissingInferenceCapability, Field: "inference_capability"}
	}
	if !hasCounter {
		return nil
	}
	if !hasObservation && !hasCompaction {
		return &DefinitionError{Kind: DefinitionMissingContextPolicy, Field: "context_policy"}
	}
	if nilLike(resolved.contextCounter) {
		return &DefinitionError{Kind: DefinitionInvalidContextCounter, Field: "context_counter"}
	}
	capability := resolved.contextCounter.CounterCapability()
	if err := capability.Validate(); err != nil {
		return &DefinitionError{Kind: DefinitionInvalidContextCounter, Field: "context_counter", Cause: err}
	}
	if err := resolved.inferenceCapability.Validate(); err != nil {
		return &DefinitionError{Kind: DefinitionInvalidInferenceCapability, Field: "inference_capability", Cause: err}
	}
	if err := contextcount.CompatibleCounter(resolved.inferenceCapability, capability); err != nil {
		return &DefinitionError{Kind: DefinitionIncompatibleContextCounter, Field: "context_counter", Cause: err}
	}
	if hasCompaction {
		if err := resolved.compaction.Validate(capability); err != nil {
			return &DefinitionError{Kind: DefinitionInvalidCompaction, Field: "compaction", Cause: err}
		}
	}
	if hasObservation {
		if err := resolved.contextObservation.Validate(capability); err != nil {
			return &DefinitionError{Kind: DefinitionInvalidContextObservation, Field: "context_observation", Cause: err}
		}
	}
	for _, mode := range resolved.modes {
		if zeroModel(mode.Model) {
			continue
		}
		if err := validateContextTransportBinding(resolved.model, mode.Model); err != nil {
			return &DefinitionError{Kind: DefinitionInvalidModeBinding, Field: "mode.model", Value: string(mode.Name), Cause: err}
		}
	}
	resolved.counterCapability = capability
	return nil
}

func indexString(index int) string { return strconv.Itoa(index) }

// Name returns the immutable attribution name used to register this definition.
func (d Definition) Name() identity.AgentName {
	if d.state == nil {
		return ""
	}
	return d.state.name
}

// Delegates returns a defensive copy of the definition's allowed delegate names.
func (d Definition) Delegates() []identity.AgentName {
	if d.state == nil {
		return nil
	}
	return append([]identity.AgentName(nil), d.state.delegates...)
}

// Modes returns defensive copies of the predeclared modes. The implicit base
// mode is available after Bind and is not included here.
func (d Definition) Modes() []Mode {
	if d.state == nil {
		return nil
	}
	return cloneModes(d.state.modes)
}

// ToolRequirements returns the union of every configured tool's Requirements across the
// base tool set and all declared modes. rig uses it to reject a workspace-requiring tool
// definition when no workspace placement is configured (the RequiresWorkspace binding could
// never be satisfied). It reads immutable design-time state, so it needs no runtime binding.
func (d Definition) ToolRequirements() tool.Requirements {
	if d.state == nil {
		return 0
	}
	var req tool.Requirements
	orAll := func(defs []tool.Definition) {
		for _, t := range defs {
			if nilLike(t) {
				continue
			}
			req |= t.Requirements()
		}
	}
	orAll(d.state.tools)
	for _, mode := range d.state.modes {
		orAll(mode.Tools)
	}
	return req
}

// InitialMode returns the explicitly selected mode, or empty for the base mode.
func (d Definition) InitialMode() ModeName {
	if d.state == nil {
		return ""
	}
	return d.state.initialMode
}

// FingerprintInitial resolves the definition's selected initial mode without building
// tools, permissions, runtime context, or any other session-specific collaborator.
func (d Definition) FingerprintInitial() InitialFingerprint {
	if d.state == nil {
		return InitialFingerprint{}
	}
	selectedModel := cloneModel(d.state.model)
	instructions := ""
	definitions := d.state.tools
	for _, mode := range d.state.modes {
		if mode.Name != d.state.initialMode {
			continue
		}
		if !zeroModel(mode.Model) {
			selectedModel = cloneModel(mode.Model)
		}
		if mode.Effort != model.EffortNone {
			selectedModel.Sampling.Effort = mode.Effort
		}
		instructions = mode.Instructions
		if len(mode.Tools) > 0 {
			definitions = mode.Tools
		}
		break
	}
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		if !nilLike(definition) {
			producedNames := definition.ProducedToolNames()
			for _, name := range producedNames {
				names = append(names, strings.TrimSpace(name))
			}
		}
	}
	return InitialFingerprint{Model: selectedModel, EffectiveSystem: EffectiveSystem(d.state.system, instructions), ToolNames: names}
}

// Delegation returns the immutable delegation policy.
func (d Definition) Delegation() Delegation {
	if d.state == nil {
		return Delegation{}
	}
	return d.state.delegation
}

// PolicyRevision returns a deterministic, secret-free digest of immutable loop
// behavior used by a rig topology fingerprint. Opaque function-valued collaborators
// require WithPolicyRevision, whose caller-supplied identity is included here.
func (d Definition) PolicyRevision() string {
	if d.state == nil {
		return ""
	}
	type toolPolicy struct {
		Name          string
		ProducedNames []string
		Requirements  tool.Requirements
	}
	type modePolicy struct {
		Name         ModeName
		Model        model.Model
		Effort       model.Effort
		Tools        []toolPolicy
		ToolLimits   ToolLimits
		Instructions string
	}
	tools := func(definitions []tool.Definition) []toolPolicy {
		out := make([]toolPolicy, 0, len(definitions))
		for _, definition := range definitions {
			if nilLike(definition) {
				out = append(out, toolPolicy{})
				continue
			}
			producedNames := definition.ProducedToolNames()
			for i := range producedNames {
				producedNames[i] = strings.TrimSpace(producedNames[i])
			}
			slices.Sort(producedNames)
			out = append(out, toolPolicy{Name: definition.Name(), ProducedNames: producedNames, Requirements: definition.Requirements()})
		}
		return out
	}
	modes := make([]modePolicy, 0, len(d.state.modes))
	for _, mode := range d.state.modes {
		modes = append(modes, modePolicy{
			Name: mode.Name, Model: cloneModel(mode.Model), Effort: mode.Effort,
			Tools: tools(mode.Tools), ToolLimits: mode.ToolLimits, Instructions: mode.Instructions,
		})
	}
	slices.SortFunc(modes, func(a, b modePolicy) int { return strings.Compare(string(a.Name), string(b.Name)) })
	delegates := append([]identity.AgentName(nil), d.state.delegates...)
	slices.SortFunc(delegates, func(a, b identity.AgentName) int { return strings.Compare(string(a), string(b)) })
	projection := struct {
		Name                identity.AgentName
		Model               model.Model
		System              string
		Tools               []toolPolicy
		Limits              ToolLimits
		Engine              Engine
		DrainTimeout        time.Duration
		Delegates           []identity.AgentName
		Delegation          Delegation
		Modes               []modePolicy
		InitialMode         ModeName
		PolicyRevision      string
		CounterCapability   *contextcount.CounterCapability
		InferenceCapability *contextcount.InferenceCapability
		ContextObservation  *ContextObservationPolicy
		Compaction          *CompactionPolicy
	}{
		Name: d.state.name, Model: cloneModel(d.state.model), System: d.state.system,
		Tools: tools(d.state.tools), Limits: d.state.limits, Engine: d.state.engine,
		DrainTimeout: d.state.drainTimeout, Delegates: delegates,
		Delegation: d.state.delegation, Modes: modes, InitialMode: d.state.initialMode,
		PolicyRevision: d.state.policyRevision,
	}
	if d.state.contextCounter != nil {
		counter := d.state.counterCapability
		capability := d.state.inferenceCapability
		projection.CounterCapability = &counter
		projection.InferenceCapability = &capability
	}
	if d.state.compaction.CountTimeout != 0 {
		policy := d.state.compaction
		projection.Compaction = &policy
	}
	if d.state.contextObservation.CountTimeout != 0 {
		policy := d.state.contextObservation
		projection.ContextObservation = &policy
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		// The projection is a total, fully-owned marshalable type; a failure here is a
		// programmer error (an unmarshalable field was added), not a runtime/input
		// condition. Fail loudly rather than return a nil-collapsed sha256(nil) digest that
		// would silently defeat restore config-mismatch drift detection.
		panic(&PolicyRevisionMarshalError{Cause: err})
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Bind creates fresh session-specific collaborators and resolves every declared mode.
func (d Definition) Bind(ctx context.Context, bindings tool.Bindings) (BoundDefinition, error) {
	if d.state == nil {
		return nil, &BindError{Kind: BindInvalidDefinition, Index: -1}
	}
	if ctx == nil {
		return nil, &BindError{Kind: BindInvalidContext, Index: -1}
	}
	if bindings.SessionID.IsZero() {
		cause := &tool.InvalidBindingsError{Field: "session_id"}
		return nil, &BindError{Kind: BindInvalidSessionID, Index: -1, Cause: cause}
	}
	if bindings.LoopID.IsZero() {
		cause := &tool.InvalidBindingsError{Field: "loop_id"}
		return nil, &BindError{Kind: BindInvalidLoopID, Index: -1, Cause: cause}
	}
	if d.state.permissionFactory != nil && nilLike(bindings.SecurityLimit) {
		return nil, &BindError{Kind: BindInvalidSecurityLimit, Index: -1}
	}

	type builtDefinition struct {
		definition tool.Definition
		tools      []tool.InvokableTool
	}
	builtByName := make(map[string]builtDefinition)
	toolNames := make(map[string]struct{})
	build := func(defs []tool.Definition) ([]tool.InvokableTool, error) {
		var selected []tool.InvokableTool
		selectedDefinitions := make(map[string]struct{}, len(defs))
		for definitionIndex, def := range defs {
			if nilLike(def) {
				return nil, &BindError{Kind: BindInvalidDefinition, Index: definitionIndex}
			}
			name := def.Name()
			if strings.TrimSpace(name) == "" {
				return nil, &BindError{Kind: BindInvalidDefinition, Name: name, Index: definitionIndex}
			}
			if previous, exists := builtByName[name]; exists {
				if !sameToolDefinition(previous.definition, def) {
					return nil, &BindError{Kind: BindDuplicateDefinitionName, Name: name, Index: definitionIndex}
				}
				if _, selectedAlready := selectedDefinitions[name]; selectedAlready {
					continue
				}
				selectedDefinitions[name] = struct{}{}
				selected = append(selected, previous.tools...)
				continue
			}
			instances, err := def.Build(ctx, cloneBindings(bindings))
			if err != nil {
				var nilTool *tool.NilBuiltToolError
				if errors.As(err, &nilTool) {
					return nil, &BindError{Kind: BindInvalidToolInfo, Name: name, Index: nilTool.Index, Cause: err}
				}
				var producedNames *tool.ProducedToolNamesError
				if errors.As(err, &producedNames) {
					return nil, &BindError{Kind: BindInvalidToolInfo, Name: name, Index: producedNames.Index, Cause: err}
				}
				return nil, err
			}
			for toolIndex, instance := range instances {
				if nilLike(instance) {
					return nil, &BindError{Kind: BindInvalidToolInfo, Name: name, Index: toolIndex}
				}
				info, infoErr := instance.Info(ctx)
				if infoErr != nil {
					return nil, &BindError{Kind: BindInvalidToolInfo, Name: name, Index: toolIndex, Cause: infoErr}
				}
				if info == nil || strings.TrimSpace(info.Name) == "" {
					return nil, &BindError{Kind: BindInvalidToolInfo, Name: name, Index: toolIndex}
				}
				if _, exists := toolNames[info.Name]; exists {
					return nil, &BindError{Kind: BindDuplicateToolName, Name: info.Name, Index: toolIndex}
				}
				toolNames[info.Name] = struct{}{}
			}
			instances = append([]tool.InvokableTool(nil), instances...)
			builtByName[name] = builtDefinition{definition: def, tools: instances}
			selectedDefinitions[name] = struct{}{}
			selected = append(selected, instances...)
		}
		return selected, nil
	}

	// withExtra appends the caller-injected ExtraTools (the derived delegation Subagent
	// tool) to a mode's tool set, so a delegate-bearing loop exposes it in EVERY mode
	// without the definition hand-listing it. The same immutable ExtraTools definitions
	// are appended to base + every mode, so build's by-name cache builds each once and
	// reuses it (no duplicate-tool-name error). An empty ExtraTools is a no-op.
	withExtra := func(defs []tool.Definition) []tool.Definition {
		if len(bindings.ExtraTools) == 0 {
			return defs
		}
		return append(append([]tool.Definition(nil), defs...), bindings.ExtraTools...)
	}
	baseTools, err := build(withExtra(d.state.tools))
	if err != nil {
		return nil, err
	}
	modes := make([]BoundMode, 0, len(d.state.modes)+1)
	baseEffort := d.state.model.Sampling.Effort
	baseModel := cloneModel(d.state.model)
	baseModel.Sampling.Effort = baseEffort
	modes = append(modes, BoundMode{Name: "", Model: baseModel, Effort: baseEffort, Tools: baseTools, ToolLimits: d.state.limits})
	for _, declared := range d.state.modes {
		selectedDefinitions := declared.Tools
		if len(selectedDefinitions) == 0 {
			selectedDefinitions = d.state.tools
		}
		instances, buildErr := build(withExtra(selectedDefinitions))
		if buildErr != nil {
			return nil, buildErr
		}
		selectedModel := declared.Model
		if zeroModel(selectedModel) {
			selectedModel = d.state.model
		}
		effort := declared.Effort
		if effort == model.EffortNone {
			effort = baseEffort
		}
		selectedModel = cloneModel(selectedModel)
		selectedModel.Sampling.Effort = effort
		modes = append(modes, BoundMode{
			Name: declared.Name, Model: selectedModel, Effort: effort,
			Tools: instances, ToolLimits: resolveLimits(d.state.limits, declared.ToolLimits),
			Instructions: declared.Instructions,
		})
	}

	var permission PermissionGate
	if d.state.permissionFactory != nil {
		permission, err = d.state.permissionFactory(ctx, cloneBindings(bindings))
		if err != nil {
			return nil, err
		}
		if nilLike(permission) {
			return nil, &BindError{Kind: BindInvalidPermission, Index: -1}
		}
	}
	return &boundDefinitionState{definition: d.state, modes: modes, permission: permission}, nil
}

// BoundDefinition is the sealed read-only runtime view of one bound loop.
type BoundDefinition interface {
	Name() identity.AgentName
	DisplayName() string
	Description() string
	Engine() Engine
	Client() inference.Client
	Model() model.Model
	Effort() model.Effort
	System() string
	EffectiveSystem() string
	Instructions() string
	Tools() []tool.InvokableTool
	ToolLimits() ToolLimits
	Modes() []BoundMode
	Mode(ModeName) (BoundMode, bool)
	InitialMode() ModeName
	Permission() PermissionGate
	Middlewares() []tool.ToolMiddleware
	DrainTimeout() time.Duration
	RuntimeContext() RuntimeContextProvider
	ContextCounter() contextcount.ContextCounter
	CounterCapability() (contextcount.CounterCapability, bool)
	InferenceCapability() (contextcount.InferenceCapability, bool)
	ContextObservationPolicy() (ContextObservationPolicy, bool)
	CompactionPolicy() (CompactionPolicy, bool)
	ValidateContextModel(model.Model) error
	Delegation() Delegation
	Delegates() []identity.AgentName
	boundDefinition()
}

type boundDefinitionState struct {
	definition *definitionState
	modes      []BoundMode
	permission PermissionGate
}

func (*boundDefinitionState) boundDefinition()              {}
func (b *boundDefinitionState) Name() identity.AgentName    { return b.definition.name }
func (b *boundDefinitionState) DisplayName() string         { return b.definition.displayName }
func (b *boundDefinitionState) Description() string         { return b.definition.description }
func (b *boundDefinitionState) Engine() Engine              { return b.definition.engine }
func (b *boundDefinitionState) Client() inference.Client    { return b.definition.client }
func (b *boundDefinitionState) InitialMode() ModeName       { return b.definition.initialMode }
func (b *boundDefinitionState) Permission() PermissionGate  { return b.permission }
func (b *boundDefinitionState) DrainTimeout() time.Duration { return b.definition.drainTimeout }
func (b *boundDefinitionState) RuntimeContext() RuntimeContextProvider {
	return b.definition.runtimeContext
}
func (b *boundDefinitionState) ContextCounter() contextcount.ContextCounter {
	return b.definition.contextCounter
}
func (b *boundDefinitionState) CounterCapability() (contextcount.CounterCapability, bool) {
	return b.definition.counterCapability, b.definition.contextCounter != nil
}
func (b *boundDefinitionState) InferenceCapability() (contextcount.InferenceCapability, bool) {
	return b.definition.inferenceCapability, b.definition.contextCounter != nil
}
func (b *boundDefinitionState) ContextObservationPolicy() (ContextObservationPolicy, bool) {
	return b.definition.contextObservation, b.definition.contextObservation.CountTimeout != 0
}
func (b *boundDefinitionState) CompactionPolicy() (CompactionPolicy, bool) {
	return b.definition.compaction, b.definition.compaction.CountTimeout != 0
}
func (b *boundDefinitionState) ValidateContextModel(model model.Model) error {
	return validateDefinitionContextModel(b.definition, model)
}
func (b *boundDefinitionState) Delegation() Delegation { return b.definition.delegation }
func (b *boundDefinitionState) Delegates() []identity.AgentName {
	return append([]identity.AgentName(nil), b.definition.delegates...)
}
func (b *boundDefinitionState) Middlewares() []tool.ToolMiddleware {
	return append([]tool.ToolMiddleware(nil), b.definition.middlewares...)
}
func (b *boundDefinitionState) System() string { return b.definition.system }
func (b *boundDefinitionState) EffectiveSystem() string {
	return EffectiveSystem(b.System(), b.Instructions())
}
func (b *boundDefinitionState) Modes() []BoundMode {
	result := make([]BoundMode, len(b.modes))
	for index := range b.modes {
		result[index] = cloneBoundMode(b.modes[index])
	}
	return result
}
func (b *boundDefinitionState) Mode(name ModeName) (BoundMode, bool) {
	for _, mode := range b.modes {
		if mode.Name == name {
			return cloneBoundMode(mode), true
		}
	}
	return BoundMode{}, false
}
func (b *boundDefinitionState) selected() BoundMode {
	mode, _ := b.Mode(b.InitialMode())
	return mode
}
func (b *boundDefinitionState) Model() model.Model          { return b.selected().Model }
func (b *boundDefinitionState) Effort() model.Effort        { return b.selected().Effort }
func (b *boundDefinitionState) Instructions() string        { return b.selected().Instructions }
func (b *boundDefinitionState) Tools() []tool.InvokableTool { return b.selected().Tools }
func (b *boundDefinitionState) ToolLimits() ToolLimits      { return b.selected().ToolLimits }

// EffectiveSystem combines a base system prompt with a mode's instructions: the base alone
// when a mode adds no instructions, the instructions alone when there is no base, otherwise
// the two joined by a blank line. It is exported as the SINGLE source of this rule so the
// loop actor (which resolves the SELECTED mode's system per turn, in loopruntime) composes
// it byte-for-byte identically — restore fidelity depends on the live and folded system
// prompts matching exactly.
func EffectiveSystem(system, instructions string) string {
	if system == "" {
		return instructions
	}
	if instructions == "" {
		return system
	}
	return system + "\n\n" + instructions
}

func WithName(name identity.AgentName) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("name"); err != nil {
			return err
		}
		o.name = name
		return nil
	}
}
func WithInference(client inference.Client, model model.Model) Option {
	model = cloneModel(model)
	return func(o *definitionOptions) error {
		if err := o.singleton("inference"); err != nil {
			return err
		}
		o.client, o.model = client, cloneModel(model)
		return nil
	}
}

// WithContextCounter installs one fixed complete-request counter.
func WithContextCounter(counter contextcount.ContextCounter) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("context_counter"); err != nil {
			return err
		}
		o.contextCounter = counter
		return nil
	}
}

// WithInferenceCapability declares the fixed inference transport posture.
func WithInferenceCapability(capability contextcount.InferenceCapability) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("inference_capability"); err != nil {
			return err
		}
		o.inferenceCapability = capability
		return nil
	}
}

// WithContextObservation installs explicit hard-admission policy without
// enabling conversation compaction.
func WithContextObservation(policy ContextObservationPolicy) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("context_observation"); err != nil {
			return err
		}
		o.contextObservation = policy
		return nil
	}
}

// WithCompaction installs explicit manual and optional automatic policy.
func WithCompaction(policy CompactionPolicy) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("compaction"); err != nil {
			return err
		}
		o.compaction = policy
		return nil
	}
}

// CompactionPolicy returns the frozen policy when configured.
func (d Definition) CompactionPolicy() (CompactionPolicy, bool) {
	if d.state == nil || d.state.compaction.CountTimeout == 0 {
		return CompactionPolicy{}, false
	}
	return d.state.compaction, true
}

// ContextObservationPolicy returns the frozen observe-only policy when configured.
func (d Definition) ContextObservationPolicy() (ContextObservationPolicy, bool) {
	if d.state == nil || d.state.contextObservation.CountTimeout == 0 {
		return ContextObservationPolicy{}, false
	}
	return d.state.contextObservation, true
}

// ValidateContextModel checks structural validity and the fixed transport binding.
func (d Definition) ValidateContextModel(model model.Model) error {
	if d.state == nil {
		return &DefinitionError{Kind: DefinitionInvalidModel, Field: "model"}
	}
	return validateDefinitionContextModel(d.state, model)
}

func validateDefinitionContextModel(state *definitionState, model model.Model) error {
	if err := model.Validate(); err != nil {
		return err
	}
	if err := model.Key().Validate(); err != nil {
		return err
	}
	if state.contextCounter == nil {
		return nil
	}
	return validateContextTransportBinding(state.model, model)
}

// WithDisplayName sets the loop's user-facing presentation label. Purely
// presentational; empty means "no explicit label" and consumers fall back to the
// agent name. It is excluded from PolicyRevision so relabeling never breaks restore
// config-drift detection.
func WithDisplayName(name string) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("display_name"); err != nil {
			return err
		}
		o.displayName = name
		return nil
	}
}

// WithDescription sets the loop's user-facing description. Purely presentational;
// excluded from PolicyRevision for the same restore-compat reason as WithDisplayName.
func WithDescription(desc string) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("description"); err != nil {
			return err
		}
		o.description = desc
		return nil
	}
}
func WithSystem(system string) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("system"); err != nil {
			return err
		}
		o.system = system
		return nil
	}
}
func WithTools(defs ...tool.Definition) Option {
	defs = append([]tool.Definition(nil), defs...)
	return func(o *definitionOptions) error { o.tools = append(o.tools, defs...); return nil }
}
func WithPermissionFactory(factory PermissionFactory) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("permission_factory"); err != nil {
			return err
		}
		o.permissionFactory = factory
		return nil
	}
}
func WithToolMiddlewares(middlewares ...tool.ToolMiddleware) Option {
	middlewares = append([]tool.ToolMiddleware(nil), middlewares...)
	return func(o *definitionOptions) error { o.middlewares = append(o.middlewares, middlewares...); return nil }
}
func WithToolLimits(limits ToolLimits) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("tool_limits"); err != nil {
			return err
		}
		o.limits = limits
		return nil
	}
}
func WithEngine(engine Engine) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("engine"); err != nil {
			return err
		}
		o.engine = engine
		return nil
	}
}
func WithDrainTimeout(timeout time.Duration) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("drain_timeout"); err != nil {
			return err
		}
		o.drainTimeout = timeout
		return nil
	}
}
func WithRuntimeContext(provider RuntimeContextProvider) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("runtime_context"); err != nil {
			return err
		}
		o.runtimeContext = provider
		return nil
	}
}

// WithPolicyRevision supplies stable loop-scoped identity for opaque policy collaborators.
func WithPolicyRevision(revision string) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("policy_revision"); err != nil {
			return err
		}
		options.policyRevision = revision
		return nil
	}
}
func WithDelegates(names ...identity.AgentName) Option {
	names = append([]identity.AgentName(nil), names...)
	return func(o *definitionOptions) error { o.delegates = append(o.delegates, names...); return nil }
}
func WithDelegation(policy Delegation) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("delegation"); err != nil {
			return err
		}
		o.delegation = policy
		return nil
	}
}
func WithModes(modes ...Mode) Option {
	modes = cloneModes(modes)
	return func(o *definitionOptions) error { o.modes = append(o.modes, cloneModes(modes)...); return nil }
}
func WithInitialMode(name ModeName) Option {
	return func(o *definitionOptions) error {
		if err := o.singleton("initial_mode"); err != nil {
			return err
		}
		o.initialMode = name
		return nil
	}
}

func validateModes(modes []Mode, initial ModeName) error {
	seen := make(map[ModeName]struct{}, len(modes))
	for _, mode := range modes {
		if strings.TrimSpace(string(mode.Name)) == "" {
			return &DefinitionError{Kind: DefinitionInvalidMode, Field: "mode.name"}
		}
		if _, exists := seen[mode.Name]; exists {
			return &DefinitionError{Kind: DefinitionDuplicateMode, Field: "mode.name", Value: string(mode.Name)}
		}
		seen[mode.Name] = struct{}{}
		if !zeroModel(mode.Model) {
			if err := mode.Model.Validate(); err != nil {
				return &DefinitionError{Kind: DefinitionInvalidMode, Field: "mode.model", Value: string(mode.Name), Cause: err}
			}
			if err := mode.Model.Key().Validate(); err != nil {
				return &DefinitionError{Kind: DefinitionInvalidMode, Field: "mode.model", Value: string(mode.Name), Cause: err}
			}
			if !mode.Model.Sampling.Effort.Valid() {
				return &DefinitionError{Kind: DefinitionInvalidMode, Field: "mode.model.sampling.effort", Value: string(mode.Name)}
			}
		}
		if !mode.Effort.Valid() || invalidLimits(mode.ToolLimits) {
			return &DefinitionError{Kind: DefinitionInvalidMode, Field: "mode", Value: string(mode.Name)}
		}
		if err := validateDefinitionTools(mode.Tools, "mode.tools"); err != nil {
			return err
		}
	}
	if len(modes) > 0 && initial == "" {
		return &DefinitionError{Kind: DefinitionMissingInitialMode, Field: "initial_mode"}
	}
	if len(modes) == 0 && initial != "" {
		return &DefinitionError{Kind: DefinitionInvalidInitialMode, Field: "initial_mode", Value: string(initial)}
	}
	if initial != "" {
		if _, exists := seen[initial]; !exists {
			return &DefinitionError{Kind: DefinitionInvalidInitialMode, Field: "initial_mode", Value: string(initial)}
		}
	}
	return nil
}

func validateDefinitionTools(defs []tool.Definition, field string) error {
	for index, def := range defs {
		if nilLike(def) || strings.TrimSpace(def.Name()) == "" {
			return &DefinitionError{Kind: DefinitionInvalidTool, Field: field, Value: indexString(index)}
		}
	}
	return nil
}

func cloneModes(modes []Mode) []Mode {
	result := make([]Mode, len(modes))
	for index := range modes {
		result[index] = cloneMode(modes[index])
	}
	return result
}
func dedupeDelegates(names []identity.AgentName) []identity.AgentName {
	seen := make(map[identity.AgentName]struct{})
	result := make([]identity.AgentName, 0, len(names))
	for _, name := range names {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			result = append(result, name)
		}
	}
	return result
}
func cloneBindings(bindings tool.Bindings) tool.Bindings {
	if bindings.Workspace != nil {
		workspace := *bindings.Workspace
		bindings.Workspace = &workspace
	}
	return bindings
}
func sameToolDefinition(a, b tool.Definition) bool {
	aValue, bValue := reflect.ValueOf(a), reflect.ValueOf(b)
	if aValue.Type() != bValue.Type() {
		return false
	}
	switch aValue.Kind() {
	case reflect.Chan, reflect.Func, reflect.Pointer, reflect.UnsafePointer:
		return aValue.Pointer() == bValue.Pointer()
	default:
		return aValue.Type().Comparable() && a == b
	}
}
func nilLike(value interface{}) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return reflected.IsNil()
	}
	return false
}
