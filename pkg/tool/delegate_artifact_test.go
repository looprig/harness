package tool

import (
	"reflect"
	"testing"
)

func TestDelegateRuntimeExposesExecutionMetadataOnly(t *testing.T) {
	t.Parallel()

	runtimeType := reflect.TypeOf(DelegateRuntime{})
	if _, present := runtimeType.FieldByName("Advertised"); present {
		t.Fatal("DelegateRuntime exposes presentation-only Advertised metadata")
	}
	if _, present := runtimeType.FieldByName("Explicit"); !present {
		t.Fatal("DelegateRuntime does not expose caller Explicit metadata")
	}
}
