package tool

import (
	"reflect"
	"strings"
	"unicode/utf8"
)

// MaxModelFacingErrorBytes bounds an explicitly model-facing error detail at
// every boundary where it can be persisted or rendered. It is deliberately
// shared by the event codec, session runtime, and agent-tool presentation layer.
const MaxModelFacingErrorBytes = 256 << 10

// ModelFacingError is the narrow opt-in marker for an error detail that is safe
// to expose to the model. The marker is intentionally not inferred from Error,
// a stable error kind, or any other ordinary error text.
type ModelFacingError interface {
	ModelFacingError() string
}

// BoundModelFacingErrorDetail normalizes invalid UTF-8 and returns a complete
// rune prefix no larger than MaxModelFacingErrorBytes.
func BoundModelFacingErrorDetail(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= MaxModelFacingErrorBytes {
		return value
	}
	end := MaxModelFacingErrorBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

const (
	maxModelFacingErrorDepth = 64
	maxModelFacingErrorNodes = 128
)

type modelFacingErrorNode struct {
	err   error
	depth int
}

// ModelFacingErrorDetail finds an explicitly marked error through ordinary
// Unwrap chains and errors.Join trees. It deliberately does not call errors.As:
// an error's custom As method is executable code and can fabricate a marker.
// Traversal is bounded and cycle-aware because errors are external boundary
// values in the session/runtime path.
func ModelFacingErrorDetail(err error) (detail string, marked bool) {
	if err == nil {
		return "", false
	}
	queue := []modelFacingErrorNode{{err: err}}
	seen := make(map[error]struct{})
	for nodes := 0; len(queue) > 0 && nodes < maxModelFacingErrorNodes; {
		current := queue[0]
		queue = queue[1:]
		if current.err == nil || current.depth > maxModelFacingErrorDepth {
			continue
		}
		nodes++
		if comparableError(current.err) {
			if _, ok := seen[current.err]; ok {
				continue
			}
			seen[current.err] = struct{}{}
		}
		if marker, ok := current.err.(ModelFacingError); ok && !nilModelFacingError(marker) {
			if detail, ok := callModelFacingError(marker); ok {
				return BoundModelFacingErrorDetail(detail), true
			}
		}
		if current.depth == maxModelFacingErrorDepth {
			continue
		}
		for _, child := range unwrapErrors(current.err) {
			if child != nil {
				queue = append(queue, modelFacingErrorNode{err: child, depth: current.depth + 1})
			}
		}
	}
	return "", false
}

func comparableError(err error) bool {
	value := reflect.ValueOf(err)
	return value.IsValid() && value.Type().Comparable()
}

func nilModelFacingError(marker ModelFacingError) bool {
	value := reflect.ValueOf(marker)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func callModelFacingError(marker ModelFacingError) (detail string, ok bool) {
	defer func() {
		if recover() != nil {
			detail, ok = "", false
		}
	}()
	return marker.ModelFacingError(), true
}

func unwrapErrors(err error) (children []error) {
	defer func() {
		if recover() != nil {
			children = nil
		}
	}()
	if one, ok := err.(interface{ Unwrap() error }); ok {
		children = append(children, one.Unwrap())
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		children = append(children, many.Unwrap()...)
	}
	return children
}
