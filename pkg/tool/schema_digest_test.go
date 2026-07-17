package tool

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestSchemaDigest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		schema  string
		wantErr bool
	}{
		{name: "object schema", schema: `{"type":"object","properties":{"q":{"type":"string"}}}`},
		{name: "empty schema", schema: ``},
		{name: "null schema", schema: `null`},
		{name: "whitespace only", schema: "  \n\t "},
		{name: "invalid json rejected", schema: `{"type":`, wantErr: true},
		{name: "trailing garbage rejected", schema: `{} nope`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SchemaDigest(json.RawMessage(tt.schema))
			if (err != nil) != tt.wantErr {
				t.Fatalf("SchemaDigest() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				var invalid *InvalidSchemaError
				if !errors.As(err, &invalid) {
					t.Fatalf("error = %v, want *InvalidSchemaError", err)
				}
				return
			}
			if len(got) != 64 {
				t.Fatalf("digest = %q, want 64 hex chars", got)
			}
		})
	}
}

// TestSchemaDigestIsWhitespaceCanonical is the load-bearing property: identity must not
// change because a server reformatted its schema.
//
// Mutation check: digesting the raw bytes instead of the json.Compact output makes these
// two digests differ and fails here.
func TestSchemaDigestIsWhitespaceCanonical(t *testing.T) {
	t.Parallel()
	compact, err := SchemaDigest(json.RawMessage(`{"type":"object","a":[1,2]}`))
	if err != nil {
		t.Fatalf("SchemaDigest: %v", err)
	}
	spaced, err := SchemaDigest(json.RawMessage("{\n  \"type\": \"object\",\n  \"a\": [1, 2]\n}"))
	if err != nil {
		t.Fatalf("SchemaDigest: %v", err)
	}
	if compact != spaced {
		t.Fatalf("whitespace changed the digest: %q vs %q", compact, spaced)
	}
}

// TestSchemaDigestDistinguishesSchemas guards the other direction: a digest that ignored
// its input (or returned a constant) would silently defeat drift detection.
func TestSchemaDigestDistinguishesSchemas(t *testing.T) {
	t.Parallel()
	a, err := SchemaDigest(json.RawMessage(`{"a":1}`))
	if err != nil {
		t.Fatalf("SchemaDigest: %v", err)
	}
	b, err := SchemaDigest(json.RawMessage(`{"a":2}`))
	if err != nil {
		t.Fatalf("SchemaDigest: %v", err)
	}
	if a == b {
		t.Fatal("different schemas produced the same digest")
	}
	empty, err := SchemaDigest(nil)
	if err != nil {
		t.Fatalf("SchemaDigest(nil): %v", err)
	}
	if empty == a {
		t.Fatal("empty schema digests the same as a real schema")
	}
}
