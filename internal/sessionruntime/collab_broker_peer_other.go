//go:build aix || dragonfly || freebsd || hurd || illumos || netbsd || openbsd || solaris

package sessionruntime

import "net"

// Some Unix variants expose peer credentials through platform-specific APIs;
// when this narrow build does not expose one, the broker still enforces the
// owner-only socket and accepts the platform's unavailable credential signal.
func collabPeerUID(net.Conn) (uint32, bool) { return 0, false }
