package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/inventivepotter/urvi/internal/agent/loop"
	"github.com/inventivepotter/urvi/internal/hashcache"
	"github.com/inventivepotter/urvi/internal/tool"
)

// check.go implements the seven-stage, fail-secure PermissionChecker.Check
// (design §3c) plus the per-tool argument extraction/classification and the
// persisted-approval (Stage 5) reading path.
//
// FAIL-SECURE INVARIANT: every parse, resolve, or classification error — and any
// ambiguity — resolves to EffectDeny (when a boundary is provably crossed) or
// falls through toward EffectAsk (Stage 7), NEVER to EffectAutoApprove. Stages 1
// and 2 (containment + hard-deny) run FIRST and are non-bypassable: no later
// approval stage can upgrade their deny.
//
// PER-TOOL ARG JSON FIELD-NAME CONTRACT (the Phase-6 tools MUST use these exact
// field names; this is the boundary-extraction contract):
//
//	ReadFile  : {"path": "<file>"}                      (read; boundary = path)
//	WriteFile : {"path": "<file>"}                      (write; boundary = path)
//	EditFile  : {"path": "<file>"}                      (write; boundary = path)
//	Glob      : {"pattern": "<glob>", "root": "<dir>"}  (read; boundary = root, default ".")
//	Grep      : {"pattern": "<re>", "path": "<dir>"}    (read; boundary = path, default ".")
//	Bash      : {"command": "<cmd>", "workdir": "<dir>"}(bash; boundary = command + optional workdir)
//	Fetch     : {"url": "<url>", "method": "<METHOD>"}  (network; no filesystem boundary)
//	WebSearch : {"query": "<q>"}                        (network; no filesystem boundary)
//
// An UNKNOWN tool name is classified "unknown": if its args carry a "path" field
// it is treated as a contained read path (containment + read-AND-write hard-deny)
// so it can never bypass a denied path/escape; otherwise it skips stages 1–2 and
// falls through toward Ask. Unknown tools are never hard-approved or matched by a
// persisted/session record unless an operator named them explicitly.

// wildcardTool is the HardApprove sentinel meaning "all tools" (Stage 4).
const wildcardTool = "*"

// Per-tool arg JSON field names (the extraction contract documented above).
const (
	fieldPath    = "path"
	fieldRoot    = "root"
	fieldPattern = "pattern"
	fieldCommand = "command"
	fieldWorkdir = "workdir"
	fieldURL     = "url"
	fieldMethod  = "method"
)

// defaultSearchDir is the containment boundary used when a Glob/Grep call omits
// its dir field — the workspace root itself.
const defaultSearchDir = "."

// toolClass is how Check classifies a tool to know which boundary to extract.
type toolClass uint8

const (
	classUnknown toolClass = iota // unknown tool name (see contract note)
	classRead                     // path read tool (ReadFile/Glob/Grep)
	classWrite                    // path write tool (WriteFile/EditFile)
	classBash                     // shell command tool
	classNetwork                  // Fetch/WebSearch — no filesystem boundary
)

// Built-in tool names — the classification keys. Keeping the names here makes the
// classification explicit and auditable (no reliance on capability interfaces
// that a hostile tool could spoof to dodge a stage).
const (
	toolReadFile  = "ReadFile"
	toolWriteFile = "WriteFile"
	toolEditFile  = "EditFile"
	toolGlob      = "Glob"
	toolGrep      = "Grep"
	toolBash      = "Bash"
	toolFetch     = "Fetch"
	toolWebSearch = "WebSearch"
)

// classifyTool maps a tool name to its class. Classification is by NAME (an
// explicit switch), not by probing capability interfaces: the gate must decide
// which boundary applies from the operator-known tool identity, not from a method
// set the tool itself controls (a hostile tool could otherwise hide its write
// nature). An unrecognized name is classUnknown — handled fail-secure by Check.
func classifyTool(toolName string) toolClass {
	switch toolName {
	case toolReadFile, toolGlob, toolGrep:
		return classRead
	case toolWriteFile, toolEditFile:
		return classWrite
	case toolBash:
		return classBash
	case toolFetch, toolWebSearch:
		return classNetwork
	default:
		return classUnknown
	}
}

// Check runs the seven fail-secure stages top-to-bottom; the first definitive
// effect wins. It holds c.mu for its whole duration so the read of
// policy.Policies and the drive of the approval caches are atomic w.r.t. a
// concurrent session-scope grant.
//
//	Stage 1  Containment   — deny if a path arg escapes the workspace
//	Stage 2  HardDeny      — deny if a path/command matches a hard-deny rule
//	Stage 3  EffectChecker — a tool's per-call override, if it has an opinion
//	Stage 4  HardApprove   — operator always-allow (tool name or "*")
//	Stage 5  Persisted     — ws then user approvals files; deny beats allow
//	Stage 6  Session       — in-memory policy list
//	Stage 7  Default       — EffectAsk
func (c *PermissionChecker) Check(ctx context.Context, t tool.InvokableTool, toolName, argsJSON string) loop.Effect {
	c.mu.Lock()
	defer c.mu.Unlock()

	class := classifyTool(toolName)

	// Stages 1 & 2 are the non-bypassable safety denies. The outcome is one of:
	// denied (a boundary was provably crossed), askOnly (the call is malformed/
	// ambiguous — it cleared the boundary but must NOT be auto-approved), or
	// cleared (proceed to the approval stages).
	switch eff, outcome := c.stageContainmentAndHardDeny(toolName, class, argsJSON); outcome {
	case boundaryDenied:
		return eff
	case boundaryAskOnly:
		return loop.EffectAsk
	}

	// Fail-secure malformed-args gate (defense in depth — the runner normally
	// sanitizes invalid Input to "{}" before Check). If the args are not a
	// parseable JSON object, we cannot prove the call's boundary, so NO approval
	// stage may run: a malformed call can never be auto-approved (not by an
	// EffectChecker, a HardApprove "*", a persisted allow, or a session policy).
	// It falls straight to EffectAsk so the user is prompted.
	if !argsAreObject(argsJSON) {
		return loop.EffectAsk
	}

	// Stage 3: EffectChecker (an explicit per-call override from the tool).
	if eff, handled := stageEffectChecker(t, argsJSON); handled {
		return eff
	}

	// Stage 4: operator always-allow.
	if c.stageHardApprove(toolName) {
		return loop.EffectAutoApprove
	}

	// Stage 5: persisted approvals (deny beats allow across BOTH files).
	if eff, decided := c.stagePersistedApprovals(ctx, toolName, class, argsJSON); decided {
		return eff
	}

	// Stage 6: in-memory session policies.
	if eff, decided := c.stageSessionPolicies(toolName, class, argsJSON); decided {
		return eff
	}

	// Stage 7: default.
	return loop.EffectAsk
}

// boundaryOutcome is the result of the Stage-1/2 safety evaluation.
//
// The loop.Effect returned ALONGSIDE a boundaryOutcome is meaningful ONLY when
// the outcome is boundaryDenied (it carries the EffectDeny to return). For
// boundaryCleared and boundaryAskOnly the paired Effect is a DON'T-CARE: Check
// ignores it (a cleared call proceeds to the approval stages; an askOnly call
// returns loop.EffectAsk regardless). The helpers below conventionally return
// loop.EffectAsk in those cases, but only the outcome is load-bearing.
type boundaryOutcome uint8

const (
	// boundaryCleared: the call passed containment + hard-deny; proceed to the
	// approval stages.
	boundaryCleared boundaryOutcome = iota
	// boundaryDenied: a boundary was provably crossed (escape or hard-deny match);
	// return the accompanying EffectDeny — non-bypassable.
	boundaryDenied
	// boundaryAskOnly: the call is malformed/ambiguous (e.g. a required file path
	// is missing). It did not cross a boundary, but it must NOT be auto-approved by
	// any later stage; Check returns EffectAsk.
	boundaryAskOnly
)

// stageContainmentAndHardDeny runs Stages 1 and 2 for path/bash tools. Network
// tools have no filesystem boundary and clear immediately. The returned Effect is
// meaningful only when the outcome is boundaryDenied.
func (c *PermissionChecker) stageContainmentAndHardDeny(toolName string, class toolClass, argsJSON string) (loop.Effect, boundaryOutcome) {
	switch class {
	case classRead:
		// Glob/Grep boundary is a SEARCH DIR (defaults to the root when omitted);
		// ReadFile's boundary is a REQUIRED file path.
		return c.checkPathBoundary(argsJSON, readBoundaryField(toolName), isSearchDirTool(toolName), c.policy.HardDeny.DeniedReadPaths)
	case classWrite:
		return c.checkPathBoundary(argsJSON, fieldPath, false, c.policy.HardDeny.DeniedWritePaths)
	case classBash:
		return c.checkBashBoundary(argsJSON)
	case classNetwork:
		// No filesystem boundary → cleared. The Effect is a don't-care here (only
		// boundaryDenied carries a meaningful Effect); cleared proceeds to Stage 3+.
		return loop.EffectAsk, boundaryCleared
	default: // classUnknown
		return c.checkUnknownBoundary(argsJSON)
	}
}

// isSearchDirTool reports whether the tool's path boundary is a directory to
// search (defaults to the workspace root when omitted) rather than a required
// file path. Glob ("root") and Grep ("path") search a directory; ReadFile reads a
// required file.
func isSearchDirTool(toolName string) bool {
	return toolName == toolGlob || toolName == toolGrep
}

// readBoundaryField returns the JSON field holding the read boundary for a read
// tool: ReadFile reads "path"; Glob's boundary is "root"; Grep's is "path".
func readBoundaryField(toolName string) string {
	switch toolName {
	case toolGlob:
		return fieldRoot
	case toolGrep:
		return fieldPath
	default: // toolReadFile
		return fieldPath
	}
}

// checkPathBoundary extracts the path field, runs containment (Stage 1) and the
// absolute hard-deny match (Stage 2) against deniedGlobs. For a search-dir tool
// (Glob/Grep) a missing/empty field defaults to the workspace root. For a file
// tool (ReadFile/WriteFile/EditFile) the path is REQUIRED: a missing/empty path
// is a malformed call → fall through to Ask (never auto-approved, but not a
// boundary-crossing deny either). Any extraction or containment error is a DENY
// (fail-secure: an unparseable/escaping path tool call must not slip past).
func (c *PermissionChecker) checkPathBoundary(argsJSON, field string, searchDir bool, deniedGlobs []string) (loop.Effect, boundaryOutcome) {
	raw, ok, err := extractStringField(argsJSON, field)
	if err != nil {
		// Unparseable args for a filesystem tool: cannot prove containment → deny.
		return loop.EffectDeny, boundaryDenied
	}
	if !ok || raw == "" {
		if searchDir {
			raw = defaultSearchDir // search the workspace root by default.
		} else {
			// A file tool with no required path is malformed/ambiguous: it did not
			// cross a boundary, but it must never be auto-approved → Ask only.
			return loop.EffectAsk, boundaryAskOnly
		}
	}
	return c.containAndHardDeny(raw, deniedGlobs)
}

// checkBashBoundary runs the Bash safety gates: containment of an optional workdir
// (Stage 1) and the denied-bash-prefix match on the normalized command (Stage 2).
// Unparseable args fall through (false) — a bash call with no extractable command
// cannot match a prefix and has no path to contain, so a later stage Asks; this
// is fail-secure because Bash defaults to Ask and is never auto-approved here.
func (c *PermissionChecker) checkBashBoundary(argsJSON string) (loop.Effect, boundaryOutcome) {
	cmd, _, cmdErr := extractStringField(argsJSON, fieldCommand)
	workdir, wdOK, wdErr := extractStringField(argsJSON, fieldWorkdir)
	if cmdErr != nil || wdErr != nil {
		// Unparseable args: no command/workdir to evaluate. The denied-prefix gate
		// cannot inspect the command, so a malformed Bash call must NOT be
		// auto-approved → Ask only (the malformed-args gate would also catch this).
		return loop.EffectAsk, boundaryAskOnly
	}

	// Stage 1: contain the workdir if one was supplied.
	if wdOK && workdir != "" {
		if _, err := containedPath(c.policy.WorkspaceRoot, workdir); err != nil {
			return loop.EffectDeny, boundaryDenied
		}
	}

	// Stage 2: denied bash prefix on the normalized command.
	if matchDeniedBashPrefix(cmd, c.policy.HardDeny.DeniedBashPrefixes) {
		return loop.EffectDeny, boundaryDenied
	}
	// Cleared: the paired Effect is a don't-care (ignored unless boundaryDenied).
	return loop.EffectAsk, boundaryCleared
}

// checkUnknownBoundary handles an unclassifiable tool that may still carry a
// path-shaped arg. If a "path" field is present it is treated as a contained read
// path and matched against BOTH the read and write hard-deny sets (we do not know
// the tool's direction, so we apply the union — this can only DENY, never
// approve). With no path field there is no filesystem boundary to enforce here;
// the call falls through toward Ask (it cannot be hard-approved or matched by a
// record unless an operator named it).
func (c *PermissionChecker) checkUnknownBoundary(argsJSON string) (loop.Effect, boundaryOutcome) {
	raw, ok, err := extractStringField(argsJSON, fieldPath)
	if err != nil {
		// Unparseable args, unknown tool: nothing we can prove. Ask only (an
		// unknown tool also cannot be hard-approved/matched downstream anyway).
		return loop.EffectAsk, boundaryAskOnly
	}
	if !ok || raw == "" {
		// No path-shaped boundary to enforce; let it flow to the (Ask) default —
		// an unknown tool is never hard-approved or matched by a record.
		return loop.EffectAsk, boundaryCleared
	}
	// Apply the union of read + write deny sets (direction unknown → strictest).
	union := make([]string, 0, len(c.policy.HardDeny.DeniedReadPaths)+len(c.policy.HardDeny.DeniedWritePaths))
	union = append(union, c.policy.HardDeny.DeniedReadPaths...)
	union = append(union, c.policy.HardDeny.DeniedWritePaths...)
	return c.containAndHardDeny(raw, union)
}

// containAndHardDeny runs containment (Stage 1) then the absolute hard-deny match
// (Stage 2) for one resolved path. A containment failure or a hard-deny match
// returns (EffectDeny, true). Clearing both returns (_, false).
func (c *PermissionChecker) containAndHardDeny(rawPath string, deniedGlobs []string) (loop.Effect, boundaryOutcome) {
	abs, err := containedPath(c.policy.WorkspaceRoot, rawPath)
	if err != nil {
		// Escapes the workspace or cannot be resolved → deny (Stage 1).
		return loop.EffectDeny, boundaryDenied
	}
	home := resolveHomeOrEmpty(c.homeDir)
	for _, pat := range deniedGlobs {
		if matchHardDenyAbs(pat, abs, home) {
			return loop.EffectDeny, boundaryDenied // Stage 2.
		}
	}
	// Cleared: the paired Effect is a don't-care (ignored unless boundaryDenied).
	return loop.EffectAsk, boundaryCleared
}

// stageEffectChecker consults the tool's optional EffectChecker (Stage 3). A tool
// that does not implement EffectChecker yields handled=false. The interface lives
// in tools/ and returns a loop.Effect; a handled=true result pins the effect
// (subject only to the earlier non-bypassable Stages 1–2, already passed).
func stageEffectChecker(t tool.InvokableTool, argsJSON string) (loop.Effect, bool) {
	ec, ok := t.(EffectChecker)
	if !ok {
		return loop.EffectAsk, false
	}
	return ec.CheckEffect(argsJSON)
}

// stageHardApprove reports whether the operator always-allows this tool (Stage 4):
// either the wildcard "*" is present or the exact tool name is listed.
func (c *PermissionChecker) stageHardApprove(toolName string) bool {
	for _, name := range c.policy.HardApprove.Tools {
		if name == wildcardTool || name == toolName {
			return true
		}
	}
	return false
}

// stagePersistedApprovals reads the workspace-then-user approvals files (Stage 5)
// and reduces ALL matching records with DENY-BEATS-ALLOW across BOTH files. A
// missing file is empty (not an error); a malformed file behaves as empty and is
// warned once via slog (never a deny, never an auto-approve). The decision:
// any matching deny → EffectDeny; else any matching allow → EffectAutoApprove;
// else (_, false) to fall through.
func (c *PermissionChecker) stagePersistedApprovals(ctx context.Context, toolName string, class toolClass, argsJSON string) (loop.Effect, bool) {
	matcher := c.recordMatcher(toolName, class, argsJSON)

	home, err := c.homeDir()
	if err != nil {
		// Home unresolvable → both store files absent → contribute nothing.
		slog.WarnContext(ctx, "tools: home dir unresolvable; persisted approvals skipped", "err", err)
		return loop.EffectAsk, false
	}

	wsRecords := c.loadWorkspaceApprovals(ctx, home)
	userRecords := c.loadUserApprovals(ctx, home)

	all := make([]ApprovalRecord, 0, len(wsRecords)+len(userRecords))
	all = append(all, wsRecords...)
	all = append(all, userRecords...)
	return reduceApprovalRecords(all, matcher)
}

// loadWorkspaceApprovals reads + parses the workspace-scoped approvals file. A
// missing file → no records (not an error); a read or parse error → no records +
// a single warning (fail open to the next stage). It NEVER reads any in-repo
// path — only <home>/.urvi/workspaces/<hash>/approvals.json.
func (c *PermissionChecker) loadWorkspaceApprovals(ctx context.Context, home string) []ApprovalRecord {
	hash, err := workspaceHash(c.policy.WorkspaceRoot)
	if err != nil {
		slog.WarnContext(ctx, "tools: workspace root unresolvable; workspace approvals skipped", "err", err)
		return nil
	}
	return loadApprovalRecords(ctx, c.wsCache, home, workspaceApprovalsPath(home, hash))
}

// loadUserApprovals reads + parses the user-global approvals file with the same
// fail-open semantics as loadWorkspaceApprovals.
func (c *PermissionChecker) loadUserApprovals(ctx context.Context, home string) []ApprovalRecord {
	return loadApprovalRecords(ctx, c.userCache, home, userApprovalsPath(home))
}

// loadApprovalRecords reads the file at p (HOME-anchored, with its components
// symlink-checked and the file itself hardened-checked), feeds the bytes to the
// content-keyed cache, and returns its records. A non-existent file is empty (no
// warning — an absent store is normal). Any other read error, a hardening
// violation, or a parse error yields no records plus a single warning naming the
// PATH only (never the contents — those could carry sensitive match patterns).
// This is the fail-open-to-Ask behaviour.
//
// home is the resolved home dir (the trust anchor); the path from home down to p
// is checked for symlinked components, and p itself is checked for being a regular
// file with no group/world-writable bit — see openHardenedApprovalsFile.
//
// If the file parsed but ≥1 individual record was malformed and dropped (the
// valid records still apply), a SINGLE aggregate warning is emitted with the
// dropped COUNT and the file PATH — never the record contents/secrets.
func loadApprovalRecords(ctx context.Context, cache *hashcache.Cache[ApprovalsFile], home, p string) []ApprovalRecord {
	b, ok := readHardenedApprovalsFile(ctx, home, p)
	if !ok {
		return nil // absent (normal) or a rejected/unreadable file (already warned).
	}
	af, err := cache.Load(b)
	if err != nil {
		// Malformed file → behave as empty + warn (path only, never contents).
		slog.WarnContext(ctx, "tools: malformed approvals file; skipped (treated as empty)", "path", p, "err", err)
		return nil
	}
	if af.SkippedRecords > 0 {
		// Aggregate operability signal: how many records were dropped and from
		// where. NEVER the record contents (which could carry sensitive patterns).
		slog.WarnContext(ctx, "tools: skipped malformed approval records", "count", af.SkippedRecords, "path", p)
	}
	return af.Approvals
}

// readHardenedApprovalsFile reads p with the §3c READ-side store hardening,
// returning (bytes, true) only when every check passes. It returns (nil, false)
// for an absent file (the normal case — NO warning) and for any rejected or
// unreadable file (a single path-only warning, never contents). The checks, in
// order (all fail-secure → treat-as-empty):
//  1. no symlinked path component from home down to p (don't read through a
//     symlinked ~/.urvi or workspaces/<hash>);
//  2. open with O_RDONLY|O_NOFOLLOW so a SYMLINKED FILE p fails to open (rather
//     than following it) — closing the resolve→open TOCTOU window;
//  3. fstat the OPEN fd (not the path) and require a REGULAR file (reject a dir,
//     device, fifo, …) with NO group/world-writable bit (a non-owner could
//     otherwise tamper with the policy that auto-approves tool calls).
func readHardenedApprovalsFile(ctx context.Context, home, p string) ([]byte, bool) {
	if err := assertHardenedStorePath(home, p); err != nil {
		// A symlinked policy-path component OR a group/world-writable ancestor
		// DIRECTORY → don't read through it; treat as empty (a non-owner could
		// otherwise have planted the file via a loose ancestor dir).
		slog.WarnContext(ctx, "tools: approvals path has a symlinked or world-writable component; skipped (treated as empty)", "path", p, "err", err)
		return nil, false
	}

	// O_NOFOLLOW makes a symlink AT p fail to open (ELOOP) rather than be followed.
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- p = trusted home + fixed store names + a sha256 hash; not user input.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false // Absent store: normal, not an error, no warning.
		}
		slog.WarnContext(ctx, "tools: could not open approvals file; skipped (treated as empty)", "path", p, "err", err)
		return nil, false
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		slog.WarnContext(ctx, "tools: could not stat approvals file; skipped (treated as empty)", "path", p, "err", err)
		return nil, false
	}
	if !fi.Mode().IsRegular() {
		slog.WarnContext(ctx, "tools: approvals path is not a regular file; skipped (treated as empty)", "path", p)
		return nil, false
	}
	if fi.Mode().Perm()&groupWorldWritable != 0 {
		slog.WarnContext(ctx, "tools: approvals file is group- or world-writable; skipped (treated as empty)", "path", p)
		return nil, false
	}

	b, err := io.ReadAll(io.LimitReader(f, maxApprovalsFileBytes))
	if err != nil {
		slog.WarnContext(ctx, "tools: could not read approvals file; skipped (treated as empty)", "path", p, "err", err)
		return nil, false
	}
	return b, true
}

// maxApprovalsFileBytes caps the approvals file read so a pathological store file
// cannot exhaust memory. The store holds a small, human-edited rule list; 1 MiB is
// far beyond any legitimate size.
const maxApprovalsFileBytes int64 = 1 << 20

// loadApprovalRecordsForGrant reads the EXISTING valid records at p for Grant's
// load→append→write cycle, reusing the parseApprovalsFile parser directly (NOT the
// Check hashcache, which is for the read path). A missing, unreadable, or malformed
// file yields no records so a grant always succeeds (it then writes a clean,
// hardened file). The caller has ALREADY asserted no symlinked component and owns
// the dir; this open is O_NOFOLLOW so a symlinked file p is not followed either.
func loadApprovalRecordsForGrant(ctx context.Context, p string) []ApprovalRecord {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- p = trusted home + fixed store names + a sha256 hash; not user input.
	if err != nil {
		if !os.IsNotExist(err) {
			slog.WarnContext(ctx, "tools: could not open existing approvals for grant; starting fresh", "path", p, "err", err)
		}
		return nil
	}
	defer func() { _ = f.Close() }()

	b, err := io.ReadAll(io.LimitReader(f, maxApprovalsFileBytes))
	if err != nil {
		slog.WarnContext(ctx, "tools: could not read existing approvals for grant; starting fresh", "path", p, "err", err)
		return nil
	}
	af, err := parseApprovalsFile(b)
	if err != nil {
		slog.WarnContext(ctx, "tools: existing approvals malformed; starting fresh", "path", p, "err", err)
		return nil
	}
	return af.Approvals
}

// stageSessionPolicies evaluates the in-memory ToolPolicy list (Stage 6) with the
// same deny-beats-allow reduction. Each ToolPolicy is projected to ApprovalRecords
// (one per Match entry; an empty Match list = a single all-calls record) so the
// shared matcher/reducer applies uniformly.
func (c *PermissionChecker) stageSessionPolicies(toolName string, class toolClass, argsJSON string) (loop.Effect, bool) {
	matcher := c.recordMatcher(toolName, class, argsJSON)
	records := sessionPoliciesToRecords(c.policy.Policies)
	return reduceApprovalRecords(records, matcher)
}

// sessionPoliciesToRecords flattens session ToolPolicies into ApprovalRecords. A
// policy with no Match becomes a single empty-Match (all-calls) record; a policy
// with N matches becomes N records sharing the policy's tool+effect.
func sessionPoliciesToRecords(policies []loop.ToolPolicy) []ApprovalRecord {
	var out []ApprovalRecord
	for _, p := range policies {
		if len(p.Match) == 0 {
			out = append(out, ApprovalRecord{Tool: p.Tool, Effect: p.Effect})
			continue
		}
		for _, m := range p.Match {
			out = append(out, ApprovalRecord{Tool: p.Tool, Match: m, Effect: p.Effect})
		}
	}
	return out
}

// recordPredicate reports whether a stored approval record matches the live call.
type recordPredicate func(ApprovalRecord) bool

// reduceApprovalRecords applies DENY-BEATS-ALLOW over all records the predicate
// matches: if ANY matching record is EffectDeny → (EffectDeny, true); else if ANY
// matching record is EffectAutoApprove → (EffectAutoApprove, true); else
// (EffectAsk, false) to fall through. A matching record whose effect is the
// (rare) explicit "ask" is treated as a non-decision here — it neither denies nor
// approves, so evaluation continues (Stage 7 ultimately Asks anyway). This keeps
// the reducer a pure two-pass deny-then-allow scan.
func reduceApprovalRecords(records []ApprovalRecord, match recordPredicate) (loop.Effect, bool) {
	sawAllow := false
	for _, rec := range records {
		if !match(rec) {
			continue
		}
		if rec.Effect == loop.EffectDeny {
			return loop.EffectDeny, true // deny wins immediately.
		}
		if rec.Effect == loop.EffectAutoApprove {
			sawAllow = true
		}
	}
	if sawAllow {
		return loop.EffectAutoApprove, true
	}
	return loop.EffectAsk, false
}

// recordMatcher builds the per-call predicate that decides whether a stored
// record's Tool+Match applies to this live call. A record matches only if its
// Tool equals toolName; the Match interpretation is per the tool's class (file
// glob on the ws-relative path, exact/prefix Bash, Fetch grammar, WebSearch
// tool-level). An empty Match means "all calls of this tool". Any extraction
// failure makes the predicate reject every record (fail-secure: no match → no
// approval), so an unparseable live call can never be matched by a stored allow.
func (c *PermissionChecker) recordMatcher(toolName string, class toolClass, argsJSON string) recordPredicate {
	switch class {
	case classRead, classWrite, classUnknown:
		return c.pathRecordMatcher(toolName, readOrWriteBoundaryField(toolName, class), argsJSON)
	case classBash:
		return c.bashRecordMatcher(toolName, argsJSON)
	case classNetwork:
		return c.networkRecordMatcher(toolName, argsJSON)
	default:
		// Unreachable, but fail-secure: match nothing.
		return func(ApprovalRecord) bool { return false }
	}
}

// readOrWriteBoundaryField returns the path field used for record matching for a
// path-shaped class. Glob matches on "root", Grep/ReadFile on "path", write tools
// and unknown tools on "path".
func readOrWriteBoundaryField(toolName string, class toolClass) string {
	if class == classRead {
		return readBoundaryField(toolName)
	}
	return fieldPath
}

// pathRecordMatcher returns a predicate matching file-tool records by glob over
// the workspace-relative canonical path. If the live path cannot be extracted or
// contained, the predicate rejects every record (fail-secure). An empty record
// Match matches every call of the tool.
func (c *PermissionChecker) pathRecordMatcher(toolName, field, argsJSON string) recordPredicate {
	relPath, ok := c.workspaceRelPath(field, argsJSON)
	return func(rec ApprovalRecord) bool {
		if rec.Tool != toolName {
			return false
		}
		if rec.Match == "" {
			return true // all calls of this tool.
		}
		if !ok {
			return false // unparseable/uncontained live path → never matches.
		}
		return MatchFileGlob(rec.Match, relPath)
	}
}

// bashRecordMatcher returns a predicate matching Bash records against the live
// command (exact-normalized, or prefix when the record opts in). An unextractable
// command rejects every non-empty-Match record but still honours an empty-Match
// (all-Bash) record.
func (c *PermissionChecker) bashRecordMatcher(toolName, argsJSON string) recordPredicate {
	cmd, ok, _ := extractStringField(argsJSON, fieldCommand)
	return func(rec ApprovalRecord) bool {
		if rec.Tool != toolName {
			return false
		}
		if rec.Match == "" {
			return true
		}
		if !ok {
			return false
		}
		return MatchBash(rec.Match, rec.Prefix, cmd)
	}
}

// networkRecordMatcher returns a predicate for Fetch/WebSearch. WebSearch ignores
// Match (a grant is tool-level: any non-deny/allow record for the tool applies).
// Fetch interprets Match via the METHOD scheme://host[path] grammar; an
// unextractable url/method rejects every non-empty-Match record but honours an
// empty-Match (all-Fetch) record.
func (c *PermissionChecker) networkRecordMatcher(toolName, argsJSON string) recordPredicate {
	if toolName == toolWebSearch {
		return func(rec ApprovalRecord) bool { return rec.Tool == toolName }
	}
	// Fetch.
	u, uOK, _ := extractStringField(argsJSON, fieldURL)
	m, _, _ := extractStringField(argsJSON, fieldMethod)
	return func(rec ApprovalRecord) bool {
		if rec.Tool != toolName {
			return false
		}
		if rec.Match == "" {
			return true
		}
		if !uOK {
			return false
		}
		return MatchFetch(rec.Match, m, u)
	}
}

// workspaceRelPath extracts the path field, contains it, and relativises the
// resolved absolute path to the resolved workspace root — the canonical form the
// file-glob matcher expects. ok=false on any extraction or containment failure
// (the caller then never matches a non-empty-Match record). A missing search-dir
// field defaults to the workspace root ("." relative).
func (c *PermissionChecker) workspaceRelPath(field, argsJSON string) (string, bool) {
	raw, present, err := extractStringField(argsJSON, field)
	if err != nil {
		return "", false
	}
	if !present || raw == "" {
		raw = defaultSearchDir
	}
	abs, err := containedPath(c.policy.WorkspaceRoot, raw)
	if err != nil {
		return "", false
	}
	resolvedRoot, err := resolveAbsRoot(c.policy.WorkspaceRoot)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(resolvedRoot, abs)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// resolveAbsRoot resolves the workspace root the same way containedPath does
// (EvalSymlinks then Abs) so a Rel against it yields the canonical workspace-
// relative path.
func resolveAbsRoot(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// matchDeniedBashPrefix reports whether the normalized command begins with any
// normalized denied prefix. Both sides are normalized (trim + collapse internal
// whitespace) so "sudo   x" matches a "sudo" prefix and "dd if=/dev/sda" matches
// a "dd if=" prefix. The match is a bare string prefix (not word-aligned) so a
// partial-token entry like "dd if=" works; this deliberately over-denies (e.g.
// "sudoedit" matches "sudo"), which is fail-secure. This is the ADVISORY
// defense-in-depth Bash gate (design §4b: NOT a security boundary — trivially
// bypassable — but it runs in the non-bypassable hard-deny stage to catch obvious
// mistakes).
func matchDeniedBashPrefix(command string, prefixes []string) bool {
	nc := normalizeWhitespace(command)
	if nc == "" {
		return false
	}
	for _, p := range prefixes {
		np := normalizeWhitespace(p)
		if np == "" {
			continue
		}
		if strings.HasPrefix(nc, np) {
			return true
		}
	}
	return false
}

// matchHardDenyAbs reports whether the ABSOLUTE candidate path matches a
// hard-deny glob. The glob may be home-relative ("~/.ssh/**") or tree-anchored
// ("**/.env"). A leading "~/" is expanded to home (if home is non-empty);
// matching then strips the leading "/" from both the (expanded) pattern and the
// candidate so the shared matchGlob's "**" can consume leading segments (so
// "**/.env" matches "/ws/a/b/.env"). It is fail-secure: a "~/" glob with no home
// available simply does not match (rather than matching the wrong thing).
func matchHardDenyAbs(pattern, absPath, home string) bool {
	pat := pattern
	if strings.HasPrefix(pat, "~/") {
		if home == "" {
			return false // cannot anchor a home-relative glob → no match.
		}
		pat = path.Join(filepath.ToSlash(home), strings.TrimPrefix(pat, "~/"))
	}
	// Strip a single leading "/" from both sides so matchGlob's segment matcher
	// (which treats "**" as zero-or-more leading segments) aligns them.
	cand := strings.TrimPrefix(filepath.ToSlash(absPath), "/")
	pat = strings.TrimPrefix(pat, "/")
	return matchGlob(pat, cand)
}

// argsAreObject reports whether argsJSON decodes as a JSON object. An empty
// document is treated as the empty object "{}" (the runner's sanitized form for a
// call with no/invalid args). Anything else (a JSON array, scalar, or syntax
// error) is NOT an object — the call is malformed and must not be auto-approved.
func argsAreObject(argsJSON string) bool {
	if strings.TrimSpace(argsJSON) == "" {
		return true // empty == "{}".
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal([]byte(argsJSON), &obj) == nil
}

// extractStringField pulls a single string field out of a JSON object. It returns
// (value, present, error): present=false when the field is absent or JSON null;
// error != nil only when argsJSON is not a JSON object or the field is present but
// not a string. A non-object/invalid args document is an error (the caller fails
// secure). Using json.RawMessage avoids decoding the whole (possibly large/secret)
// args payload into typed structs — only the one boundary field is read.
func extractStringField(argsJSON, field string) (string, bool, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &obj); err != nil {
		return "", false, &ArgExtractError{Field: field, Reason: "args is not a JSON object", Err: err}
	}
	raw, present := obj[field]
	if !present {
		return "", false, nil
	}
	// A JSON null is treated as absent.
	if string(raw) == "null" {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, &ArgExtractError{Field: field, Reason: "field is not a string", Err: err}
	}
	return s, true, nil
}

// parseApprovalsFile is the strict, fail-secure parser the approval caches use.
// It uses a DisallowUnknownFields-free decode (a forward-compatible file may carry
// extra keys) but RELIES on Effect.UnmarshalJSON to reject any unknown effect
// string: a record with a bad effect makes the WHOLE-FILE decode fail, which the
// caller treats as "empty file" (fail-secure). For the design's "skip a single
// bad record, keep valid ones" behaviour, records are decoded ONE AT A TIME so a
// single bad effect drops only its record. A top-level JSON syntax error (a
// corrupt file) returns an error → empty stage.
func parseApprovalsFile(b []byte) (ApprovalsFile, error) {
	// First, decode the envelope with raw records so a single bad record does not
	// fail the whole file.
	var envelope struct {
		Version   int               `json:"version"`
		Approvals []json.RawMessage `json:"approvals"`
	}
	// Empty input is an empty (zero) file, not an error: an absent store already
	// short-circuits before here, but an explicitly-empty byte slice is benign.
	if len(strings.TrimSpace(string(b))) == 0 {
		return ApprovalsFile{}, nil
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return ApprovalsFile{}, &ApprovalsParseError{Reason: "approvals file is not valid JSON", Err: err}
	}

	out := ApprovalsFile{Version: envelope.Version}
	for _, rawRec := range envelope.Approvals {
		var rec ApprovalRecord
		if err := json.Unmarshal(rawRec, &rec); err != nil {
			// A single bad record (e.g. unknown effect via Effect.UnmarshalJSON, or
			// a non-string effect) is SKIPPED, not fatal. We drop the bad record so
			// valid records still apply, and only TALLY it here — we deliberately do
			// not warn per-record (which would risk logging record contents). The
			// loader (loadApprovalRecords) emits a single AGGREGATE count+path warn
			// when out.SkippedRecords > 0; a whole-file syntax error is warned
			// separately. A wholly-bad file yields zero records, landing Stage 5 at
			// Ask.
			out.SkippedRecords++
			continue
		}
		out.Approvals = append(out.Approvals, rec)
	}
	return out, nil
}

// ArgExtractError is the typed failure for boundary-field extraction from a tool
// call's args (a non-object document or a non-string field). It is fail-secure by
// contract: every caller treats a non-nil ArgExtractError as "cannot prove the
// boundary" and denies or refuses to match.
type ArgExtractError struct {
	Field  string // the field being extracted
	Reason string // non-secret reason
	Err    error  // underlying json error, may be nil
}

func (e *ArgExtractError) Error() string {
	if e.Err != nil {
		return "tools: arg extraction failed for field " + e.Field + ": " + e.Reason + ": " + e.Err.Error()
	}
	return "tools: arg extraction failed for field " + e.Field + ": " + e.Reason
}

func (e *ArgExtractError) Unwrap() error { return e.Err }

// ApprovalsParseError is the typed failure for a whole-file approvals parse (a
// corrupt JSON document). The caller treats it as an empty store (fail open to
// the next stage), never an auto-approve.
type ApprovalsParseError struct {
	Reason string // non-secret reason
	Err    error  // underlying json error, may be nil
}

func (e *ApprovalsParseError) Error() string {
	if e.Err != nil {
		return "tools: " + e.Reason + ": " + e.Err.Error()
	}
	return "tools: " + e.Reason
}

func (e *ApprovalsParseError) Unwrap() error { return e.Err }
