package sessionstore

import (
	"bytes"

	durablestore "github.com/looprig/sessionstore"
)

// releasedEnvelopeMagic is derived through the pinned released codec rather
// than duplicating its private LRJE constant. A frame carrying this magic belongs
// to the released grammar: any decode error is therefore authoritative and must
// never fall through to the legacy JSON decoder.
var releasedEnvelopeMagic = func() [4]byte {
	frame, err := durablestore.EncodeEnvelope(durablestore.Envelope{
		Kind:       durablestore.EnvelopeKindOpeningFence,
		LeaseEpoch: 1,
	})
	if err != nil || len(frame) < 4 {
		panic("sessionstore: released envelope codec cannot produce its magic")
	}
	var magic [4]byte
	copy(magic[:], frame[:4])
	return magic
}()

func hasReleasedEnvelopeMagic(frame []byte) bool {
	return len(frame) >= len(releasedEnvelopeMagic) && bytes.Equal(frame[:len(releasedEnvelopeMagic)], releasedEnvelopeMagic[:])
}
