package sessionruntime

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

func fingerprintFromDefinition(definition loop.Definition) event.ConfigFingerprint {
	return FingerprintFrom(bindFingerprintDefinition(definition))
}

func fingerprintWithDefinition(definition loop.Definition, fields ConfigFingerprintFields) event.ConfigFingerprint {
	return fingerprintWith(bindFingerprintDefinition(definition), fields)
}

func bindFingerprintDefinition(definition loop.Definition) loop.BoundDefinition {
	sessionID, _ := uuid.New()
	loopID, _ := uuid.New()
	bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: sessionID, LoopID: loopID})
	if err != nil {
		panic(err)
	}
	return bound
}
