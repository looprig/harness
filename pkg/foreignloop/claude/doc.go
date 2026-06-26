// Package claude is the real `claude` subprocess adapter for the foreign loop. It
// builds the child argv, derives the deterministic transcript path, gates the child
// environment, and spawns the CLI in its own process group, satisfying
// foreignloop.ForeignAgent. It also resolves an operator SpecConfig into a
// foreignloop.Spec (NewSpec) — the turn-key seam a composition root wires.
//
// # CLI flag wiring is deferred to the composition-root branch
//
// This branch ships the reusable, tested composition seam — NewSpec — but NOT a CLI
// main: there is no composition root (no cmd/ main, nothing constructs a session)
// here, so there is no flag to gate foreign-engine selection on yet. That flag/gate
// lives on the composition-root branch. The full builder wiring this resolver feeds is
// already proven by pkg/session/foreign_e2e_test.go, which constructs
// WithForeignBuilder(foreignloop.BuildWith(spec), foreignloop.BuildRestoredWith(spec))
// exactly as a composition root would.
//
// # The three lines a composition root performs
//
// Once a future composition root resolves operator flags (binary path p, home h, model
// m, worktree w, allow-list allow, credential cred) it wires the seam like so:
//
//	spec, _ := claude.NewSpec(os.Environ(), claude.SpecConfig{
//		ExecPath: p, Home: h, Model: m, Cwd: w,
//		Posture: postureFromFlag(...), EnvAllow: allow, Credential: cred,
//	})
//	opts := []session.Option{
//		session.WithForeignBuilder(foreignloop.BuildWith(spec), foreignloop.BuildRestoredWith(spec)),
//		session.WithConfigFingerprintFields(session.ConfigFingerprintFields{
//			WorkspaceRoot: w, AdapterID: "claude", Posture: "default",
//		}),
//	}
//	// then cfg.Engine = loop.EngineForeignClaude for the agents that should run foreign.
//
// The cwd (WorkspaceRoot), adapter id, and posture are fingerprinted so foreign and
// native runs key distinct state; the exec path and the (non-secret) env are
// intentionally left out of the fingerprint and may drift between runs.
package claude
