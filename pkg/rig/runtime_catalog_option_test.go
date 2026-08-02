package rig

import (
	"errors"
	"testing"

	"github.com/looprig/harness/pkg/loop"
)

func TestWithRuntimeCatalogCompilesLifecycleOptionAndRejectsDuplicate(t *testing.T) {
	t.Parallel()

	catalog, err := loop.NewRuntimeCatalog(nil)
	if err != nil {
		t.Fatalf("NewRuntimeCatalog: %v", err)
	}
	state := &definitionState{seen: make(map[singletonKey]bool)}
	option := WithRuntimeCatalog(catalog)

	if err := option(state); err != nil {
		t.Fatalf("first WithRuntimeCatalog: %v", err)
	}
	if got := len(state.lifecycleOptions); got != 1 {
		t.Fatalf("lifecycle options = %d, want 1", got)
	}

	err = option(state)
	var definitionErr *DefinitionError
	if !errors.As(err, &definitionErr) || definitionErr.Kind != DefinitionDuplicateOption || definitionErr.Name != "runtime_catalog" {
		t.Fatalf("second WithRuntimeCatalog error = %T %v, want duplicate runtime_catalog option", err, err)
	}
}
