// Package session holds the state a live terminal session needs, independent
// of how clients reach it.
package session

import "sync"

// Ring is a fixed-size buffer of recent PTY output addressed by absolute byte
// offset. Offsets never wrap or reset for the life of a session, so a
// reconnecting client only has to remember "I have everything before N".
//
// Ring is safe for concurrent use.
type Ring struct {
	mu   sync.RWMutex
	buf  []byte
	head uint64 // total bytes ever written
}

// NewRing returns a Ring retaining the most recent size bytes.
func NewRing(size int) *Ring {
	if size <= 0 {
		panic("session: ring size must be positive")
	}
	return &Ring{buf: make([]byte, size)}
}

// Write appends p, discarding whatever no longer fits. It never fails.
func (r *Ring) Write(p []byte) (int, error) {
	total := len(p)
	if total == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	cap := len(r.buf)
	if total > cap {
		// Everything but the final cap bytes is discarded on arrival, but it
		// still advances the offset: the bytes existed.
		r.head += uint64(total - cap) //nolint:gosec // both operands are nonnegative slice lengths
		p = p[total-cap:]
	}
	// Writing at head%cap keeps the invariant that byte at offset o lives at
	// index o%cap, which is what makes Since a pair of copies.
	off := int(r.head % uint64(cap)) //nolint:gosec // modulo by an int-sized buffer capacity fits in int
	n := copy(r.buf[off:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
	}
	r.head += uint64(len(p))
	return total, nil
}

// Head returns the offset one past the last byte written, i.e. the sequence
// number the next byte will have.
func (r *Ring) Head() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.head
}

// Tail returns the oldest offset still replayable.
func (r *Ring) Tail() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tail()
}

// Last returns a copy of at most size trailing bytes from one consistent ring
// state. It never returns bytes older than the replay window.
func (r *Ring) Last(size int) []byte {
	if size <= 0 {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	available := r.head - r.tail()
	if uint64(size) < available {
		available = uint64(size)
	}
	start := r.head - available
	out := make([]byte, int(available)) //nolint:gosec // available is bounded by the int-sized buffer length
	if len(out) == 0 {
		return out
	}
	offset := int(start % uint64(len(r.buf))) //nolint:gosec // modulo by an int-sized buffer length fits in int
	copied := copy(out, r.buf[offset:])
	if copied < len(out) {
		copy(out[copied:], r.buf)
	}
	return out
}

func (r *Ring) tail() uint64 {
	if c := uint64(len(r.buf)); r.head > c {
		return r.head - c
	}
	return 0
}

// Since returns a copy of every byte from offset seq onward, and the offset
// those bytes end at. ok is false when seq has fallen out of the replay window
// or is ahead of what we have written, in which cases the caller must fall
// back to repainting the screen from a snapshot.
func (r *Ring) Since(seq uint64) (b []byte, head uint64, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if seq > r.head || seq < r.tail() {
		return nil, r.head, false
	}
	n := int(r.head - seq) //nolint:gosec // a valid replay range is bounded by the int-sized buffer length
	out := make([]byte, n)
	if n > 0 {
		off := int(seq % uint64(len(r.buf))) //nolint:gosec // modulo by an int-sized buffer length fits in int
		c := copy(out, r.buf[off:])
		if c < n {
			copy(out[c:], r.buf)
		}
	}
	return out, r.head, true
}
