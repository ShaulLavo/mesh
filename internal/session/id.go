package session

import (
	"crypto/rand"
	"fmt"
)

// idAlphabet is Crockford base32: no I, L, O or U, so session IDs survive
// being read aloud, retyped, or scribbled down.
const idAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// IDLen is the number of characters in a session ID.
const IDLen = 4

// NewID returns a random session ID. Collisions are the caller's problem to
// detect: with 32^4 space a host is expected to collide only once it is
// juggling hundreds of live sessions, and retrying is cheap.
func NewID() (string, error) {
	b := make([]byte, IDLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: generate id: %w", err)
	}
	out := make([]byte, IDLen)
	for i, v := range b {
		out[i] = idAlphabet[int(v)%len(idAlphabet)]
	}
	return string(out), nil
}

// NormalizeID upper-cases an ID as typed, so `m 7k3d` finds 7K3D.
func NormalizeID(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - 'a' + 'A'
		}
	}
	return string(out)
}
