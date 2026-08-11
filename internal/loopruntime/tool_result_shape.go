package loopruntime

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const toolResultTruncatedMarker = "\n[tool output truncated: omitted %d of %d bytes]\n"

func shapeToolResultText(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	normalized := strings.ToValidUTF8(text, "\uFFFD")
	if len(normalized) <= limit {
		return normalized
	}

	originalBytes := len(text)
	marker := fmt.Sprintf(toolResultTruncatedMarker, len(normalized), originalBytes)
	for i := 0; i < 8; i++ {
		remaining := limit - len(marker)
		if remaining <= 0 {
			return boundedUTF8(normalized, limit)
		}
		headBudget := remaining / 2
		tailBudget := remaining - headBudget
		head := normalized[:utf8Boundary(normalized, headBudget)]
		tailStart := utf8TailBoundary(normalized, tailBudget)
		tail := normalized[tailStart:]
		omitted := len(normalized) - len(head) - len(tail)
		updated := fmt.Sprintf(toolResultTruncatedMarker, omitted, originalBytes)
		if updated == marker {
			return head + marker + tail
		}
		marker = updated
	}

	remaining := limit - len(marker)
	if remaining <= 0 {
		return boundedUTF8(normalized, limit)
	}
	headBudget := remaining / 2
	tailBudget := remaining - headBudget
	head := normalized[:utf8Boundary(normalized, headBudget)]
	tailStart := utf8TailBoundary(normalized, tailBudget)
	return head + marker + normalized[tailStart:]
}

func utf8Boundary(value string, budget int) int {
	if budget <= 0 {
		return 0
	}
	if budget >= len(value) {
		return len(value)
	}
	for budget > 0 && !utf8.RuneStart(value[budget]) {
		budget--
	}
	return budget
}

func utf8TailBoundary(value string, budget int) int {
	if budget <= 0 {
		return len(value)
	}
	if budget >= len(value) {
		return 0
	}
	start := len(value) - budget
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return start
}

func boundedUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if limit >= len(value) {
		return value
	}
	return value[:utf8Boundary(value, limit)]
}
