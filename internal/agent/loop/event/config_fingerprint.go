package event

// ConfigFingerprint is the stable identity of the agent configuration a session
// started under, stamped onto SessionStarted so a durable journal can detect when a
// restore is being attempted against a materially changed config (a different
// model, system prompt, or tool policy). It is a fingerprint, not the config
// itself: each field is either a verbatim identifier (AgentKind, ModelID) or a
// content digest (SystemPromptRev, ToolPolicyRev), never the raw prompt text or
// tool definitions — so it is safe to persist and compare without leaking config
// internals. The derivation from a loop.Config lives in the session package
// (FingerprintFrom), which is the layer that owns the config; this package only
// defines the value and its equality.
type ConfigFingerprint struct {
	// AgentKind names the agent role this session ran (e.g. "primary"). It is left
	// empty until the agent threads its kind through loop.Config.
	AgentKind string `json:"agent_kind,omitzero"`
	// ModelID is the model identifier the session ran against (the ModelSpec.Model).
	ModelID string `json:"model_id,omitzero"`
	// SystemPromptRev is a content digest (hex sha256) of the system prompt text, so
	// a prompt change is detectable without persisting the prompt itself.
	SystemPromptRev string `json:"system_prompt_rev,omitzero"`
	// ToolPolicyRev is a content digest (hex sha256) over the tool set's stable
	// identity (its sorted tool names), so a tool-set change is detectable without
	// persisting the tool definitions.
	ToolPolicyRev string `json:"tool_policy_rev,omitzero"`
}

// Equal reports whether two fingerprints identify the same configuration: true iff
// all four fields are equal. It is the comparison a restore uses to decide whether
// the persisted config still matches the live one.
func (f ConfigFingerprint) Equal(other ConfigFingerprint) bool {
	return f.AgentKind == other.AgentKind &&
		f.ModelID == other.ModelID &&
		f.SystemPromptRev == other.SystemPromptRev &&
		f.ToolPolicyRev == other.ToolPolicyRev
}
