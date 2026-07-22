# Security policy

`looprig/harness` is an agent runtime: it executes model-directed tool
calls that can read and write files, spawn processes, and reach the network.
Security is part of the design, not an afterthought. This file is how to
report a vulnerability and what to expect when you do.

## Threat surface

The harness runtime mediates between an `inference.Client` (model output is
untrusted) and a set of tools (file, shell, network, …). The trust
boundaries the codebase enforces are:

- **Tool argument validation.** Every tool owns a `CallPreparer` that
  decodes, validates, and normalizes its arguments *before* any permission
  decision. Invalid input fails during preparation and never reaches the
  gate or the executor.
- **Three-state permission gate.** Every effectful call is decided by
  `pkg/gate`'s `Deny` / `Gated` / `Allow` evaluator: configured access first,
  every stored deny before any allow, one combined approval. The runtime
  fails closed on any ambiguity.
- **Sandbox isolation.** Harness defines `gate.AccessSource` /
  `gate.GrantIssuer` seams and the `pkg/tool` preparation boundary; the
  actual OS confinement lives in the sibling `looprig/sandbox` module and
  is wired at the composition root. Harness never imports a sandbox.
- **Durable log integrity.** One serialized writer per session journal
  produces a totally-ordered, gap-free record. Restore replays it. Grant
  tokens never enter a prompt, display, journal, or audit payload
  (`Resolution.Grants` is `json:"-"`).
- **No secrets in code.** No hardcoded tokens, passwords, keys, or
  connection strings. Required secrets are read from environment variables
  or a secrets manager and fail loudly on startup if missing.
- **Fail secure.** On error or ambiguity, deny by default. A failed
  permission check blocks the action; it does not fall through.

If you find a way to bypass any of these boundaries — get an unprepared
call to the executor, get a `Gated` call to run without an approval, get a
grant token to leak into the journal or an audit record, get the runtime
to fall through on a failed permission check, or get a tool argument to
reach an executor without normalization — that is a vulnerability we want
to hear about.

## Reporting a vulnerability

**Please do not open a public GitHub issue for a security vulnerability.**

Instead, report it privately:

- **Preferred:** use GitHub's private vulnerability reporting at
  <https://github.com/looprig/harness/security/advisories/new>. This
  creates a private fork and advisory visible only to the maintainers.
- **Alternative:** email `security@looprig.dev` with a description, a
  reproduction, and an impact assessment. PGP key available on request.

Please include:

- A clear description of the issue and the trust boundary it crosses.
- Steps to reproduce, with the smallest possible example. If you have a
  patch, attach it.
- The versions or commit shas you reproduced against.
- Your assessment of impact and any affected downstream modules
  (`looprig/tools`, `looprig/sandbox`, `looprig/foreignloops`, …).

## What to expect

- **Acknowledgement** within two business days.
- **An initial assessment** within seven days, including a severity read
  and a co-ordinated disclosure timeline.
- **A fix or mitigation** targeted at the next patch release, or an
  expedited release for high-severity issues. We will co-ordinate
  disclosure with you and credit you in the release notes unless you
  prefer otherwise.
- We will keep you informed at each step and never publicly disclose a
  reported issue before a fix is available.

## Out of scope

The security policy covers `looprig/harness` and its boundaries. Issues
in sibling modules belong in their own repositories:

- `looprig/tools` — bash/web/standard tool implementations.
- `looprig/sandbox` — OS confinement and access profiles.
- `looprig/foreignloops` — `codex` / `claude` subprocess backends.
- `looprig/mcp` — MCP client and integration.
- `looprig/storage`, `looprig/fsstore`, `looprig/natsstore`,
  `looprig/rclonestore` — storage backends.
- `looprig/inference`, `looprig/core`, `looprig/eval`, `looprig/tui`.

Hardening advice and "found a cool bypass that requires the user to
configure the runtime insecurely on purpose" are not vulnerabilities —
open a regular issue for those.

## Hardening your own deployment

If you are deploying harness, the levers that most affect your security
posture are:

1. **The `gate.Evaluator` you wire.** `NewInteractiveEvaluator` requires
   both an `Approver` and a durable `RuleWriter`; `NewHeadlessEvaluator`
   never prompts and resolves an unmet gated requirement as a typed
   approval-required denial. Pick the one that matches your trust model.
2. **The `AccessSource` and `GrantIssuer` you bind.** They are the only
   thing that decides `Deny` / `Gated` / `Allow` per requirement kind. The
   sibling `looprig/sandbox` provides OS-enforced implementations; do not
   write a `staticAllow` one for production.
3. **The tool set you register.** Tools own preparation. A tool that
   returns an empty `tool.Request` for an effectful action has no
   requirements and the gate will allow it. Audit your tool definitions.
4. **The journal backend.** Use a `storage.Composite` whose `Ledger`
   provides single-writer fencing and durable persistence. The in-memory
   `memstore` reference backend is for tests, not production.
5. **Secrets.** Read them from environment variables or a secrets manager,
   never from a checked-in file. `CLAUDE.md` lists the only sanctioned
   external packages; anything else needs explicit approval.
