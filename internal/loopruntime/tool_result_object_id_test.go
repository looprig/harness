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
	pos        token.Position
	minted     bool // the value is a captureObjectID(...) call
	positional bool // the value was supplied by an unkeyed composite literal element
}

// findObjectIDAssignments reports every place file supplies a value for
// ObjectReference's ObjectID field — through a composite literal key, through an
// UNKEYED composite literal element, or through a selector assignment — and
// whether that value came from captureObjectID. names is the set of bare type
// names that stand for ObjectReference, as returned by objectReferenceTypeNames.
//
// The three arms are matched differently on purpose. A keyed composite literal
// must also NAME the type: any expression whose final identifier is in names, so
// a dot import, a same-named alias and a RENAMING alias declared anywhere in the
// scanned file set all match, while an unrelated struct that happens to have an
// ObjectID field (pkg/sessionstore's DurableBodyTooLargeError does) does not. A
// literal with an elided type, which only occurs inside a slice or map of the
// named type, is matched on the field name alone. An unkeyed element carries no
// field name at all, so it is matched on the literal's type alone — and only when
// that type is written as a plain (possibly pointer, possibly qualified) name,
// never for an elided, slice or map literal, whose elements are values rather
// than fields. Because field order is not resolved here, every unkeyed element of
// such a literal is reported; ObjectReference has one field today, so that is
// exact, and a second field would turn the extra element into a loud failure to
// triage rather than a silent pass. A selector assignment carries no type
// information at all, so it is matched on the field name: that is deliberately
// wide, for the same reason, because post-construction mutation is the other
// route into object_id.
func findObjectIDAssignments(fset *token.FileSet, file *ast.File, names map[string]bool) []objectIDAssignment {
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
			if !namesObjectReference(node.Type, names) {
				return true
			}
			unkeyed := namesObjectReferenceStruct(node.Type, names)
			for _, element := range node.Elts {
				kv, ok := element.(*ast.KeyValueExpr)
				if !ok {
					if unkeyed {
						found = append(found, objectIDAssignment{
							pos:        fset.Position(element.Pos()),
							minted:     isMint(element),
							positional: true,
						})
					}
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
// BOTH directions against source that did not exist when it was written: for each
// construction that reaches object_id, a form that bypasses captureObjectID is
// reported as unminted and the same form built through captureObjectID is
// reported as minted. Without this, a guard that simply never matched anything
// would pass.
//
// The constructions are enumerated rather than reasoned about, because each one
// escaped an earlier version of the detector for its own reason: the UNKEYED
// literal because the composite arm skipped every element that was not a
// KeyValueExpr, and the RENAMING alias because the type matcher compared the
// final identifier against the single name "ObjectReference". The slice case is
// here to hold the boundary from the other side — its elements are values, not
// fields, so the unkeyed arm must not fire on them.
func TestToolResultCaptureObjectIDDetectorSeesAnUnmintedConstruction(t *testing.T) {
	t.Parallel()
	const src = `package p

import sessionwire "github.com/looprig/core/sessionwire/v1"

type objRefAlias = sessionwire.ObjectReference

func bypass() *sessionwire.ObjectReference {
	return &sessionwire.ObjectReference{ObjectID: "https://signed.example/o?sig=1"}
}

func minted(digest string) sessionwire.ObjectReference {
	return sessionwire.ObjectReference{ObjectID: captureObjectID(digest)}
}

func mutate(r *sessionwire.ObjectReference, raw string) {
	r.ObjectID = raw
}

func unkeyed() sessionwire.ObjectReference {
	return sessionwire.ObjectReference{"https://signed.example/o?sig=1"}
}

func unkeyedMinted(digest string) sessionwire.ObjectReference {
	return sessionwire.ObjectReference{captureObjectID(digest)}
}

func renamed(raw string) objRefAlias {
	return objRefAlias{ObjectID: raw}
}

func slice(digest string) []sessionwire.ObjectReference {
	return []sessionwire.ObjectReference{{ObjectID: captureObjectID(digest)}}
}
`
	// One entry per assignment above, in source order.
	wantMinted := []bool{false, true, false, false, true, false, true}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	names := objectReferenceTypeNames([]*ast.File{file})
	found := findObjectIDAssignments(fset, file, names)
	if len(found) != len(wantMinted) {
		t.Fatalf("assignments found = %d, want %d: %v", len(found), len(wantMinted), found)
	}
	for i, assignment := range found {
		if assignment.minted != wantMinted[i] {
			t.Errorf("assignment %d at %v minted = %v, want %v", i, assignment.pos, assignment.minted, wantMinted[i])
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
	var files []*ast.File
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
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		files = append(files, file)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) < 100 {
		t.Fatalf("scanned %d production files, want the whole module; the walk is not reaching it", len(files))
	}
	// The alias set is computed over the WHOLE file set before any file is
	// inspected, so a renaming alias declared in one package is recognized in
	// every other one.
	names := objectReferenceTypeNames(files)
	total := 0
	for _, file := range files {
		for _, assignment := range findObjectIDAssignments(fset, file, names) {
			total++
			if assignment.minted {
				continue
			}
			if assignment.positional {
				t.Errorf("%v: an ObjectReference is built with unkeyed fields, so object_id is supplied "+
					"positionally without passing through captureObjectID; the wire contract cannot check "+
					"object_id opacity, so this producer is its only guarantor", assignment.pos)
				continue
			}
			t.Errorf("%v: ObjectID is assigned a value that did not come from captureObjectID; "+
				"the wire contract cannot check object_id opacity, so this producer is its only guarantor", assignment.pos)
		}
	}
	if total == 0 {
		t.Fatal("no ObjectID assignment found anywhere in production code; the guard is vacuous")
	}
}

// namesObjectReference reports whether a composite literal's type expression
// names one of names, directly or as the element or value type of a slice or map.
// A nil type is an elided element literal, which only occurs inside a literal of
// the surrounding type, so it is left to the field name to decide.
func namesObjectReference(expr ast.Expr, names map[string]bool) bool {
	switch node := expr.(type) {
	case nil:
		return true
	case *ast.ArrayType:
		return namesObjectReference(node.Elt, names)
	case *ast.MapType:
		return namesObjectReference(node.Value, names)
	default:
		return namesObjectReferenceStruct(expr, names)
	}
}

// namesObjectReferenceStruct reports whether expr writes the STRUCT type itself —
// a bare or qualified name in names, optionally pointed to. It excludes the nil,
// slice and map forms namesObjectReference also accepts, because the elements of
// those literals are whole values rather than fields, so an unkeyed element of
// one says nothing about ObjectID.
func namesObjectReferenceStruct(expr ast.Expr, names map[string]bool) bool {
	switch node := expr.(type) {
	case *ast.Ident:
		return names[node.Name]
	case *ast.SelectorExpr:
		return names[node.Sel.Name]
	case *ast.StarExpr:
		return namesObjectReferenceStruct(node.X, names)
	default:
		return false
	}
}

// objectReferenceTypeNames returns the bare type names that stand for
// sessionwire.ObjectReference across files: ObjectReference itself, plus every
// name declared in files as an alias or a defined type over one of them, iterated
// to a fixpoint so an alias of an alias is included too.
//
// This is what makes matching on the final identifier sound rather than merely
// usually right. Matching a bare name catches a dot import and a SAME-named
// alias, but a RENAMING alias — type ref = sessionwire.ObjectReference — reaches
// the same field under a name the matcher has never heard of; collecting the
// declarations gives it that name. Keying by bare name module-wide is
// deliberately wide in the other direction: an unrelated package's own type named
// ObjectReference would be scanned too, which is a loud failure to triage.
//
// It is syntactic, not a type resolution, so it covers exactly the alias and
// defined-type declarations present in the scanned file set. A type reaching
// ObjectReference through a declaration OUTSIDE that set — an embedded field, a
// generic instantiation, or an alias published by a dependency — is not collected.
func objectReferenceTypeNames(files []*ast.File) map[string]bool {
	names := map[string]bool{"ObjectReference": true}
	for changed := true; changed; {
		changed = false
		for _, file := range files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.TYPE {
					continue
				}
				for _, spec := range gen.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || names[ts.Name.Name] {
						continue
					}
					if namesObjectReferenceStruct(ts.Type, names) {
						names[ts.Name.Name] = true
						changed = true
					}
				}
			}
		}
	}
	return names
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
