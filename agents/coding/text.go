package coding

import (
	"strings"

	"github.com/inventivepotter/urvi/internal/content"
)

// aiMessageText concatenates the text of every *content.TextBlock in m, ignoring
// non-text blocks. A nil message yields the empty string. It is the package's
// final-text projection, reused by the eval harness (eval_integration_test.go) to
// flatten a turn's terminal AIMessage into a plain string.
func aiMessageText(m *content.AIMessage) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range m.Blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}
