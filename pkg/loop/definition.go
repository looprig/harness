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
)

const defaultDrainTimeout = 5 * time.Second

// Option contributes immutable loop-definition data.
type Option func(*definitionOptions) error

// Definition is a concrete immutable loop definition. Its zero value is invalid.
type Definition struct{ state *definitionState }

type definitionState struct {
	name              identity.AgentName
	client            inference.Client
	model             inference.Model
	system            string
	tools             []tool.Definition
	permissionFactory PermissionFactory
	middlewares       []tool.ToolMiddleware
	limits            ToolLimits
	engine            Engine
	drainTimeout      time.Duration
	runtimeContext    RuntimeContextProvider
	delegates         []identity.AgentName
	delegation        Delegation
	modes             []Mode
	initialMode       ModeName
	policyRevision    string
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

// InitialMode returns the explicitly selected mode, or empty for the base mode.
func (d Definition) InitialMode() ModeName {
	if d.state == nil {
		return ""
	}
	return d.state.initialMode
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
		Name         string
		Requirements tool.Requirements
	}
	type modePolicy struct {
		Name         ModeName
		Model        inference.Model
		Effort       inference.Effort
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
			out = append(out, toolPolicy{Name: definition.Name(), Requirements: definition.Requirements()})
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
		Name           identity.AgentName
		Model          inference.Model
		System         string
		Tools          []toolPolicy
		Limits         ToolLimits
		Engine         Engine
		DrainTimeout   time.Duration
		Delegates      []identity.AgentName
		Delegation     Delegation
		Modes          []modePolicy
		InitialMode    ModeName
		PolicyRevision string
	}{
		Name: d.state.name, Model: cloneModel(d.state.model), System: d.state.system,
		Tools: tools(d.state.tools), Limits: d.state.limits, Engine: d.state.engine,
		DrainTimeout: d.state.drainTimeout, Delegates: delegates,
		Delegation: d.state.delegation, Modes: modes, InitialMode: d.state.initialMode,
		PolicyRevision: d.state.policyRevision,
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

	baseTools, err := build(d.state.tools)
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
		instances, buildErr := build(selectedDefinitions)
		if buildErr != nil {
			return nil, buildErr
		}
		model := declared.Model
		if zeroModel(model) {
			model = d.state.model
		}
		effort := declared.Effort
		if effort == inference.EffortNone {
			effort = baseEffort
		}
		model = cloneModel(model)
		model.Sampling.Effort = effort
		modes = append(modes, BoundMode{
			Name: declared.Name, Model: model, Effort: effort,
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
	Engine() Engine
	Client() inference.Client
	Model() inference.Model
	Effort() inference.Effort
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
func (b *boundDefinitionState) Engine() Engine              { return b.definition.engine }
func (b *boundDefinitionState) Client() inference.Client    { return b.definition.client }
func (b *boundDefinitionState) InitialMode() ModeName       { return b.definition.initialMode }
func (b *boundDefinitionState) Permission() PermissionGate  { return b.permission }
func (b *boundDefinitionState) DrainTimeout() time.Duration { return b.definition.drainTimeout }
func (b *boundDefinitionState) RuntimeContext() RuntimeContextProvider {
	return b.definition.runtimeContext
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
	return effectiveSystem(b.System(), b.Instructions())
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
func (b *boundDefinitionState) Model() inference.Model      { return b.selected().Model }
func (b *boundDefinitionState) Effort() inference.Effort    { return b.selected().Effort }
func (b *boundDefinitionState) Instructions() string        { return b.selected().Instructions }
func (b *boundDefinitionState) Tools() []tool.InvokableTool { return b.selected().Tools }
func (b *boundDefinitionState) ToolLimits() ToolLimits      { return b.selected().ToolLimits }

func effectiveSystem(system, instructions string) string {
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
func WithInference(client inference.Client, model inference.Model) Option {
	model = cloneModel(model)
	return func(o *definitionOptions) error {
		if err := o.singleton("inference"); err != nil {
			return err
		}
		o.client, o.model = client, cloneModel(model)
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
