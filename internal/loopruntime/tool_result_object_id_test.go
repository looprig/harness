package loopruntime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// captureObjectIDGrammar is the complete shape a minted capture object identity
// may take. Asserting the WHOLE string against it (rather than asserting the
// absence of a few characters) is what makes the opacity claim checkable: any
// URL, credential, bucket key or filesystem path contains at least one character
// outside this alphabet or changes the length.
var captureObjectIDGrammar = regexp.MustCompile(`\A` + regexp.QuoteMeta(captureObjectIDPrefix) + `[0-9a-f]{` + strconv.Itoa(captureObjectIDHexLen) + `}\z`)

// TestCaptureObjectIDIsOpaqueForEveryResultContent enumerates result payloads
// chosen to be exactly the things ObjectReference's contract forbids in an
// object_id — a signed URL, a credential, a backend key, a filesystem path — plus
// the degenerate and non-UTF-8 cases. The identity is derived from a digest, so
// none of them can travel into it; the enumeration is what turns that reasoning
// into a probe.
func TestCaptureObjectIDIsOpaqueForEveryResultContent(t *testing.T) {
	t.Parallel()
	payloads := []string{
		"",
		"plain text",
		"https://bucket.s3.amazonaws.test/sessions/s1/objects/o1?X-Amz-Signature=deadbeef",
		"AKIAIOSFODNN7EXAMPLE/wJalrXUtnFEMI",
		"sessions/tenant-1/session-2/tool-results/0001.bin",
		"/var/folders/tmp/looprig-spill-1234/result",
		"\xff\xfe\x00binary",
		strings.Repeat("A", 4096),
	}
	seen := map[string]string{}
	for _, payload := range payloads {
		sink := newCaptureSink(1 << 20)
		if _, err := sink.Write([]byte(payload)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		id := captureObjectID(sink.digestHex())
		if !captureObjectIDGrammar.MatchString(id) {
			t.Errorf("captureObjectID for %q = %q, which is outside the opaque grammar", payload, id)
		}
		if previous, duplicate := seen[id]; duplicate {
			t.Errorf("distinct payloads %q and %q minted the same identity %q", previous, payload, id)
		}
		seen[id] = payload
	}
}

// TestCaptureObjectIDIsContentAddressed pins the property the store relies on for
// Put idempotence: equal retained bytes mint an equal identity, and the identity
// depends on the RETAINED prefix rather than on what the producer offered.
func TestCaptureObjectIDIsContentAddressed(t *testing.T) {
	t.Parallel()
	first := newCaptureSink(4)
	second := newCaptureSink(4)
	if _, err := first.Write([]byte("abcd")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := second.Write([]byte("abcdEFGH")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := captureObjectID(second.digestHex()), captureObjectID(first.digestHex()); got != want {
		t.Fatalf("identity for the same retained prefix = %q, want %q", got, want)
	}
}

// objectIDAssignment is one place in the source where a value is placed into an
// ObjectReference.ObjectID field.
type objectIDAssignment struct {
	pos    token.Position
	minted bool // the value is a captureObjectID(...) call
}

// findObjectIDAssignments reports every place file assigns ObjectReference's
// ObjectID field — through a composite literal key or through a selector
// assignment — and whether the assigned value came from captureObjectID.
//
// The two arms are matched differently on purpose. A composite literal must also
// NAME the type — any expression whose final identifier is ObjectReference, so an
// alias or a dot import still matches, while an unrelated struct that happens to
// have an ObjectID field (pkg/sessionstore's DurableBodyTooLargeError does) does
// not. A literal with an elided type, which only occurs inside a slice or map of
// the named type, is matched on the field name alone. A selector assignment
// carries no type information at all, so it is matched on the field name: that is
// deliberately wide, because post-construction mutation is the other route into
// object_id, and a false positive there is a loud failure to triage rather than a
// silent pass.
func findObjectIDAssignments(fset *token.FileSet, file *ast.File) []objectIDAssignment {
	var found []objectIDAssignment
	isMint := func(value ast.Expr) bool {
		call, ok := value.(*ast.CallExpr)
		if !ok {
			return false
		}
		name, ok := call.Fun.(*ast.Ident)
		return ok && name.Name == "captureObjectID"
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if !namesObjectReference(node.Type) {
				return true
			}
			for _, element := range node.Elts {
				kv, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "ObjectID" {
					continue
				}
				found = append(found, objectIDAssignment{pos: fset.Position(kv.Pos()), minted: isMint(kv.Value)})
			}
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				selector, ok := lhs.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "ObjectID" {
					continue
				}
				var value ast.Expr
				if i < len(node.Rhs) {
					value = node.Rhs[i]
				}
				found = append(found, objectIDAssignment{pos: fset.Position(lhs.Pos()), minted: value != nil && isMint(value)})
			}
		}
		return true
	})
	return found
}

// TestToolResultCaptureObjectIDDetectorSeesAnUnmintedConstruction proves the detector in
// BOTH directions against source that did not exist when it was written: a
// composite literal that bypasses captureObjectID is reported as unminted, and
// the same literal built through captureObjectID is reported as minted. Without
// this, a guard that simply never matched anything would pass.
func TestToolResultCaptureObjectIDDetectorSeesAnUnmintedConstruction(t *testing.T) {
	t.Parallel()
	const src = `package p

import sessionwire "github.com/looprig/core/sessionwire/v1"

func bypass() *sessionwire.ObjectReference {
	return &sessionwire.ObjectReference{ObjectID: "https://signed.example/o?sig=1"}
}

func minted(digest string) sessionwire.ObjectReference {
	return sessionwire.ObjectReference{ObjectID: captureObjectID(digest)}
}

func mutate(r *sessionwire.ObjectReference, raw string) {
	r.ObjectID = raw
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := findObjectIDAssignments(fset, file)
	if len(found) != 3 {
		t.Fatalf("assignments found = %d, want 3", len(found))
	}
	want := []bool{false, true, false}
	for i, assignment := range found {
		if assignment.minted != want[i] {
			t.Errorf("assignment %d at %v minted = %v, want %v", i, assignment.pos, assignment.minted, want[i])
		}
	}
}

// TestToolResultCaptureObjectIDIsMintedOnlyByCaptureObjectID is the opacity guard for the module
// as it actually is. It derives its own file set by walking the module root, so a
// file added after it was written is covered without editing it — the failure
// mode of a guard that names its own subject.
func TestToolResultCaptureObjectIDIsMintedOnlyByCaptureObjectID(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	fset := token.NewFileSet()
	scanned := 0
	total := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != root && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, assignment := range findObjectIDAssignments(fset, file) {
			total++
			if !assignment.minted {
				t.Errorf("%v: ObjectID is assigned a value that did not come from captureObjectID; "+
					"the wire contract cannot check object_id opacity, so this producer is its only guarantor", assignment.pos)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned < 100 {
		t.Fatalf("scanned %d production files, want the whole module; the walk is not reaching it", scanned)
	}
	if total == 0 {
		t.Fatal("no ObjectID assignment found anywhere in production code; the guard is vacuous")
	}
}

// namesObjectReference reports whether a composite literal's type expression
// names ObjectReference. A nil type is an elided element literal, which only
// occurs inside a literal of the surrounding type, so it is left to the field
// name to decide.
func namesObjectReference(expr ast.Expr) bool {
	switch node := expr.(type) {
	case nil:
		return true
	case *ast.Ident:
		return node.Name == "ObjectReference"
	case *ast.SelectorExpr:
		return node.Sel.Name == "ObjectReference"
	case *ast.StarExpr:
		return namesObjectReference(node.X)
	case *ast.ArrayType:
		return namesObjectReference(node.Elt)
	case *ast.MapType:
		return namesObjectReference(node.Value)
	default:
		return false
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
