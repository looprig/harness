package rig

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// externalStore is written the way a composition root outside harness has to
// write one: it names pkg/loop and the standard library only. If the option's
// parameter type were an internal one this file could still compile — it is in
// the same module — so the mechanical proof is
// TestPublicCaptureWiringNamesNoInternalPackage below, not this type.
type externalStore struct{ objects map[string][]byte }

func newExternalStore() *externalStore { return &externalStore{objects: map[string][]byte{}} }

func (s *externalStore) PutToolResultObject(_ context.Context, objectID string, content []byte) error {
	s.objects[objectID] = append([]byte(nil), content...)
	return nil
}

func (s *externalStore) StatToolResultObject(_ context.Context, objectID string) (loop.ToolResultObjectStat, error) {
	return loop.ToolResultObjectStat{SizeBytes: uint64(len(s.objects[objectID]))}, nil
}

// TestWithToolResultCaptureRejectsAnUnusableWiring enumerates every way the
// public option can be called wrong. Each rejection is at Define time rather
// than at the first oversized tool result, which is the difference between a
// misconfiguration and a silently unretained turn.
func TestWithToolResultCaptureRejectsAnUnusableWiring(t *testing.T) {
	t.Parallel()
	absolute := t.TempDir()
	tests := []struct {
		name    string
		options []Option
		want    DefinitionErrorKind
	}{
		{name: "nil store", options: []Option{WithToolResultCapture(nil, absolute)}, want: DefinitionInvalidToolResultCapture},
		{name: "empty spill base", options: []Option{WithToolResultCapture(newExternalStore(), "")}, want: DefinitionInvalidToolResultCapture},
		{name: "blank spill base", options: []Option{WithToolResultCapture(newExternalStore(), "   ")}, want: DefinitionInvalidToolResultCapture},
		{name: "relative spill base", options: []Option{WithToolResultCapture(newExternalStore(), "spills")}, want: DefinitionInvalidToolResultCapture},
		{
			name:    "duplicate option",
			options: []Option{WithToolResultCapture(newExternalStore(), absolute), WithToolResultCapture(newExternalStore(), absolute)},
			want:    DefinitionDuplicateOption,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := defineWith(t, sessionStoreT(t), tt.options...)
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != tt.want {
				t.Fatalf("Define error = %v, want kind %q", err, tt.want)
			}
		})
	}
	if _, err := defineWith(t, sessionStoreT(t), WithToolResultCapture(newExternalStore(), absolute)); err != nil {
		t.Fatalf("a valid capture wiring was rejected: %v", err)
	}
}

// TestToolResultSpillBaseMayNotOverlapTheWorkspaceRegion is the mechanism that
// keeps a capture spill out of every workspace checkpoint. A checkpoint archives
// the whole workspace region, so exclusion cannot be a walk filter that a later
// snapshot path might not consult — it is a placement invariant, refused at
// Define in EVERY overlapping arrangement and accepted in the disjoint one.
func TestToolResultSpillBaseMayNotOverlapTheWorkspaceRegion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		spill   func(root, sibling string) string
		wantErr bool
	}{
		{name: "spill inside the workspace", spill: func(root, _ string) string { return filepath.Join(root, "spills") }, wantErr: true},
		{name: "spill deep inside the workspace", spill: func(root, _ string) string { return filepath.Join(root, "a", "b", "spills") }, wantErr: true},
		{name: "spill IS the workspace", spill: func(root, _ string) string { return root }, wantErr: true},
		{name: "workspace inside the spill base", spill: func(root, _ string) string { return filepath.Dir(root) }, wantErr: true},
		{name: "disjoint sibling", spill: func(_, sibling string) string { return sibling }},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			root := filepath.Join(parent, "workspace")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatalf("mkdir workspace: %v", err)
			}
			sibling := t.TempDir()
			_, err := defineWith(t, sessionStoreT(t),
				WithSharedWorkspace(wsStoreT(t), root),
				WithSnapshots(SnapshotPolicy{Trigger: SnapshotOnTurnDone, Priority: SnapshotBestEffort}),
				WithToolResultCapture(newExternalStore(), tt.spill(root, sibling)),
			)
			if tt.wantErr {
				var definitionErr *DefinitionError
				if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionToolResultSpillOverlapsWorkspace {
					t.Fatalf("Define error = %v, want kind %q", err, DefinitionToolResultSpillOverlapsWorkspace)
				}
				return
			}
			if err != nil {
				t.Fatalf("a disjoint spill base was rejected: %v", err)
			}
		})
	}
}

// TestAWorkspaceCheckpointWouldCaptureAnInTreeSpill proves the OTHER direction
// of the exclusion: the refusal above is load-bearing because a checkpoint really
// does archive everything under the region. Without this, "excluded from
// checkpoints" would be a claim about a snapshot that might not have contained it
// anyway.
func TestAWorkspaceCheckpointWouldCaptureAnInTreeSpill(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("work"), 0o600); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}
	inTree := filepath.Join(root, "spills")
	if err := os.MkdirAll(inTree, 0o700); err != nil {
		t.Fatalf("mkdir in-tree spill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(inTree, "abc.capture"), []byte("raw tool output"), 0o600); err != nil {
		t.Fatalf("write spill: %v", err)
	}
	ws := wsStoreT(t)
	ref, err := ws.Snapshot(context.Background(), root)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	restored := t.TempDir()
	if err := ws.Materialize(context.Background(), ref, restored); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	captured, err := os.ReadFile(filepath.Join(restored, "spills", "abc.capture"))
	if err != nil {
		t.Fatalf("an in-tree spill was expected in the checkpoint but is absent: %v", err)
	}
	if string(captured) != "raw tool output" {
		t.Fatalf("checkpointed spill = %q, want the raw tool output", captured)
	}
}

// TestRigCaptureSafetyProjectsEveryLoopsToolDefinitions holds the descriptor to
// the assembly it describes: every tool definition of every loop appears exactly
// once, streaming and materialized are told apart by the declaration rather than
// by the name, and the finite materialized maximum is the runtime's.
func TestRigCaptureSafetyProjectsEveryLoopsToolDefinitions(t *testing.T) {
	t.Parallel()
	streaming := declaredTool{Definition: tool.NewDefinition("streamer", 0, nilFactory), safety: tool.DeclaredCaptureSafety{Streaming: true, HighOutput: true}}
	bulky := declaredTool{Definition: tool.NewDefinition("bulky", 0, nilFactory), safety: tool.DeclaredCaptureSafety{HighOutput: true}}
	quiet := tool.NewDefinition("quiet", 0, nilFactory)
	primary, err := loop.Define(
		loop.WithName(identity.AgentName("planner")),
		loop.WithInference(&stubLLM{}, validModel("planner")),
		loop.WithTools(streaming, quiet),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	// modeOnly is reachable ONLY through a declared mode. A projection that read
	// the base tool set alone would leave a high-output tool out of the
	// descriptor, which is precisely the tool a placement decision needs to see.
	modeOnly := declaredTool{Definition: tool.NewDefinition("mode-only", 0, nilFactory), safety: tool.DeclaredCaptureSafety{HighOutput: true}}
	secondary, err := loop.Define(
		loop.WithName(identity.AgentName("worker")),
		loop.WithInference(&stubLLM{}, validModel("worker")),
		loop.WithTools(bulky, quiet),
		loop.WithModes(loop.Mode{Name: "deep", Tools: []tool.Definition{bulky, modeOnly}}),
		loop.WithInitialMode("deep"),
	)
	if err != nil {
		t.Fatalf("loop.Define: %v", err)
	}
	rig, err := Define(WithLoops(primary, secondary), WithPrimers("planner", "worker"), WithActivePrimer("planner"), WithSessionStore(sessionStoreT(t)))
	if err != nil {
		t.Fatalf("Define: %v", err)
	}
	descriptor := rig.CaptureSafety()
	var names []string
	classes := map[string]tool.CaptureClass{}
	high := map[string]bool{}
	for _, row := range descriptor.Definitions {
		names = append(names, row.Definition)
		classes[row.Definition] = row.Class
		high[row.Definition] = row.HighOutput
	}
	if got := strings.Join(names, ","); got != "bulky,mode-only,quiet,streamer" {
		t.Fatalf("definitions = %q, want each tool definition exactly once, sorted", got)
	}
	if classes["streamer"] != tool.CaptureClassStreaming {
		t.Errorf("streamer class = %q, want streaming", classes["streamer"])
	}
	if classes["bulky"] != tool.CaptureClassMaterialized || classes["quiet"] != tool.CaptureClassMaterialized {
		t.Errorf("classes = %v, want the undeclared and materialized tools classified materialized", classes)
	}
	if !high["streamer"] || !high["bulky"] || !high["mode-only"] || high["quiet"] {
		t.Errorf("high output = %v, want exactly the three declared ones", high)
	}
	if classes["mode-only"] != tool.CaptureClassMaterialized {
		t.Errorf("mode-only class = %q, want materialized", classes["mode-only"])
	}
	if descriptor.MaterializedMaxBytes != loop.DefaultMaterializedToolResultBytes {
		t.Fatalf("MaterializedMaxBytes = %d, want %d", descriptor.MaterializedMaxBytes, loop.DefaultMaterializedToolResultBytes)
	}
	if !descriptor.Safe() {
		t.Fatalf("Safe = false, want true: %v", descriptor.Unsafe())
	}
}

func nilFactory(context.Context, tool.Bindings) ([]tool.InvokableTool, error) { return nil, nil }

type declaredTool struct {
	tool.Definition
	safety tool.DeclaredCaptureSafety
}

func (d declaredTool) DeclaredCaptureSafety() tool.DeclaredCaptureSafety { return d.safety }

// TestPublicCaptureWiringNamesNoInternalPackage is the mechanical form of the
// carry-forward this task exists to discharge. A test inside this module cannot
// FAIL to compile against an internal type — every package here may import
// internal/ — so the property is proved by reading the source: no EXPORTED
// declaration of pkg/rig or pkg/loop may mention an identifier that came from an
// internal package, because a consumer outside github.com/looprig/harness could
// not write the call.
//
// The declaration set is derived by parsing the packages rather than by naming
// the capture functions, so it also holds every exported declaration those
// packages gain later.
func TestPublicCaptureWiringNamesNoInternalPackage(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, dir := range []string{".", filepath.Join("..", "loop")} {
		names, count, err := exportedDeclarationsNamingInternalPackages(dir)
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
		checked += count
		if len(names) != 0 {
			t.Errorf("%s: exported declarations name an internal package and cannot be called from outside the module: %v", dir, names)
		}
	}
	if checked < 40 {
		t.Fatalf("checked %d exported declarations; the guard is vacuous", checked)
	}
}

// exportedDeclarationsNamingInternalPackages parses every production file in dir
// and returns the exported declarations whose PUBLIC surface mentions a type
// from an internal import — a function parameter or result, an exported struct
// field, an exported interface method, or an exported variable's type. It also
// returns how many exported declarations it inspected, so a caller can refuse a
// vacuous run.
//
// It deliberately ignores unexported fields and unexported declarations: those
// are not nameable from outside the module either way, so an internal type there
// is not a boundary violation. The file set is derived by reading the directory,
// so a file added later is covered without editing this helper.
func exportedDeclarationsNamingInternalPackages(dir string) ([]string, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	var offenders []string
	inspected := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if parseErr != nil {
			return nil, 0, parseErr
		}
		internal := internalImportNames(file)
		if len(internal) == 0 {
			// Still count this file's exported declarations: a package whose
			// files import nothing internal is exactly the state being asserted,
			// and skipping them would understate the guard's reach.
			inspected += countExportedDeclarations(file)
			continue
		}
		for _, decl := range file.Decls {
			for _, found := range exportedSurfaceNaming(decl, internal, &inspected) {
				offenders = append(offenders, name+": "+found)
			}
		}
	}
	sort.Strings(offenders)
	return offenders, inspected, nil
}

// internalImportNames maps each import's local package identifier to true when
// its path crosses an internal boundary.
func internalImportNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		if !strings.Contains(path, "/internal/") && !strings.HasSuffix(path, "/internal") {
			continue
		}
		local := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			local = spec.Name.Name
		}
		names[local] = true
	}
	return names
}

func countExportedDeclarations(file *ast.File) int {
	count := 0
	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.FuncDecl:
			if node.Recv == nil && node.Name.IsExported() {
				count++
			}
		case *ast.GenDecl:
			for _, spec := range node.Specs {
				switch named := spec.(type) {
				case *ast.TypeSpec:
					if named.Name.IsExported() {
						count++
					}
				case *ast.ValueSpec:
					for _, ident := range named.Names {
						if ident.IsExported() {
							count++
						}
					}
				}
			}
		}
	}
	return count
}

// exportedSurfaceNaming reports the exported declarations in decl whose public
// surface names one of the internal packages.
func exportedSurfaceNaming(decl ast.Decl, internal map[string]bool, inspected *int) []string {
	var found []string
	namesInternal := func(expr ast.Expr) bool {
		hit := false
		ast.Inspect(expr, func(n ast.Node) bool {
			selector, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, isIdent := selector.X.(*ast.Ident); isIdent && internal[pkg.Name] {
				hit = true
			}
			return true
		})
		return hit
	}
	switch node := decl.(type) {
	case *ast.FuncDecl:
		if node.Recv != nil || !node.Name.IsExported() {
			return nil
		}
		*inspected++
		if namesInternal(node.Type) {
			found = append(found, "func "+node.Name.Name)
		}
	case *ast.GenDecl:
		for _, spec := range node.Specs {
			switch named := spec.(type) {
			case *ast.TypeSpec:
				if !named.Name.IsExported() {
					continue
				}
				*inspected++
				switch underlying := named.Type.(type) {
				case *ast.StructType:
					for _, field := range underlying.Fields.List {
						if !exportedFieldNames(field) {
							continue
						}
						if namesInternal(field.Type) {
							found = append(found, "type "+named.Name.Name+" field")
						}
					}
				case *ast.InterfaceType:
					for _, method := range underlying.Methods.List {
						if exportedFieldNames(method) && namesInternal(method.Type) {
							found = append(found, "type "+named.Name.Name+" method")
						}
					}
				default:
					if namesInternal(named.Type) {
						found = append(found, "type "+named.Name.Name)
					}
				}
			case *ast.ValueSpec:
				for _, ident := range named.Names {
					if !ident.IsExported() {
						continue
					}
					*inspected++
					if named.Type != nil && namesInternal(named.Type) {
						found = append(found, "var "+ident.Name)
					}
				}
			}
		}
	}
	return found
}

// exportedFieldNames reports whether a struct field or interface method is
// exported. An anonymous (embedded) field is treated as exported, because
// embedding an internal type promotes its methods onto the public type.
func exportedFieldNames(field *ast.Field) bool {
	if len(field.Names) == 0 {
		return true
	}
	for _, name := range field.Names {
		if name.IsExported() {
			return true
		}
	}
	return false
}
