package loopruntime

import (
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
)

func TestParseCompactionSummaryXML(t *testing.T) {
	t.Parallel()
	valid := `<conversation_summary><goal>ship &lt;safe&gt;</goal><constraints></constraints><decisions/><state>tests &amp; build pass</state><open_items></open_items></conversation_summary>`
	tests := []struct {
		name       string
		raw        string
		wantReason loop.InvalidSummaryReason
	}{
		{name: "valid escaped data and empty optional sections", raw: valid},
		{name: "empty document", raw: ``, wantReason: loop.InvalidSummaryXMLRoot},
		{name: "wrong root", raw: `<summary><goal>x</goal><constraints/><decisions/><state>y</state><open_items/></summary>`, wantReason: loop.InvalidSummaryXMLRoot},
		{name: "root attribute", raw: `<conversation_summary id="x"><goal>x</goal><constraints/><decisions/><state>y</state><open_items/></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "namespace attribute", raw: `<conversation_summary xmlns="urn:x"><goal>x</goal><constraints/><decisions/><state>y</state><open_items/></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "leading whitespace", raw: " \n" + valid, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "trailing whitespace", raw: valid + "\n", wantReason: loop.InvalidSummaryXMLStructure},
		{name: "wrapper prose", raw: "summary: " + valid, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "trailing element", raw: valid + `<extra/>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "comment before root", raw: `<!--x-->` + valid, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "comment in root", raw: `<conversation_summary><!--x--><goal>x</goal><constraints/><decisions/><state>y</state><open_items/></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "directive", raw: `<!DOCTYPE conversation_summary>` + valid, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "processing instruction", raw: `<?xml version="1.0"?>` + valid, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "cdata", raw: `<conversation_summary><goal><![CDATA[ship safely]]></goal><constraints/><decisions/><state>ready</state><open_items/></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "missing child", raw: `<conversation_summary><goal>x</goal><constraints/><decisions/><state>y</state></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "duplicate child", raw: `<conversation_summary><goal>x</goal><constraints/><decisions/><state>y</state><state>again</state><open_items/></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "unknown child", raw: `<conversation_summary><goal>x</goal><constraints/><unknown/><state>y</state><open_items/></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "out of order", raw: `<conversation_summary><constraints/><goal>x</goal><decisions/><state>y</state><open_items/></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "child attribute", raw: `<conversation_summary><goal id="x">x</goal><constraints/><decisions/><state>y</state><open_items/></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "nested child", raw: `<conversation_summary><goal><b>x</b></goal><constraints/><decisions/><state>y</state><open_items/></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "root character prose", raw: `<conversation_summary>prose<goal>x</goal><constraints/><decisions/><state>y</state><open_items/></conversation_summary>`, wantReason: loop.InvalidSummaryXMLStructure},
		{name: "empty goal", raw: "<conversation_summary><goal> \n </goal><constraints/><decisions/><state>y</state><open_items/></conversation_summary>", wantReason: loop.InvalidSummaryXMLContent},
		{name: "empty state", raw: "<conversation_summary><goal>x</goal><constraints/><decisions/><state>\t</state><open_items/></conversation_summary>", wantReason: loop.InvalidSummaryXMLContent},
		{name: "malformed closing tag", raw: `<conversation_summary><goal>x</goal><constraints/><decisions/><state>y</state><open_items></conversation_summary>`, wantReason: loop.InvalidSummaryXMLSyntax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, err := ParseCompactionSummaryXML([]byte(tt.raw))
			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("ParseCompactionSummaryXML() error = %v", err)
				}
				if summary == nil || summary.Role != content.RoleUser || len(summary.Blocks) != 1 {
					t.Fatalf("summary = %#v, want one user text block", summary)
				}
				text, ok := summary.Blocks[0].(*content.TextBlock)
				if !ok || text.Text != tt.raw {
					t.Fatalf("summary text = %#v, want validated XML preserved exactly", summary.Blocks[0])
				}
				return
			}
			var invalid *loop.InvalidSummaryError
			if !errors.As(err, &invalid) || invalid.Reason != tt.wantReason {
				t.Fatalf("ParseCompactionSummaryXML() error = %T %v, want reason %q", err, err, tt.wantReason)
			}
			if summary != nil {
				t.Fatalf("summary = %#v on invalid XML", summary)
			}
		})
	}
}

func TestCompactionOutcomeValidate(t *testing.T) {
	t.Parallel()
	value := loop.CompactionOutput{}
	cause := errors.New("compaction failed")
	tests := []struct {
		name    string
		outcome CompactionOutcome
		wantErr bool
	}{
		{name: "success", outcome: CompactionOutcome{Value: &value}},
		{name: "failure", outcome: CompactionOutcome{Err: cause}},
		{name: "neither", outcome: CompactionOutcome{}, wantErr: true},
		{name: "both", outcome: CompactionOutcome{Value: &value, Err: cause}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.outcome.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
