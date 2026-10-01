//go:build !linux

package edge

import "net/http"

// Without a verified socket owner, loopback is a peer address, not forwarded identity.
func trustedLoopbackForwarder(*http.Request) bool { return false }
