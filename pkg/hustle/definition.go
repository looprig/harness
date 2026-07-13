package hustle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/inference"
)

const (
	maxPayloadBytes    = 16 * 1024 * 1024
	reservedNamePrefix = "_looprig."
)

// Name is the stable registration name of one hustle definition.
type Name string

// Participation selects the session-global execution lane.
type Participation uint8

const (
	ParticipationUnknown Participation = iota
	ParticipationBlocking
	ParticipationBackground
)

// ModelSource selects how an invocation obtains its inference binding.
type ModelSource uint8

const (
	ModelSourceUnknown ModelSource = iota
	ModelSourceCurrentLoop
	ModelSourceNamed
)

// Limits bounds the serialized request and response payloads.
type Limits struct {
	InputBytes  int
	OutputBytes int
}

// InferenceBinding pairs a client with its validated, secret-free model.
type InferenceBinding struct {
	Client inference.Client
	Model  inference.Model
}

// ModelResolver resolves the exact originating loop's live inference binding.
type ModelResolver interface {
	ResolveHustleModel(context.Context, uuid.UUID) (InferenceBinding, error)
}

// Bindings supplies runtime collaborators needed by a definition.
type Bindings struct {
	Models ModelResolver
}

// DefinitionDescriptor is the complete secret-free behavioral projection used
// by rig identity and durable audit records.
type DefinitionDescriptor struct {
	Name                     Name
	Participation            Participation
	ModelSource              ModelSource
	NamedModelKey            inference.ModelKey
	NamedModelPolicyRevision string
	PromptRevision           string
	PromptSHA256             [sha256.Size]byte
	PolicyRevision           string
	TimeoutNanos             int64
	Limits                   Limits
}

// Validate checks the complete descriptor-only constructor domain without
// requiring the raw system prompt or an inference client.
func (d DefinitionDescriptor) Validate() error {
	trimmedName := strings.TrimSpace(string(d.Name))
	if trimmedName == "" {
		return &DefinitionError{Kind: DefinitionMissingName, Field: "name"}
	}
	if strings.HasPrefix(trimmedName, reservedNamePrefix) {
		return &DefinitionError{Kind: DefinitionReservedName, Field: "name"}
	}
	if d.Participation != ParticipationBlocking && d.Participation != ParticipationBackground {
		return &DefinitionError{Kind: DefinitionInvalidParticipation, Field: "participation"}
	}
	if d.ModelSource != ModelSourceCurrentLoop && d.ModelSource != ModelSourceNamed {
		return &DefinitionError{Kind: DefinitionInvalidModelSource, Field: "model_source"}
	}
	if d.TimeoutNanos <= 0 {
		return &DefinitionError{Kind: DefinitionInvalidTimeout, Field: "timeout"}
	}
	if invalidLimits(d.Limits) {
		return &DefinitionError{Kind: DefinitionInvalidLimits, Field: "limits"}
	}
	if strings.TrimSpace(d.PromptRevision) == "" {
		return &DefinitionError{Kind: DefinitionInvalidPromptRevision, Field: "prompt_revision"}
	}
	if d.PromptSHA256 == ([sha256.Size]byte{}) {
		return &DefinitionError{Kind: DefinitionInvalidSystemPrompt, Field: "prompt_sha256"}
	}
	if strings.TrimSpace(d.PolicyRevision) == "" {
		return &DefinitionError{Kind: DefinitionInvalidPolicyRevision, Field: "policy_revision"}
	}
	if d.ModelSource == ModelSourceNamed {
		if err := d.NamedModelKey.Validate(); err != nil {
			return &DefinitionError{Kind: DefinitionInvalidModel, Field: "named_model_key", Cause: err}
		}
		if strings.TrimSpace(d.NamedModelPolicyRevision) == "" {
			return &DefinitionError{Kind: DefinitionInvalidModel, Field: "named_model_policy_revision"}
		}
		return nil
	}
	if d.NamedModelKey != (inference.ModelKey{}) || d.NamedModelPolicyRevision != "" {
		return &DefinitionError{Kind: DefinitionInvalidModel, Field: "current_loop_model"}
	}
	return nil
}

// Option contributes one immutable definition property.
type Option func(*definitionOptions) error

// Definition is an immutable hustle definition. Its zero value is invalid.
type Definition struct{ state *definitionState }

type definitionState struct {
	descriptor   DefinitionDescriptor
	policyDigest string
	timeout      time.Duration
	systemPrompt string
	named        InferenceBinding
}

type definitionOptions struct {
	name           Name
	participation  Participation
	timeout        time.Duration
	limits         Limits
	modelSource    ModelSource
	named          InferenceBinding
	systemPrompt   string
	promptRevision string
	policyRevision string
	seen           map[string]struct{}
}

func (o *definitionOptions) singleton(field string) error {
	if _, exists := o.seen[field]; exists {
		return &DefinitionError{Kind: DefinitionDuplicateOption, Field: field}
	}
	o.seen[field] = struct{}{}
	return nil
}

// WithName sets the stable definition name.
func WithName(name Name) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("name"); err != nil {
			return err
		}
		options.name = name
		return nil
	}
}

// WithParticipation selects the definition's fixed execution lane.
func WithParticipation(participation Participation) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("participation"); err != nil {
			return err
		}
		options.participation = participation
		return nil
	}
}

// WithTimeout sets the exact invocation timeout.
func WithTimeout(timeout time.Duration) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("timeout"); err != nil {
			return err
		}
		options.timeout = timeout
		return nil
	}
}

// WithLimits sets serialized input and output byte limits.
func WithLimits(limits Limits) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("limits"); err != nil {
			return err
		}
		options.limits = limits
		return nil
	}
}

// WithCurrentLoopModel resolves the originating loop's live model on every run.
func WithCurrentLoopModel() Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("model_source"); err != nil {
			return err
		}
		options.modelSource = ModelSourceCurrentLoop
		return nil
	}
}

// WithNamedInference freezes a named client/model pair in the definition.
func WithNamedInference(client inference.Client, model inference.Model) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("model_source"); err != nil {
			return err
		}
		options.modelSource = ModelSourceNamed
		options.named = InferenceBinding{Client: client, Model: model}
		return nil
	}
}

// WithSystemPrompt freezes the raw prompt and its public revision label.
func WithSystemPrompt(prompt, revision string) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("system_prompt"); err != nil {
			return err
		}
		options.systemPrompt = prompt
		options.promptRevision = revision
		return nil
	}
}

// WithPolicyRevision identifies opaque parser and request-policy behavior.
func WithPolicyRevision(revision string) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("policy_revision"); err != nil {
			return err
		}
		options.policyRevision = revision
		return nil
	}
}

// Define validates and freezes one text-only hustle definition.
func Define(opts ...Option) (Definition, error) {
	resolved := &definitionOptions{seen: make(map[string]struct{})}
	for index, opt := range opts {
		if opt == nil {
			return Definition{}, &DefinitionError{Kind: DefinitionNilOption, Field: "options[" + strconv.Itoa(index) + "]"}
		}
		if err := opt(resolved); err != nil {
			return Definition{}, err
		}
	}
	if err := validateDefinitionOptions(resolved); err != nil {
		return Definition{}, err
	}
	return freezeDefinition(resolved)
}

func validateDefinitionOptions(options *definitionOptions) error {
	trimmedName := strings.TrimSpace(string(options.name))
	if trimmedName == "" {
		return &DefinitionError{Kind: DefinitionMissingName, Field: "name"}
	}
	if strings.HasPrefix(trimmedName, reservedNamePrefix) {
		return &DefinitionError{Kind: DefinitionReservedName, Field: "name"}
	}
	if options.participation != ParticipationBlocking && options.participation != ParticipationBackground {
		return &DefinitionError{Kind: DefinitionInvalidParticipation, Field: "participation"}
	}
	if options.modelSource == ModelSourceUnknown {
		return &DefinitionError{Kind: DefinitionMissingModelSource, Field: "model_source"}
	}
	if options.modelSource == ModelSourceNamed {
		if err := validateInferenceBinding(options.named); err != nil {
			return err
		}
	}
	if options.timeout <= 0 {
		return &DefinitionError{Kind: DefinitionInvalidTimeout, Field: "timeout"}
	}
	if invalidLimits(options.limits) {
		return &DefinitionError{Kind: DefinitionInvalidLimits, Field: "limits"}
	}
	if strings.TrimSpace(options.systemPrompt) == "" {
		return &DefinitionError{Kind: DefinitionInvalidSystemPrompt, Field: "system_prompt"}
	}
	if strings.TrimSpace(options.promptRevision) == "" {
		return &DefinitionError{Kind: DefinitionInvalidPromptRevision, Field: "prompt_revision"}
	}
	if _, exists := options.seen["policy_revision"]; !exists {
		return &DefinitionError{Kind: DefinitionMissingPolicyRevision, Field: "policy_revision"}
	}
	if strings.TrimSpace(options.policyRevision) == "" {
		return &DefinitionError{Kind: DefinitionInvalidPolicyRevision, Field: "policy_revision"}
	}
	return nil
}

func validateInferenceBinding(binding InferenceBinding) error {
	if nilClient(binding.Client) {
		return &DefinitionError{Kind: DefinitionInvalidClient, Field: "client"}
	}
	if err := binding.Model.Validate(); err != nil {
		return &DefinitionError{Kind: DefinitionInvalidModel, Field: "model", Cause: err}
	}
	if err := binding.Model.Key().Validate(); err != nil {
		return &DefinitionError{Kind: DefinitionInvalidModel, Field: "model_key", Cause: err}
	}
	if field := invalidSamplingField(binding.Model.Sampling); field != "" {
		return &DefinitionError{Kind: DefinitionInvalidModel, Field: string(field)}
	}
	return nil
}

type samplingField string

const (
	samplingTemperatureField samplingField = "model.sampling.temperature"
	samplingTopPField        samplingField = "model.sampling.top_p"
	samplingEffortField      samplingField = "model.sampling.effort"
)

func invalidSamplingField(sampling inference.Sampling) samplingField {
	if nonFinite(sampling.Temperature) {
		return samplingTemperatureField
	}
	if nonFinite(sampling.TopP) {
		return samplingTopPField
	}
	if !sampling.Effort.Valid() {
		return samplingEffortField
	}
	return ""
}

func nonFinite(value *float64) bool {
	return value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0))
}

func invalidLimits(limits Limits) bool {
	return limits.InputBytes <= 0 || limits.InputBytes > maxPayloadBytes ||
		limits.OutputBytes <= 0 || limits.OutputBytes > maxPayloadBytes
}

func freezeDefinition(options *definitionOptions) (Definition, error) {
	named := InferenceBinding{Client: options.named.Client, Model: options.named.Model.Clone()}
	namedRevision := ""
	if options.modelSource == ModelSourceNamed {
		var err error
		namedRevision, err = digestModelPolicy(named.Model)
		if err != nil {
			return Definition{}, &DefinitionError{Kind: DefinitionInvalidModel, Field: "model_policy", Cause: err}
		}
	}
	descriptor := DefinitionDescriptor{
		Name: options.name, Participation: options.participation, ModelSource: options.modelSource,
		PromptRevision: options.promptRevision, PromptSHA256: sha256.Sum256([]byte(options.systemPrompt)),
		PolicyRevision: options.policyRevision, TimeoutNanos: int64(options.timeout), Limits: options.limits,
	}
	if options.modelSource == ModelSourceNamed {
		descriptor.NamedModelKey = named.Model.Key()
		descriptor.NamedModelPolicyRevision = namedRevision
	}
	if err := descriptor.Validate(); err != nil {
		return Definition{}, err
	}
	policyDigest, err := digestDescriptorPolicy(descriptor)
	if err != nil {
		return Definition{}, err
	}
	return Definition{state: &definitionState{
		descriptor: descriptor, policyDigest: policyDigest, timeout: options.timeout,
		systemPrompt: options.systemPrompt, named: named,
	}}, nil
}

func digestModelPolicy(model inference.Model) (string, error) {
	encoded, err := json.Marshal(model)
	if err != nil {
		return "", &RevisionError{Cause: err}
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func digestDescriptorPolicy(descriptor DefinitionDescriptor) (string, error) {
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return "", &RevisionError{Cause: err}
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// Name returns the stable registration name.
func (d Definition) Name() Name {
	if d.state == nil {
		return ""
	}
	return d.state.descriptor.Name
}

// Participation returns the definition's fixed execution lane.
func (d Definition) Participation() Participation {
	if d.state == nil {
		return ParticipationUnknown
	}
	return d.state.descriptor.Participation
}

// Timeout returns the definition's exact invocation timeout.
func (d Definition) Timeout() time.Duration {
	if d.state == nil {
		return 0
	}
	return d.state.timeout
}

// Limits returns the definition's immutable payload limits.
func (d Definition) Limits() Limits {
	if d.state == nil {
		return Limits{}
	}
	return d.state.descriptor.Limits
}

// Descriptor returns the definition's secret-free behavioral projection.
func (d Definition) Descriptor() DefinitionDescriptor {
	if d.state == nil {
		return DefinitionDescriptor{}
	}
	return d.state.descriptor
}

// PolicyRevision returns the stable digest of all behavior-affecting fields.
func (d Definition) PolicyRevision() string {
	if d.state == nil {
		return ""
	}
	return d.state.policyDigest
}

// Bind validates runtime collaborators and returns a read-only bound view.
func (d Definition) Bind(ctx context.Context, bindings Bindings) (BoundDefinition, error) {
	if d.state == nil {
		return nil, &BindError{Kind: BindInvalidDefinition}
	}
	if ctx == nil {
		return nil, &BindError{Kind: BindInvalidContext}
	}
	if d.state.descriptor.ModelSource == ModelSourceCurrentLoop && nilResolver(bindings.Models) {
		return nil, &BindError{Kind: BindMissingModelResolver}
	}
	return &boundDefinitionState{definition: d, models: bindings.Models}, nil
}

// BoundDefinition is the sealed runtime view of one immutable definition.
type BoundDefinition interface {
	Name() Name
	Participation() Participation
	Timeout() time.Duration
	Limits() Limits
	Descriptor() DefinitionDescriptor
	ResolveInference(context.Context, uuid.UUID) (InferenceBinding, error)
	SystemPrompt() string
	boundDefinition()
}

type boundDefinitionState struct {
	definition Definition
	models     ModelResolver
}

func (b *boundDefinitionState) Name() Name                       { return b.definition.Name() }
func (b *boundDefinitionState) Participation() Participation     { return b.definition.Participation() }
func (b *boundDefinitionState) Timeout() time.Duration           { return b.definition.Timeout() }
func (b *boundDefinitionState) Limits() Limits                   { return b.definition.Limits() }
func (b *boundDefinitionState) Descriptor() DefinitionDescriptor { return b.definition.Descriptor() }
func (b *boundDefinitionState) SystemPrompt() string             { return b.definition.state.systemPrompt }
func (*boundDefinitionState) boundDefinition()                   {}

// ResolveInference returns a fresh model clone. Current-loop definitions call
// their exact UUID resolver on every invocation and never fall back.
func (b *boundDefinitionState) ResolveInference(ctx context.Context, loopID uuid.UUID) (InferenceBinding, error) {
	if ctx == nil {
		return InferenceBinding{}, &ResolveError{Kind: ResolveInvalidContext}
	}
	if b.definition.state.descriptor.ModelSource == ModelSourceNamed {
		named := b.definition.state.named
		return InferenceBinding{Client: named.Client, Model: named.Model.Clone()}, nil
	}
	if loopID.IsZero() {
		return InferenceBinding{}, &ResolveError{Kind: ResolveInvalidLoopID}
	}
	binding, err := b.models.ResolveHustleModel(ctx, loopID)
	if err != nil {
		return InferenceBinding{}, &ResolveError{Kind: ResolveModelFailed, Cause: err}
	}
	if err := validateResolvedBinding(binding); err != nil {
		return InferenceBinding{}, err
	}
	return InferenceBinding{Client: binding.Client, Model: binding.Model.Clone()}, nil
}

func validateResolvedBinding(binding InferenceBinding) error {
	if nilClient(binding.Client) {
		return &ResolveError{Kind: ResolveInvalidBinding}
	}
	if err := binding.Model.Validate(); err != nil {
		return &ResolveError{Kind: ResolveInvalidBinding, Cause: err}
	}
	if err := binding.Model.Key().Validate(); err != nil {
		return &ResolveError{Kind: ResolveInvalidBinding, Cause: err}
	}
	if invalidSamplingField(binding.Model.Sampling) != "" {
		return &ResolveError{Kind: ResolveInvalidBinding}
	}
	return nil
}

func nilClient(client inference.Client) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	return nilReflectValue(value)
}

func nilResolver(resolver ModelResolver) bool {
	if resolver == nil {
		return true
	}
	value := reflect.ValueOf(resolver)
	return nilReflectValue(value)
}

func nilReflectValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
