package event

// ConfigFingerprint is the stable identity of the agent configuration a session
// started under, stamped onto SessionStarted so a durable journal can detect when a
// restore is being attempted against a materially changed config (a different
// model, system prompt, tool policy, skill-trust mode, workspace, foreign adapter, or
// permission posture). It is a fingerprint, not the config itself: each field is either
// a verbatim identifier (AgentKind, ModelID, WorkspaceRoot, AgentAdapter,
// PermissionPosture), a content digest (SystemPromptRev, ToolPolicyRev), or a mode flag
// (RuntimeSkills) — never the raw prompt text or tool definitions — so it is safe to
// persist and compare without leaking config internals. The derivation from a
// loop.Config lives in the session package (FingerprintFrom), which is the layer that
// owns the config; the fields not on loop.Config (AgentKind, RuntimeSkills,
// WorkspaceRoot, AgentAdapter, PermissionPosture) are injected at the composition root.
// This package only defines the value and its equality.
//
// The fields evolve ADDITIVELY: every field is omitzero, so an old journal record
// that predates a field decodes it as the zero value and compares Equal to a record
// that also leaves it empty — a session persisted before a field was added restores
// without a spurious mismatch.
type ConfigFingerprint struct {
	// TopologyRev is the digest of ordered loop definitions, primer roots, active
	// primer, and delegation edges owned by the rig.
	TopologyRev string `json:"topology_rev,omitzero"`
	// AgentKind names the agent role+swarm this session ran (e.g. "swe:orchestrator").
	// It is empty for a caller that does not inject a kind (a non-swarm/legacy session).
	AgentKind string `json:"agent_kind,omitzero"`
	// ModelID is the model identifier the session ran against (the llm.Model.Name).
	ModelID string `json:"model_id,omitzero"`
	// SystemPromptRev is a content digest (hex sha256) of the system prompt text, so
	// a prompt change is detectable without persisting the prompt itself.
	SystemPromptRev string `json:"system_prompt_rev,omitzero"`
	// ToolPolicyRev is a content digest (hex sha256) over the tool set's stable
	// identity (its sorted tool names), so a tool-set change is detectable without
	// persisting the tool definitions.
	ToolPolicyRev string `json:"tool_policy_rev,omitzero"`
	// RuntimeSkills records whether the untrusted, human-gated workspace skill source
	// was enabled for this session. A session must not silently resume under a
	// different skill-trust mode, so the flag is part of the fingerprint. It is the
	// MODE only — the flag alone does NOT distinguish two repos' .skills/, which is
	// what WorkspaceRoot is for.
	RuntimeSkills bool `json:"runtime_skills,omitzero"`
	// WorkspaceRoot is the canonical absolute workspace-root id (filepath.Clean of the
	// absolute root). It binds the session to the repo whose .skills/ (and file tools)
	// it ran against, so a session cannot silently resume under a different repo's
	// workspace. Empty for a caller that does not inject a root.
	WorkspaceRoot string `json:"workspace_root,omitzero"`
	// AgentAdapter identifies the foreign-agent adapter that backed this session
	// (e.g. "claude"). Empty for a native session. A session must not silently resume
	// under a different foreign adapter, so it is part of the fingerprint.
	AgentAdapter string `json:"agent_adapter,omitzero"`
	// PermissionPosture is the non-interactive permission mode the foreign agent ran
	// under (e.g. "default", "acceptEdits"). Empty for a native session. A change in
	// posture is a behavior change that must not resume unnoticed.
	PermissionPosture string `json:"permission_posture,omitzero"`
	// NativePermissionPolicyRev is a content digest (hex sha256) of the NATIVE
	// permission configuration (allowlist + hard-deny lists + MaxReadBytes + the
	// headless mode bits), computed by tools.PolicyFingerprint at the composition
	// root and injected. Empty for a foreign session (which uses PermissionPosture)
	// or a caller that does not inject it. A change is a behavior change that must
	// not resume unnoticed.
	NativePermissionPolicyRev string `json:"native_permission_policy_rev,omitzero"`
}

// Equal reports whether two fingerprints identify the same configuration: true iff
// every field is equal. It is the comparison a restore uses to decide whether the
// persisted config still matches the live one. New fields are additive (omitzero), so
// an old record's empty new field equals a current record that also leaves it empty.
func (f ConfigFingerprint) Equal(other ConfigFingerprint) bool {
	return f.TopologyRev == other.TopologyRev &&
		f.AgentKind == other.AgentKind &&
		f.ModelID == other.ModelID &&
		f.SystemPromptRev == other.SystemPromptRev &&
		f.ToolPolicyRev == other.ToolPolicyRev &&
		f.RuntimeSkills == other.RuntimeSkills &&
		f.WorkspaceRoot == other.WorkspaceRoot &&
		f.AgentAdapter == other.AgentAdapter &&
		f.PermissionPosture == other.PermissionPosture &&
		f.NativePermissionPolicyRev == other.NativePermissionPolicyRev
}
