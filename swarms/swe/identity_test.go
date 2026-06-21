package swe

import (
	"encoding/xml"
	"strings"
	"testing"
)

// TestIdentityNonEmpty proves the shared SWE identity prompt is present: the
// swarm prepends it to every agent's role, so an empty constant would silently
// strip the cross-cutting persona/security/persistence guidance.
func TestIdentityNonEmpty(t *testing.T) {
	t.Parallel()
	if strings.TrimSpace(Identity) == "" {
		t.Fatal("Identity is empty")
	}
}

// TestIdentityContainsCrossCuttingBits proves the identity carries the four
// cross-cutting concerns the design (§ shared identity) mandates: the product
// name, the concise/direct persona, persistence (never fabricate), the secrets/
// PII rule, and the reversibility stance. Substring checks are coarse but pin
// the intent so a future edit cannot quietly drop a whole concern.
func TestIdentityContainsCrossCuttingBits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		{name: "names the SWE product", want: "SWE"},
		{name: "concise persona", want: "concise"},
		{name: "persistence: keep going", want: "resolved"},
		{name: "never fabricate", want: "fabricate"},
		{name: "secrets/PII boundary", want: "secret"},
		{name: "PII boundary", want: "PII"},
		{name: "reversibility stance", want: "reversible"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(Identity, tt.want) {
				t.Errorf("Identity is missing %q", tt.want)
			}
		})
	}
}

// TestIdentityIsWellFormedXML proves the identity is a single well-formed XML
// element carrying the product attribute. The swarm assembles identity+role
// into a system prompt; malformed XML would corrupt that assembly.
func TestIdentityIsWellFormedXML(t *testing.T) {
	t.Parallel()
	var probe struct {
		XMLName xml.Name `xml:"identity"`
		Product string   `xml:"product,attr"`
	}
	if err := xml.Unmarshal([]byte(Identity), &probe); err != nil {
		t.Fatalf("Identity is not well-formed XML: %v", err)
	}
	if probe.Product != "SWE" {
		t.Errorf("Identity product attr = %q, want %q", probe.Product, "SWE")
	}
}
