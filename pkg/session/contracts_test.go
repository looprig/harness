package session_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/session"
)

// publicSessionContracts are the contract interfaces pkg/session is allowed to
// export. Everything else it exports must be an error type: the package is
// contracts plus errors, and construction belongs to rig.
//
// GateHost is separate from the two session views on purpose — it is the
// integration host's end of a gate, not an operator's view of a session — and it
// is listed here rather than folded into SessionController for the reasons on the
// type itself.
//
// The RestoreDecider family is the restore-drift decision contract: the
// RestoreDecider interface, its RestoreDecision return vocabulary, and the two
// stateless fail-secure default policies (DefaultPolicyDecider / AcceptAllDecider)
// an application selects between. They are contract vocabulary, not session
// construction (which stays forbidden), so they belong on the contract boundary
// beside the error types.
var publicSessionContracts = map[string]bool{
	"Session": true, "SessionController": true, "GateHost": true,
	"RestoreDecider": true, "RestoreDecision": true,
	"RuntimeRestoreRequest": true, "RuntimeRestoreResolver": true,
	"DefaultPolicyDecider": true, "AcceptAllDecider": true,
	"CommittedPublicEventSource": true, "CommittedPublicEventProvider": true,
	"IdleWaiter": true, "Liveness": true, "Releaser": true,
}

var forbiddenSessionSurface = map[string]bool{
	"New": true, "Restore": true, "Compile": true, "Runner": true, "Option": true, "CompileOption": true,
}

func forbiddenSessionNames(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.FuncDecl:
			if node.Recv == nil && forbiddenSessionSurface[node.Name.Name] {
				names = append(names, node.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range node.Specs {
				switch named := spec.(type) {
				case *ast.TypeSpec:
					if forbiddenSessionSurface[named.Name.Name] {
						names = append(names, named.Name.Name)
					}
				case *ast.ValueSpec:
					for _, name := range named.Names {
						if forbiddenSessionSurface[name.Name] {
							names = append(names, name.Name)
						}
					}
				}
			}
		}
	}
	sort.Strings(names)
	return names
}

func forbiddenSessionDeclarations(filename, source string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), filename, source, 0)
	if err != nil {
		return nil, err
	}
	return forbiddenSessionNames(file), nil
}

// parseProductionGoFiles parses every production Go file in dir without applying
// the current platform's build constraints, so source guards cover inactive tags.
func parseProductionGoFiles(dir string, mode parser.Mode) ([]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]*ast.File, 0, len(entries))
	set := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		file, err := parser.ParseFile(set, filepath.Join(dir, name), nil, mode)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

func TestSessionBoundaryFileScanIncludesInactiveBuildTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inactive.go")
	source := "//go:build boundary_never\n\npackage session\n\nfunc New() {}\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := parseProductionGoFiles(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("parsed production files = %d, want 1 inactive-tag file", len(files))
	}
	if got := forbiddenSessionNames(files[0]); len(got) != 1 || got[0] != "New" {
		t.Fatalf("inactive-tag forbidden declarations = %v, want [New]", got)
	}
}

func TestPublicSessionContractsAreInterfaces(t *testing.T) {
	t.Parallel()
	var _ interface{ SessionID() uuid.UUID } = session.Session(nil)
	var _ session.Session = (session.SessionController)(nil)
}

func TestRestoreNoPrimerLoopWireValue(t *testing.T) {
	t.Parallel()
	if got, want := string(session.RestoreNoPrimerLoop), "no_primer_loop"; got != want {
		t.Fatalf("RestoreNoPrimerLoop = %q, want %q", got, want)
	}
}

func TestSessionBoundaryGuardRejectsExportedValueAliases(t *testing.T) {
	source := `package session
var New = func() {}
var Restore = New
var Compile = New
var Runner any
`
	got, err := forbiddenSessionDeclarations("fixture.go", source)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Compile", "New", "Restore", "Runner"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("forbidden declarations = %v, want %v", got, want)
	}
}

func TestPublicSessionContainsOnlyContractsAndErrors(t *testing.T) {
	t.Parallel()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	files, err := parseProductionGoFiles(dir, 0)
	if err != nil {
		t.Fatalf("parse pkg/session: %v", err)
	}
	for _, file := range files {
		if file.Name.Name != "session" {
			continue
		}
		for _, decl := range file.Decls {
			switch node := decl.(type) {
			case *ast.FuncDecl:
				if node.Recv == nil && ast.IsExported(node.Name.Name) {
					t.Errorf("pkg/session exports package function %s; construction and helpers belong to rig/internal runtime", node.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range node.Specs {
					switch named := spec.(type) {
					case *ast.TypeSpec:
						if !ast.IsExported(named.Name.Name) {
							continue
						}
						name := named.Name.Name
						if !publicSessionContracts[name] && !strings.HasSuffix(name, "Error") && !strings.HasSuffix(name, "ErrorKind") {
							t.Errorf("pkg/session exports non-contract, non-error type %s", name)
						}
					case *ast.ValueSpec:
						for _, name := range named.Names {
							if forbiddenSessionSurface[name.Name] {
								t.Errorf("pkg/session exports forbidden lifecycle value %s", name.Name)
							}
						}
					}
				}
			}
		}
	}
}

func TestOldLifecycleSurfaceIsAbsent(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "contracts_test.go" || len(entry.Name()) < 3 || entry.Name()[len(entry.Name())-3:] != ".go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range forbiddenSessionNames(file) {
			t.Errorf("old public lifecycle declaration %s remains in %s", name, entry.Name())
		}
	}
}

func TestSessionContractsDoNotExposeGenericHustleExecution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		contract   reflect.Type
		methodName string
	}{
		{name: "data plane has no generic runner", contract: reflect.TypeOf((*session.Session)(nil)).Elem(), methodName: "RunHustle"},
		{name: "controller has no generic runner", contract: reflect.TypeOf((*session.SessionController)(nil)).Elem(), methodName: "RunHustle"},
		{name: "data plane has no generic invoke", contract: reflect.TypeOf((*session.Session)(nil)).Elem(), methodName: "InvokeHustle"},
		{name: "controller has no generic invoke", contract: reflect.TypeOf((*session.SessionController)(nil)).Elem(), methodName: "InvokeHustle"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, exists := tt.contract.MethodByName(tt.methodName); exists {
				t.Fatalf("%s exposes forbidden generic hustle method %s", tt.contract.Name(), tt.methodName)
			}
		})
	}
}

func TestSessionContractsExposeOnlyFocusedCompaction(t *testing.T) {
	t.Parallel()
	dataPlane := reflect.TypeOf((*session.Session)(nil)).Elem()
	controller := reflect.TypeOf((*session.SessionController)(nil)).Elem()
	tests := []struct {
		name       string
		contract   reflect.Type
		methodName string
		want       bool
	}{
		{name: "session exposes active convenience", contract: dataPlane, methodName: "Compact", want: true},
		{name: "session exposes exact target", contract: dataPlane, methodName: "CompactToLoop", want: true},
		{name: "controller inherits active convenience", contract: controller, methodName: "Compact", want: true},
		{name: "controller inherits exact target", contract: controller, methodName: "CompactToLoop", want: true},
		{name: "session has no arbitrary rewrite", contract: dataPlane, methodName: "RewriteContext"},
		{name: "controller has no arbitrary rewrite", contract: controller, methodName: "RewriteContext"},
		{name: "session has no supplied summary", contract: dataPlane, methodName: "CompactWithSummary"},
		{name: "controller has no generic compaction runner", contract: controller, methodName: "RunCompaction"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, exists := tt.contract.MethodByName(tt.methodName)
			if exists != tt.want {
				t.Fatalf("%s method %s exists=%v, want %v", tt.contract.Name(), tt.methodName, exists, tt.want)
			}
		})
	}
}

// TestCommittedPublicEventCapabilityIsSegregated proves the committed-public-event
// capability is a SEPARATE contract, not three more methods every Session
// implementation must grow. The two session views keep exactly the compatibility
// SubscribeEvents that every TUI/CLI consumer already depends on; the committed
// stream — which a session without committed-bytes persistence cannot serve at all —
// is reachable only through the narrow source, discovered through the provider.
func TestCommittedPublicEventCapabilityIsSegregated(t *testing.T) {
	t.Parallel()
	dataPlane := reflect.TypeOf((*session.Session)(nil)).Elem()
	controller := reflect.TypeOf((*session.SessionController)(nil)).Elem()
	source := reflect.TypeOf((*session.CommittedPublicEventSource)(nil)).Elem()
	provider := reflect.TypeOf((*session.CommittedPublicEventProvider)(nil)).Elem()

	for _, view := range []reflect.Type{dataPlane, controller} {
		if _, exists := view.MethodByName("SubscribeCommittedPublicEvents"); exists {
			t.Errorf("%s exposes SubscribeCommittedPublicEvents; the capability must stay segregated", view.Name())
		}
		if _, exists := view.MethodByName("CommittedPublicEvents"); exists {
			t.Errorf("%s exposes CommittedPublicEvents; the capability must stay segregated", view.Name())
		}
		if _, exists := view.MethodByName("SubscribeEvents"); !exists {
			t.Errorf("%s lost the compatibility SubscribeEvents", view.Name())
		}
	}
	if source.NumMethod() != 1 {
		t.Errorf("CommittedPublicEventSource has %d methods, want exactly SubscribeCommittedPublicEvents", source.NumMethod())
	}
	if _, exists := source.MethodByName("SubscribeCommittedPublicEvents"); !exists {
		t.Error("CommittedPublicEventSource does not expose SubscribeCommittedPublicEvents")
	}
	if provider.NumMethod() != 1 {
		t.Errorf("CommittedPublicEventProvider has %d methods, want exactly CommittedPublicEvents", provider.NumMethod())
	}
	discover, exists := provider.MethodByName("CommittedPublicEvents")
	if !exists {
		t.Fatal("CommittedPublicEventProvider does not expose CommittedPublicEvents")
	}
	// Two results, the second a bool: the capability is DISCOVERED, never assumed.
	// A single-result form would force every provider to hand back a source it
	// cannot back with committed bytes.
	if discover.Type.NumOut() != 2 || discover.Type.Out(0) != source || discover.Type.Out(1).Kind() != reflect.Bool {
		t.Fatalf("CommittedPublicEvents signature = %v, want (CommittedPublicEventSource, bool)", discover.Type)
	}
}

// contractMethodSet renders an interface's method set as sorted
// "Name(params) results" strings. It reads the SHAPE only — names and
// signatures — so it works on a bare interface with no implementation
// anywhere, which is how a downstream consumer pins these contracts.
//
// This matters for the mutation that a maintainer would actually make. A
// coordinated rename (the interface method AND every implementation and call
// site in one gopls edit) leaves a compile-time satisfiability assertion green,
// because every site moved together. It does not touch the string literals
// below, so this guard still fails.
func contractMethodSet(contract reflect.Type) []string {
	set := make([]string, 0, contract.NumMethod())
	for i := range contract.NumMethod() {
		method := contract.Method(i)
		set = append(set, method.Name+strings.TrimPrefix(method.Type.String(), "func"))
	}
	sort.Strings(set)
	return set
}

// methodSetMatches is the single comparison every shape guard below runs: render
// the contract, join it, compare against the transcribed want. It is one function
// so that the drift test exercises the SAME comparison the real guard uses rather
// than a lookalike that could stay green while the real one rotted.
func methodSetMatches(contract reflect.Type, want []string) bool {
	return strings.Join(contractMethodSet(contract), ";") == strings.Join(want, ";")
}

// lifecycleCapabilityShape names one H4.1 capability and the method set the
// runbook says it has.
type lifecycleCapabilityShape struct {
	name     string
	contract reflect.Type
	want     []string
	// drifted is a fixture with the same intent and a different shape: the edit a
	// maintainer would coordinate across every site in one gopls action. It exists
	// so the guard's rejecting half is observed, not assumed.
	drifted reflect.Type
}

type renamedReleaser interface {
	Release(context.Context) error
}

type polledLiveness interface {
	Done() bool
}

type widenedIdleWaiter interface {
	WaitIdle(context.Context) error
	WaitBusy(context.Context) error
}

// lifecycleCapabilityShapes is the H4.1 oracle: the three method sets TRANSCRIBED
// from runbook 03-harness ("Add separate interfaces"), not read back from the
// declarations they check. Derived from the implementation it would agree with
// anything.
func lifecycleCapabilityShapes() []lifecycleCapabilityShape {
	return []lifecycleCapabilityShape{
		{
			name:     "IdleWaiter",
			contract: reflect.TypeFor[session.IdleWaiter](),
			want:     []string{"WaitIdle(context.Context) error"},
			drifted:  reflect.TypeFor[widenedIdleWaiter](),
		},
		{
			name:     "Liveness",
			contract: reflect.TypeFor[session.Liveness](),
			want:     []string{"Done() <-chan struct {}"},
			drifted:  reflect.TypeFor[polledLiveness](),
		},
		{
			name:     "Releaser",
			contract: reflect.TypeFor[session.Releaser](),
			want:     []string{"ReleaseResidency(context.Context) error"},
			drifted:  reflect.TypeFor[renamedReleaser](),
		},
	}
}

// TestSegregatedLifecycleCapabilityShapes pins the exact method set of each
// lifecycle capability. The want values are TRANSCRIBED from runbook 03-harness
// task H4.1, not read back from the types, so this is an oracle rather than a
// mirror of whatever the package currently declares.
//
// Two of the three shapes were settled on the merits and must not drift:
//   - Done returns a channel, not a poll. It is a broadcast a drain supervisor
//     selects on; an Alive(ctx) error poll is a different thing in kind.
//   - ReleaseResidency is named in full because residency release is NONTERMINAL.
//     The bare name Release loses the distinction from Shutdown, which durably
//     appends SessionStopped, at exactly the boundary where it matters.
func TestSegregatedLifecycleCapabilityShapes(t *testing.T) {
	t.Parallel()
	for _, tt := range lifecycleCapabilityShapes() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.contract.Kind() != reflect.Interface {
				t.Fatalf("session.%s kind = %v, want an interface", tt.name, tt.contract.Kind())
			}
			if !methodSetMatches(tt.contract, tt.want) {
				t.Fatalf("session.%s method set = %v, want %v", tt.name, contractMethodSet(tt.contract), tt.want)
			}
		})
	}
}

// TestLifecycleShapeGuardRejectsDriftedFixtures runs the REAL comparison — the
// same methodSetMatches, over the same rendering, against the same transcribed
// want — on a correct subject and a drifted one, and requires opposite verdicts.
//
// The positive arm is not decoration. An earlier form of this test asserted only
// that each drifted fixture compared UNEQUAL to the want, and that was
// tautological: replacing contractMethodSet's body with a constant left every arm
// green, because degrading the helper never makes an inequality assertion more
// likely to trip. Measured, not assumed. The positive arm is what couples this
// test to the helper's fidelity.
//
// Each drifted fixture is a coordinated edit the compiler cannot see once every
// site moves together: renaming ReleaseResidency to Release, replacing Done's
// broadcast channel with a poll, and widening a capability with a second method.
func TestLifecycleShapeGuardRejectsDriftedFixtures(t *testing.T) {
	t.Parallel()
	for _, tt := range lifecycleCapabilityShapes() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !methodSetMatches(tt.contract, tt.want) {
				t.Fatalf("the shape comparison rejected the real session.%s (%v); it cannot be trusted to accept anything", tt.name, contractMethodSet(tt.contract))
			}
			if methodSetMatches(tt.drifted, tt.want) {
				t.Fatalf("drifted fixture %v compared equal to %v; the shape guard cannot detect this coordinated edit", contractMethodSet(tt.drifted), tt.want)
			}
		})
	}
}

// TestSessionControllerNotWidenedForLifecycleCapabilities holds step 2 of H4.1:
// the capabilities are SEGREGATED and discovered by type assertion, exactly as
// runtimecommand.Provider and the committed-public-event capability above are.
// SessionController is not widened solely so a Host can reach them.
//
// The guard is deliberately superset-plus-exclusion rather than exact equality.
// Exact equality would also fail on an unrelated, legitimately additive method
// and would say nothing about WHY; what H4.1 owes is (a) nothing released is
// lost and (b) none of the three lifecycle methods appears on either view. An
// ADDED implemented method is not a widening "solely for Host": it is an ordinary
// API addition, and the segregation arm below is what rejects the widening this
// task actually forbids.
//
// The released half compares full SIGNATURES, not names. Names alone let a
// source-INCOMPATIBLE change to a released method pass: narrowing
// Interrupt(context.Context) (bool, error) to Interrupt(context.Context) error
// leaves every name intact, and coordinated with the runtime it compiles.
func TestSessionControllerNotWidenedForLifecycleCapabilities(t *testing.T) {
	t.Parallel()
	dataPlane := reflect.TypeFor[session.Session]()
	controller := reflect.TypeFor[session.SessionController]()

	// Transcribed from released harness v0.30.2 plus the current data-plane
	// declaration; source compatibility means every one of these survives with the
	// signature it shipped with.
	sessionMethods := []string{
		"ActiveLoop() loop.Handle",
		"Compact(context.Context) (uuid.UUID, error)",
		"CompactToLoop(context.Context, uuid.UUID) (uuid.UUID, error)",
		"Interrupt(context.Context) (bool, error)",
		"Loop(uuid.UUID) (loop.Handle, bool)",
		"RespondGate(context.Context, gate.GateResponse) error",
		"SessionID() uuid.UUID",
		"Submit(context.Context, []content.Block) (uuid.UUID, error)",
		"SubmitToLoop(context.Context, uuid.UUID, []content.Block) (uuid.UUID, error)",
		"SubscribeEvents(event.EventFilter) (event.Subscription, error)",
	}
	controllerMethods := append([]string{
		"CheckpointWorkspace(context.Context) (workspacestore.Ref, error)",
		"LoopController(uuid.UUID) (loop.Controller, bool)",
		"RestoreWorkspace(context.Context, workspacestore.Ref) error",
		"SetActiveLoop(context.Context, uuid.UUID) error",
		"Shutdown(context.Context) error",
	}, sessionMethods...)

	released := map[reflect.Type][]string{dataPlane: sessionMethods, controller: controllerMethods}
	for view, want := range released {
		present := make(map[string]bool, view.NumMethod())
		for _, signature := range contractMethodSet(view) {
			present[signature] = true
		}
		for _, signature := range want {
			if !present[signature] {
				t.Errorf("%s no longer declares released method %s; it has %v", view.Name(), signature, contractMethodSet(view))
			}
		}
	}

	segregated := []string{"WaitIdle", "Done", "ReleaseResidency"}
	for _, view := range []reflect.Type{dataPlane, controller} {
		for _, name := range segregated {
			if _, exists := view.MethodByName(name); exists {
				t.Errorf("%s exposes %s; the lifecycle capability must stay segregated and be discovered by assertion", view.Name(), name)
			}
		}
	}
}

// TestProductionSessionSatisfiesIdleAndLiveness asserts the capability at RUN
// time via reflect rather than with a compile-time var _ assertion. A compile
// error is not an assertion kill: it stops the test binary from existing, so
// `go test -list` reports nothing and no detector is exercised. Implements
// returning false is a real failing assertion.
func TestProductionSessionSatisfiesIdleAndLiveness(t *testing.T) {
	t.Parallel()
	production := reflect.TypeFor[*sessionruntime.Session]()
	for _, tt := range []struct {
		name     string
		contract reflect.Type
	}{
		{name: "IdleWaiter", contract: reflect.TypeFor[session.IdleWaiter]()},
		{name: "Liveness", contract: reflect.TypeFor[session.Liveness]()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !production.Implements(tt.contract) {
				t.Fatalf("production *sessionruntime.Session does not satisfy session.%s", tt.name)
			}
		})
	}
}

// TestLifecycleSatisfactionGuardDetectsAMissingMethod is the negative control
// for the test above, which would otherwise assert a property its subject
// already has with a detector nobody has seen reject anything.
func TestLifecycleSatisfactionGuardDetectsAMissingMethod(t *testing.T) {
	t.Parallel()
	notASession := reflect.TypeFor[*struct{}]()
	for _, tt := range []struct {
		name     string
		contract reflect.Type
	}{
		{name: "IdleWaiter", contract: reflect.TypeFor[session.IdleWaiter]()},
		{name: "Liveness", contract: reflect.TypeFor[session.Liveness]()},
		{name: "Releaser", contract: reflect.TypeFor[session.Releaser]()},
	} {
		if notASession.Implements(tt.contract) {
			t.Errorf("*struct{} reported as satisfying session.%s; the satisfaction guard cannot reject anything", tt.name)
		}
	}
}

// TestProductionSessionDoesNotYetReleaseResidency pins a KNOWN GAP, in the same
// spirit as the sessionstore interrupt-settlement pin.
//
// H4.1 exports capability interfaces over behavior that already exists. WaitIdle
// and Done do exist on the production Session. A nonterminal residency release
// does NOT: the only teardown the runtime has is Shutdown, which durably appends
// SessionStopped and is therefore terminal by construction. Building one is task
// H4.2, and Host's O3.1 step 5 — a registry loser releasing its runtime
// nonterminally — is blocked behind H4.2, not behind this task.
//
// This assertion is expected to FAIL when H4.2 lands. That is its purpose, and the
// failure MESSAGE carries the instruction, not this comment: a maintainer meeting a
// red test reads the string the test printed, not the prose above the function.
//
// The observation is only that the method now exists. The message says so rather
// than asserting H4.2 as the cause, because any future type gaining that name
// would trip it identically.
func TestProductionSessionDoesNotYetReleaseResidency(t *testing.T) {
	t.Parallel()
	production := reflect.TypeFor[*sessionruntime.Session]()
	if production.Implements(reflect.TypeFor[session.Releaser]()) {
		t.Fatal("production *sessionruntime.Session now declares ReleaseResidency, so it satisfies session.Releaser. If that is H4.2's nonterminal release, move the type into TestProductionSessionSatisfiesIdleAndLiveness and delete this gap pin; if it is anything else, the method name is wrong.")
	}
	if _, exists := reflect.TypeFor[session.Releaser]().MethodByName("ReleaseResidency"); !exists {
		t.Fatal("session.Releaser lost ReleaseResidency; the gap pin above is vacuous")
	}
}
