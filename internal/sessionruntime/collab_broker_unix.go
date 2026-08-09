//go:build aix || darwin || dragonfly || freebsd || hurd || illumos || linux || netbsd || openbsd || solaris

package sessionruntime

import "net"

func collabPlatformSupported() bool { return true }

func listenCollabEndpoint(endpoint string) (net.Listener, error) {
	return net.Listen("unix", endpoint)
}
