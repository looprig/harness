package registry

import (
	"fmt"
	"strings"
)

// DuplicateNameError is returned by Registry.Register when a name is already
// bound to a constructor. It is a leaf error carrying the offending name;
// callers may errors.As to inspect it. Registration fails secure (the existing
// binding is never overwritten).
type DuplicateNameError struct{ Name string }

func (e DuplicateNameError) Error() string {
	return fmt.Sprintf("registry: name %q already registered", e.Name)
}

// UnknownNameError is returned by Registry.Open when a name was never
// registered. Known lists the registered names in ascending order so callers
// can surface valid choices. It is a leaf error with context fields and is
// errors.As-able.
type UnknownNameError struct {
	Name  string
	Known []string
}

func (e UnknownNameError) Error() string {
	return fmt.Sprintf("registry: unknown name %q (known: %s)", e.Name, strings.Join(e.Known, ", "))
}
