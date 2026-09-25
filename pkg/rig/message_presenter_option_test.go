package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/present"
)

type stubMessagePresenter struct{}

func (*stubMessagePresenter) Present(context.Context, present.Input) (present.Frame, error) {
	return present.Frame{}, nil
}

func TestWithMessagePresenterRefusesNilAndDuplicate(t *testing.T) {
	t.Parallel()
	var typedNil *stubMessagePresenter
	tests := []struct {
		name     string
		opts     []Option
		wantKind DefinitionErrorKind
	}{
		{name: "nil presenter", opts: []Option{WithMessagePresenter(nil)}, wantKind: DefinitionInvalidMessagePresenter},
		{name: "typed nil presenter", opts: []Option{WithMessagePresenter(typedNil)}, wantKind: DefinitionInvalidMessagePresenter},
		{name: "duplicate", opts: []Option{WithMessagePresenter(&stubMessagePresenter{}), WithMessagePresenter(&stubMessagePresenter{})}, wantKind: DefinitionDuplicateOption},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Define(validRigOptions(t, tc.opts...)...)
			var definitionErr *DefinitionError
			if !errors.As(err, &definitionErr) || definitionErr.Kind != tc.wantKind {
				t.Fatalf("Define() = %v, want kind %q", err, tc.wantKind)
			}
		})
	}
}

func TestWithMessagePresenterDefines(t *testing.T) {
	t.Parallel()
	if _, err := Define(validRigOptions(t, WithMessagePresenter(&stubMessagePresenter{}))...); err != nil {
		t.Fatalf("Define: %v", err)
	}
}
