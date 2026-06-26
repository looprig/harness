package foreignloop

import "io"

// DecodeStream is the adapter seam over the package-private stream decoder: it
// turns a claude --output-format stream-json stdout into ForeignEvents. The
// returned func reports the first decode error after the channel closes.
func DecodeStream(r io.Reader) (<-chan ForeignEvent, func() error) { return decodeStream(r) }
