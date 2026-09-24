// Package session holds the state a live terminal session needs, independent
// of how clients reach it.
package session

import "sync"

// Ring is a bounded buffer of recent PTY output addressed by absolute byte
// offset. Offsets never wrap or reset for the life of a session, so a
// reconnecting client only has to remember "I have everything before N".
//
// Storage is a fixed array of chunks allocated on first write, so a quiet
// session pays for what it printed rather than for the whole window. Chunking
// keeps each byte at the index a flat ring would use, so offsets, the window
// and wraparound are unchanged.
//
// Ring is safe for concurrent use.
type Ring struct {
	mu     sync.RWMutex
	chunks [][]byte
	chunk  int    // capacity of every chunk but possibly the last
	size   int    // bytes retained once the window is full
	head   uint64 // total bytes ever written
}

// ringChunkSize is small enough that an idle shell's prompt costs one chunk,
// and large enough that a full PTY read touches at most three.
const ringChunkSize = 16 << 10

// NewRing returns a Ring retaining the most recent size bytes.
func NewRing(size int) *Ring {
	return newRing(size, ringChunkSize)
}

func newRing(size, chunk int) *Ring {
	if size <= 0 {
		panic("session: ring size must be positive")
	}
	chunk = min(chunk, size)
	return &Ring{
		chunks: make([][]byte, (size+chunk-1)/chunk),
		chunk:  chunk,
		size:   size,
	}
}

// Write appends p, discarding whatever no longer fits. It never fails.
func (r *Ring) Write(p []byte) (int, error) {
	total := len(p)
	if total == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if total > r.size {
		// Everything but the final size bytes is discarded on arrival, but it
		// still advances the offset: the bytes existed.
		r.head += uint64(total - r.size) //nolint:gosec // both operands are nonnegative slice lengths
		p = p[total-r.size:]
	}
	r.writeAt(r.index(r.head), p)
	r.head += uint64(len(p))
	return total, nil
}

// index maps an absolute offset to its position in the window. Byte o always
// lives at o%size, which is what makes a replay a contiguous walk.
func (r *Ring) index(offset uint64) int {
	return int(offset % uint64(r.size)) //nolint:gosec // modulo by an int-sized window fits in int
}

// writeAt copies p into the window from idx, wrapping at size. p is never
// longer than size.
func (r *Ring) writeAt(idx int, p []byte) {
	for len(p) > 0 {
		c := idx / r.chunk
		if r.chunks[c] == nil {
			r.chunks[c] = make([]byte, min(r.chunk, r.size-c*r.chunk))
		}
		n := copy(r.chunks[c][idx-c*r.chunk:], p)
		p = p[n:]
		idx = (idx + n) % r.size
	}
}

// readAt fills dst from the window starting at idx. Every position in the
// replay window has been written, so its chunk exists.
func (r *Ring) readAt(dst []byte, idx int) {
	for len(dst) > 0 {
		c := idx / r.chunk
		n := copy(dst, r.chunks[c][idx-c*r.chunk:])
		dst = dst[n:]
		idx = (idx + n) % r.size
	}
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
	out := make([]byte, int(available)) //nolint:gosec // available is bounded by the int-sized window
	r.readAt(out, r.index(start))
	return out
}

func (r *Ring) tail() uint64 {
	if c := uint64(r.size); r.head > c { //nolint:gosec // the window size is a positive int set at construction
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
	r.readAt(out, r.index(seq))
	return out, r.head, true
}
