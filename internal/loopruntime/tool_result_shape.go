package loopruntime

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const toolResultTruncatedMarker = "\n[tool output truncated: omitted %d of %d bytes]\n"

type normalizedToolResult struct {
	text       string
	runeStarts []int
	sourceSpan []int
}

func normalizeToolResult(text string) normalizedToolResult {
	normalized := strings.ToValidUTF8(text, "\uFFFD")
	result := normalizedToolResult{text: normalized}
	for offset := 0; offset < len(text); {
		runeValue, size := utf8.DecodeRuneInString(text[offset:])
		if runeValue == utf8.RuneError && size == 1 {
			start := offset
			offset += size
			for offset < len(text) {
				next, nextSize := utf8.DecodeRuneInString(text[offset:])
				if next != utf8.RuneError || nextSize != 1 {
					break
				}
				offset += nextSize
			}
			result.sourceSpan = append(result.sourceSpan, offset-start)
			continue
		}
		result.sourceSpan = append(result.sourceSpan, size)
		offset += size
	}
	for offset := 0; offset < len(normalized); {
		result.runeStarts = append(result.runeStarts, offset)
		_, size := utf8.DecodeRuneInString(normalized[offset:])
		offset += size
	}
	return result
}

func (value normalizedToolResult) sourceBytes(start, end int) int {
	result := 0
	for index, runeStart := range value.runeStarts {
		if runeStart >= end {
			break
		}
		if runeStart >= start {
			result += value.sourceSpan[index]
		}
	}
	return result
}

func shapeToolResultText(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	normalized := normalizeToolResult(text)
	if len(normalized.text) <= limit {
		return normalized.text
	}

	originalBytes := len(text)
	marker := fmt.Sprintf(toolResultTruncatedMarker, originalBytes, originalBytes)
	for i := 0; i < 8; i++ {
		remaining := limit - len(marker)
		if remaining <= 0 {
			return boundedUTF8(normalized.text, limit)
		}
		headBudget := remaining / 2
		tailBudget := remaining - headBudget
		headEnd := utf8Boundary(normalized.text, headBudget)
		tailStart := utf8TailBoundary(normalized.text, tailBudget)
		head := normalized.text[:headEnd]
		tail := normalized.text[tailStart:]
		omitted := originalBytes - normalized.sourceBytes(0, headEnd) - normalized.sourceBytes(tailStart, len(normalized.text))
		updated := fmt.Sprintf(toolResultTruncatedMarker, omitted, originalBytes)
		if updated == marker {
			return head + marker + tail
		}
		marker = updated
	}

	remaining := limit - len(marker)
	if remaining <= 0 {
		return boundedUTF8(normalized.text, limit)
	}
	headBudget := remaining / 2
	tailBudget := remaining - headBudget
	headEnd := utf8Boundary(normalized.text, headBudget)
	tailStart := utf8TailBoundary(normalized.text, tailBudget)
	head := normalized.text[:headEnd]
	return head + marker + normalized.text[tailStart:]
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
