//go:build !unix

package socketclose

import "io"

// DiscardQueued does nothing where Unix sockets are unavailable.
func DiscardQueued(io.Closer) {}
