package claude

import "github.com/looprig/harness/pkg/foreignloop"

// SpecConfigError is the fail-closed result of resolving an invalid SpecConfig into a
// foreignloop.Spec: a required field (ExecPath or Model) was empty. It is distinct from
// SpawnConfigError — which fails at spawn time inside Agent.Spawn — so a composition
// root can errors.As on the resolve-time failure (NewSpec) on its own.
type SpecConfigError struct{ Field, Reason string }

func (e *SpecConfigError) Error() string {
	return "claude: spec config: " + e.Field + ": " + e.Reason
}

// SpecConfig is the operator-supplied input the composition root resolves into a
// foreignloop.Spec. It is a plain value type so a CLI/flag layer can populate it.
type SpecConfig struct {
	ExecPath   string                        // path to the `claude` binary (e.g. from exec.LookPath)
	Home       string                        // user home for transcript derivation (e.g. os.UserHomeDir)
	Model      string                        // model id passed to --model
	Cwd        string                        // working dir / worktree the agent runs in
	Posture    foreignloop.PermissionPosture // non-interactive permission posture
	EnvAllow   []string                      // whitelisted env keys passed through from the parent (PATH, HOME, TERM, LANG, …)
	Credential map[string]string             // explicit extra env (e.g. {"ANTHROPIC_API_KEY": "…"}) — the ONLY secret crossing the boundary
}

// NewSpec resolves a SpecConfig + the parent environment into a foreignloop.Spec backed
// by the claude adapter — the turn-key seam a future CLI composition root calls.
//
// The child env is the whitelist of parentEnv (keys in cfg.EnvAllow) plus cfg.Credential,
// built through whitelistEnv: parentEnv is passed IN (the composition root hands
// os.Environ()) so this stays testable and NEVER reaches for the global env itself, and
// the credential is the only secret that crosses the process boundary. Returns a typed
// *SpecConfigError when a required field (ExecPath or Model) is empty.
//
// Composition-root contract (deferred to the composition-root branch, see doc.go):
//
//   - Foreign-engine selection is gated behind the root's OWN flag (default native/off).
//     Once selected, the root wires the seam via
//     rig.WithForeignBuilders(foreignloop.BuildWith(spec), foreignloop.BuildRestoredWith(spec)).
//
//   - The cwd is fingerprinted via the rig's ConfigFingerprintFields.WorkspaceRoot,
//     and the adapter/posture via AdapterID/Posture, so the root must ALSO inject
//
//     fingerprintOption := rig.WithFingerprintFields(rig.ConfigFingerprintFields{
//     WorkspaceRoot: cfg.Cwd,
//     AdapterID:     "claude",
//     Posture:       posture,
//     })
//
//   - ExecPath and Env are intentionally NOT fingerprinted: the binary location and the
//     non-secret environment are permitted to drift across runs without re-keying state.
func NewSpec(parentEnv []string, cfg SpecConfig) (foreignloop.Spec, error) {
	if cfg.ExecPath == "" {
		return foreignloop.Spec{}, &SpecConfigError{Field: "ExecPath", Reason: "required"}
	}
	if cfg.Model == "" {
		return foreignloop.Spec{}, &SpecConfigError{Field: "Model", Reason: "required"}
	}
	env := whitelistEnv(parentEnv, cfg.EnvAllow, cfg.Credential)
	agent := &Agent{ExecPath: cfg.ExecPath, Home: cfg.Home, Model: cfg.Model, Env: env}
	return foreignloop.Spec{
		Agent:    agent,
		ExecPath: cfg.ExecPath,
		Cwd:      cfg.Cwd,
		Posture:  cfg.Posture,
		Env:      env,
	}, nil
}
