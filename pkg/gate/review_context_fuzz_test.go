package gate_test

import (
	"errors"
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/gate"
)

func FuzzReviewContextBuildBoundsAndDeterminism(f *testing.F) {
	f.Add(
		[]byte("inspect the repository"),
		[]byte(`{"command":"rg TODO"}`),
		[]byte("tool evidence"),
		uint16(128),
		uint16(128),
		uint16(512),
		uint16(8),
		uint16(128),
	)
	f.Add(
		[]byte("αβγδ current intent"),
		[]byte("safe-action"),
		[]byte("prefix-ζηθ-suffix"),
		uint16(40),
		uint16(40),
		uint16(96),
		uint16(3),
		uint16(24),
	)
	f.Add(
		[]byte{0xff, 0xfe},
		[]byte("action"),
		[]byte("evidence"),
		uint16(32),
		uint16(32),
		uint16(128),
		uint16(4),
		uint16(32),
	)

	f.Fuzz(func(
		t *testing.T,
		userBytes []byte,
		actionBytes []byte,
		toolBytes []byte,
		maxUserRaw uint16,
		maxToolRaw uint16,
		maxBytesRaw uint16,
		maxEntriesRaw uint16,
		maxTokensRaw uint16,
	) {
		input := validReviewContext()
		input.Entries[0].Content = string(userBytes)
		input.Entries[1].Content = string(actionBytes)
		input.Entries = append([]gate.ReviewContextEntry{{
			Origin:  gate.ReviewContextOriginTool,
			Kind:    gate.ReviewContextKindToolResult,
			Content: string(toolBytes),
		}}, input.Entries...)
		original := input.Clone()

		policy := validReviewContextPolicy()
		policy.MaxUserEntryBytes = int(maxUserRaw%256) + 1
		policy.MaxToolEntryBytes = int(maxToolRaw%256) + 1
		policy.MaxBytes = int(maxBytesRaw%1024) + 1
		policy.MaxEntries = int(maxEntriesRaw%8) + 1
		policy.MaxEstimatedTokens = int(maxTokensRaw%256) + 1
		policy.MaxAgentEntryBytes = 256
		policy.MaxBlockBytes = 256
		policy.MaxActiveActionBytes = 256

		first, err := gate.BuildReviewContext(input, policy)
		if !reflect.DeepEqual(input, original) {
			t.Fatal("BuildReviewContext() mutated fuzz input")
		}
		if err != nil {
			if !reflect.DeepEqual(first, gate.ReviewContext{}) {
				t.Fatalf("BuildReviewContext() output on error = %#v, want zero value", first)
			}
			var validationErr *gate.ReviewValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("BuildReviewContext() error = %T, want *ReviewValidationError", err)
			}
			if len(err.Error()) > 128 {
				t.Fatalf("BuildReviewContext() error length = %d, want bounded", len(err.Error()))
			}
			return
		}

		second, secondErr := gate.BuildReviewContext(input, policy)
		if secondErr != nil || !reflect.DeepEqual(first, second) {
			t.Fatalf("BuildReviewContext() is nondeterministic: second error = %v", secondErr)
		}
		if len(first.Entries) > policy.MaxEntries {
			t.Fatalf("entries = %d, want <= %d", len(first.Entries), policy.MaxEntries)
		}
		if contentBytes(first.Entries) > policy.MaxBytes {
			t.Fatalf("content bytes = %d, want <= %d", contentBytes(first.Entries), policy.MaxBytes)
		}
		if estimatedReviewTokens(first.Entries) > policy.MaxEstimatedTokens {
			t.Fatalf("estimated tokens = %d, want <= %d", estimatedReviewTokens(first.Entries), policy.MaxEstimatedTokens)
		}
		activeFound := false
		userFound := false
		for _, entry := range first.Entries {
			if !utf8.ValidString(string(entry.Origin)) ||
				!utf8.ValidString(string(entry.Kind)) ||
				!utf8.ValidString(entry.Content) {
				t.Fatalf("output entry is invalid UTF-8: %#v", entry)
			}
			if entry.Kind == gate.ReviewContextKindAssistantToolRequest {
				activeFound = true
				if entry.Truncated || entry.Content != string(actionBytes) {
					t.Fatalf("active action changed: %#v", entry)
				}
			}
			if entry.Kind == gate.ReviewContextKindUserMessage {
				userFound = true
			}
		}
		if !activeFound || !userFound {
			t.Fatalf("required entries missing: active=%t user=%t", activeFound, userFound)
		}
	})
}
