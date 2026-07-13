package rig

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestRigOwnsFingerprintPublicSurface(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "fingerprint.go", nil, 0)
	if err != nil {
		t.Fatalf("parse fingerprint.go: %v", err)
	}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquote import: %v", err)
		}
		if path == "github.com/looprig/harness/internal/sessionruntime" {
			t.Fatal("pkg/rig fingerprint implementation delegates to internal/sessionruntime")
		}
	}
	var fields *ast.TypeSpec
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			typ, ok := spec.(*ast.TypeSpec)
			if ok && typ.Name.Name == "ConfigFingerprintFields" {
				fields = typ
			}
		}
	}
	if fields == nil {
		t.Fatal("ConfigFingerprintFields declaration missing from fingerprint.go")
	}
	if fields.Assign.IsValid() {
		t.Fatal("ConfigFingerprintFields must be a rig-owned type, not an alias")
	}
	if _, ok := fields.Type.(*ast.StructType); !ok {
		t.Fatalf("ConfigFingerprintFields underlying syntax = %T, want struct", fields.Type)
	}
}

func TestExportedTypesDoNotAliasInternalRuntime(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read rig package: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		internalAliases := make(map[string]bool)
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote %s import: %v", entry.Name(), err)
			}
			if path != "github.com/looprig/harness/internal/sessionruntime" {
				continue
			}
			name := "sessionruntime"
			if spec.Name != nil {
				name = spec.Name.Name
			}
			internalAliases[name] = true
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				typ, ok := spec.(*ast.TypeSpec)
				if !ok || !ast.IsExported(typ.Name.Name) || !typ.Assign.IsValid() {
					continue
				}
				selector, ok := typ.Type.(*ast.SelectorExpr)
				ident, isIdent := selectorX(selector, ok)
				if isIdent && internalAliases[ident.Name] {
					t.Errorf("%s: exported type %s aliases internal/sessionruntime", entry.Name(), typ.Name.Name)
				}
			}
		}
	}
}

func selectorX(selector *ast.SelectorExpr, ok bool) (*ast.Ident, bool) {
	if !ok {
		return nil, false
	}
	ident, isIdent := selector.X.(*ast.Ident)
	return ident, isIdent
}
