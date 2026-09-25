package rig

import (
	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/present"
)

// WithMessagePresenter installs one presenter for new and restored sessions.
// Its rendering is journaled, so changing it affects only subsequent messages
// and does not change the rig's configuration fingerprint.
func WithMessagePresenter(p present.Presenter) Option {
	return func(state *definitionState) error {
		if state.seen[keyMessagePresenter] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keyMessagePresenter)}
		}
		if nilInterfaceValue(p) {
			return &DefinitionError{Kind: DefinitionInvalidMessagePresenter}
		}
		state.seen[keyMessagePresenter] = true
		state.lifecycleOptions = append(state.lifecycleOptions, sessionruntime.WithLifecycleMessagePresenter(p))
		return nil
	}
}
