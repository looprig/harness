package serve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The wire-contract artifact directories under testdata/. These are hand-authored
// (no OpenAPI/JSON-Schema dependency — CLAUDE.md is stdlib-only); this test is a
// lightweight guard that a malformed hand-edit is caught, NOT a full validator.
const (
	schemaDir   = "testdata/schema"
	openapiFile = "testdata/openapi.yaml"
)

// contractSchema is the shallow view of a JSON-Schema document this test reads: the
// top-level required set and the property names. It is deliberately partial — full
// schema semantics ($ref resolution, type checking) are NOT interpreted here.
type contractSchema struct {
	Required   []string                   `json:"required"`
	Properties map[string]json.RawMessage `json:"properties"`
}

// TestOpenAPIExists proves the hand-written OpenAPI document is present and
// non-empty, so a deletion or truncation is caught. It is YAML (the stdlib cannot
// parse YAML and we do not depend on a YAML library), so it is a doc artifact only —
// existence is the assertion.
func TestOpenAPIExists(t *testing.T) {
	t.Parallel()
	info, err := os.Stat(openapiFile)
	if err != nil {
		t.Fatalf("stat %s: %v", openapiFile, err)
	}
	if info.Size() == 0 {
		t.Fatalf("%s is empty", openapiFile)
	}
}

func TestCompactionStartedDocumented(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "JSON Schema kind", path: filepath.Join(schemaDir, "ephemeral_frame.schema.json"), want: `"compaction_started"`},
		{name: "OpenAPI kind", path: openapiFile, want: "compaction_started"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(tt.path)
			if err != nil {
				t.Fatalf("read %s: %v", tt.path, err)
			}
			if !strings.Contains(string(raw), tt.want) {
				t.Errorf("%s does not document %q", tt.path, tt.want)
			}
		})
	}
}

// TestSchemaFilesParse proves every schema/*.json document parses as valid JSON, so
// a malformed hand-edit (a trailing comma, an unbalanced brace) fails fast rather
// than silently rotting the contract.
func TestSchemaFilesParse(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join(schemaDir, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", schemaDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no schema files found under %s", schemaDir)
	}

	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			t.Parallel()
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatalf("%s is not valid JSON: %v", f, err)
			}
			if _, ok := doc["$schema"]; !ok {
				t.Errorf("%s missing $schema (draft declaration)", f)
			}
		})
	}
}

// TestFixtureFilesParse proves every fixtures/*.json document parses as valid JSON
// and every fixtures/*.sse frame has the SSE shape (an `event:` line and a `data:`
// line whose JSON payload parses).
func TestFixtureFilesParse(t *testing.T) {
	t.Parallel()
	if *update {
		t.Skip("-update rewrites fixtures concurrently; validate them on a normal run")
	}

	files, err := filepath.Glob(filepath.Join(fixturesDir, "*"))
	if err != nil {
		t.Fatalf("glob %s: %v", fixturesDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no fixtures found under %s", fixturesDir)
	}

	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			t.Parallel()
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			switch filepath.Ext(f) {
			case ".json":
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(b, &doc); err != nil {
					t.Fatalf("%s is not valid JSON: %v", f, err)
				}
			case ".sse":
				if !strings.HasPrefix(string(b), "event: ") {
					t.Errorf("%s does not start with an SSE event: line", f)
				}
				payload := sseData(t, b)
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(payload, &doc); err != nil {
					t.Fatalf("%s data: payload is not valid JSON: %v (payload %s)", f, err, payload)
				}
			default:
				t.Fatalf("unexpected fixture extension: %s", f)
			}
		})
	}
}

// TestFixturesMatchSchemaShape is the shallow structural cross-check: each fixture's
// top-level keys must all be declared as schema properties, and every schema-required
// key must be present in the fixture. It is NOT full JSON-Schema validation (no type
// or $ref checking) — just a stdlib-only guard that a fixture and its schema have not
// drifted apart in field names.
func TestFixturesMatchSchemaShape(t *testing.T) {
	t.Parallel()
	if *update {
		t.Skip("-update rewrites fixtures concurrently; validate them on a normal run")
	}

	tests := []struct {
		fixture string
		schema  string
		sse     bool // read the data: payload rather than the whole file
	}{
		{fixture: "capabilities.json", schema: "capabilities.schema.json"},
		{fixture: "create_idle.json", schema: "create_response.schema.json"},
		{fixture: "create_with_command.json", schema: "create_response.schema.json"},
		{fixture: "session_list.json", schema: "session_list.schema.json"},
		{fixture: "restore.json", schema: "restore_response.schema.json"},
		{fixture: "input.json", schema: "input_response.schema.json"},
		{fixture: "interrupt.json", schema: "interrupt_response.schema.json"},
		{fixture: "gate_accepted.json", schema: "gate_accepted_response.schema.json"},
		{fixture: "status_running.json", schema: "session_status.schema.json"},
		{fixture: "journal_page.json", schema: "event_journal_page.schema.json"},
		{fixture: "error_400.json", schema: "error_response.schema.json"},
		{fixture: "error_404.json", schema: "error_response.schema.json"},
		{fixture: "error_409.json", schema: "error_response.schema.json"},
		{fixture: "error_500.json", schema: "error_response.schema.json"},
		{fixture: "error_503.json", schema: "error_response.schema.json"},
		{fixture: "enduring_frame.sse", schema: "enduring_frame.schema.json", sse: true},
		{fixture: "ephemeral_token_delta.sse", schema: "ephemeral_frame.schema.json", sse: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()

			raw, err := os.ReadFile(filepath.Join(fixturesDir, tt.fixture))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			if tt.sse {
				raw = sseData(t, raw)
			}
			var fixture map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatalf("decode fixture %s: %v", tt.fixture, err)
			}

			schemaBytes, err := os.ReadFile(filepath.Join(schemaDir, tt.schema))
			if err != nil {
				t.Fatalf("read schema: %v", err)
			}
			var schema contractSchema
			if err := json.Unmarshal(schemaBytes, &schema); err != nil {
				t.Fatalf("decode schema %s: %v", tt.schema, err)
			}

			// Every fixture key must be a declared property.
			for key := range fixture {
				if _, ok := schema.Properties[key]; !ok {
					t.Errorf("fixture key %q is not a declared property in %s", key, tt.schema)
				}
			}
			// Every required property must be present in the fixture.
			for _, req := range schema.Required {
				if _, ok := fixture[req]; !ok {
					t.Errorf("schema %s requires %q but fixture %s omits it", tt.schema, req, tt.fixture)
				}
			}
		})
	}
}

// sseData extracts the JSON payload following the `data: ` prefix of an SSE frame,
// failing the test if no data: line is present.
func sseData(t *testing.T, frame []byte) []byte {
	t.Helper()
	for _, line := range strings.Split(string(frame), "\n") {
		if rest, ok := strings.CutPrefix(line, "data: "); ok {
			return []byte(rest)
		}
	}
	t.Fatalf("no data: line in SSE frame: %s", frame)
	return nil
}
