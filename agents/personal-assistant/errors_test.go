package personalassistant

import (
	"errors"
	"strings"
	"testing"
)

func TestMissingEnvError(t *testing.T) {
	t.Parallel()
	err := error(&MissingEnvError{Var: "LLM_API_KEY"})
	if !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Errorf("Error() = %q, want it to contain LLM_API_KEY", err.Error())
	}
	var me *MissingEnvError
	if !errors.As(err, &me) {
		t.Fatalf("errors.As(*MissingEnvError) failed")
	}
	if me.Var != "LLM_API_KEY" {
		t.Errorf("Var = %q, want LLM_API_KEY", me.Var)
	}
}

func TestEmptyInputError(t *testing.T) {
	t.Parallel()
	err := error(&EmptyInputError{})
	if err.Error() == "" {
		t.Errorf("Error() is empty")
	}
	var ee *EmptyInputError
	if !errors.As(err, &ee) {
		t.Fatalf("errors.As(*EmptyInputError) failed")
	}
}
