package rig

import (
	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

type ConfigFingerprintFields = sessionruntime.ConfigFingerprintFields

func FingerprintFrom(cfg loop.BoundDefinition) event.ConfigFingerprint {
	return sessionruntime.FingerprintFrom(cfg)
}

func fingerprintWith(cfg loop.BoundDefinition, fields ConfigFingerprintFields) event.ConfigFingerprint {
	return sessionruntime.FingerprintWith(cfg, fields)
}
