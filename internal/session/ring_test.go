package session

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestRingReplaysEverythingThatFits(t *testing.T) {
	r := NewRing(64)
	writeRing(t, r, []byte("hello "))
	writeRing(t, r, []byte("world"))

	got, head, ok := r.Since(0)
	if !ok {
		t.Fatal("Since(0) not ok")
	}
	if string(got) != "hello world" {
		t.Fatalf("got %q", got)
	}
	if head != 11 {
		t.Fatalf("head = %d, want 11", head)
	}

	got, _, ok = r.Since(6)
	if !ok || string(got) != "world" {
		t.Fatalf("Since(6) = %q, %v", got, ok)
	}

	got, _, ok = r.Since(11)
	if !ok || len(got) != 0 {
		t.Fatalf("Since(head) = %q, %v; want empty, ok", got, ok)
	}
}

func TestRingDropsOldestAndReportsWindow(t *testing.T) {
	r := NewRing(8)
	writeRing(t, r, []byte("0123456789")) // 10 bytes into an 8 byte window

	if got, want := r.Head(), uint64(10); got != want {
		t.Fatalf("Head = %d, want %d", got, want)
	}
	if got, want := r.Tail(), uint64(2); got != want {
		t.Fatalf("Tail = %d, want %d", got, want)
	}
	if _, _, ok := r.Since(1); ok {
		t.Fatal("Since(1) should have fallen out of the window")
	}
	got, _, ok := r.Since(2)
	if !ok || string(got) != "23456789" {
		t.Fatalf("Since(2) = %q, %v", got, ok)
	}
}

func TestRingRejectsFutureOffsets(t *testing.T) {
	r := NewRing(16)
	writeRing(t, r, []byte("abc"))
	if _, _, ok := r.Since(4); ok {
		t.Fatal("Since past head should not be ok")
	}
}

// The offset arithmetic is the part most likely to be subtly wrong, so check
// it against a plain byte slice under randomized writes that wrap repeatedly.
func TestRingMatchesReferenceUnderWrapping(t *testing.T) {
	const size = 37
	r := NewRing(size)
	var ref []byte
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test data is intentional

	for i := 0; i < 500; i++ {
		chunk := make([]byte, rng.Intn(90))
		if _, err := rng.Read(chunk); err != nil {
			t.Fatal(err)
		}
		writeRing(t, r, chunk)
		ref = append(ref, chunk...)

		tail := r.Tail()
		if int(r.Head()) != len(ref) { //nolint:gosec // the test head equals the int-sized reference slice length
			t.Fatalf("iteration %d: head = %d, want %d", i, r.Head(), len(ref))
		}
		seq := tail + uint64(rng.Intn(int(r.Head()-tail)+1)) //nolint:gosec // the replay span is bounded by the int-sized ring buffer
		got, head, ok := r.Since(seq)
		if !ok {
			t.Fatalf("iteration %d: Since(%d) not ok (tail %d head %d)", i, seq, tail, r.Head())
		}
		if head != r.Head() {
			t.Fatalf("iteration %d: head mismatch", i)
		}
		if want := ref[seq:]; !bytes.Equal(got, want) {
			t.Fatalf("iteration %d: Since(%d) mismatch\n got %q\nwant %q", i, seq, got, want)
		}
	}
}

// Chunk boundaries must be invisible: the same writes against every chunk
// shape, including a short final chunk and writes wider than the window, have
// to replay exactly what a flat buffer would.
func TestChunkedRingMatchesReferenceAcrossChunkShapes(t *testing.T) {
	for _, shape := range []struct{ size, chunk int }{
		{1, 1}, {7, 1}, {7, 3}, {8, 4}, {37, 5}, {37, 37}, {37, 64}, {64, 16}, {100, 7},
	} {
		r := newRing(shape.size, shape.chunk)
		var ref []byte
		rng := rand.New(rand.NewSource(int64(shape.size*1000 + shape.chunk))) //nolint:gosec // deterministic test data is intentional
		for i := 0; i < 400; i++ {
			chunk := make([]byte, rng.Intn(shape.size*2+2))
			if _, err := rng.Read(chunk); err != nil {
				t.Fatal(err)
			}
			writeRing(t, r, chunk)
			ref = append(ref, chunk...)

			head, tail := r.Head(), r.Tail()
			wantTail := uint64(max(0, len(ref)-shape.size)) //nolint:gosec // nonnegative test length
			if head != uint64(len(ref)) || tail != wantTail {
				t.Fatalf("%+v iteration %d: head %d tail %d, want %d %d", shape, i, head, tail, len(ref), wantTail)
			}
			for seq := tail; seq <= head; seq++ {
				got, gotHead, ok := r.Since(seq)
				if !ok || gotHead != head || !bytes.Equal(got, ref[seq:]) {
					t.Fatalf("%+v iteration %d: Since(%d) = %q, %d, %v; want %q", shape, i, seq, got, gotHead, ok, ref[seq:])
				}
			}
			if _, _, ok := r.Since(head + 1); ok {
				t.Fatalf("%+v iteration %d: Since past head was ok", shape, i)
			}
			if tail > 0 {
				if _, _, ok := r.Since(tail - 1); ok {
					t.Fatalf("%+v iteration %d: Since before tail was ok", shape, i)
				}
			}
			n := rng.Intn(shape.size + 3)
			want := ref[max(int(tail), len(ref)-n):] //nolint:gosec // tail is bounded by the reference length
			if got := r.Last(n); !bytes.Equal(got, want) {
				t.Fatalf("%+v iteration %d: Last(%d) = %q, want %q", shape, i, n, got, want)
			}
		}
	}
}

func TestRingAllocatesOnlyWhatWasWritten(t *testing.T) {
	r := newRing(64, 16)
	if got := allocatedChunks(r); got != 0 {
		t.Fatalf("new ring allocated %d chunks", got)
	}
	writeRing(t, r, make([]byte, 17))
	if got := allocatedChunks(r); got != 2 {
		t.Fatalf("17 bytes allocated %d chunks, want 2", got)
	}
	writeRing(t, r, make([]byte, 100))
	if got := allocatedChunks(r); got != 4 {
		t.Fatalf("a wrapped ring allocated %d chunks, want 4", got)
	}

	short := newRing(20, 16)
	writeRing(t, short, make([]byte, 20))
	if got := len(short.chunks[1]); got != 4 {
		t.Fatalf("final chunk has %d bytes, want the 4 left in the window", got)
	}
}

func TestDefaultRingStartsEmpty(t *testing.T) {
	r := NewRing(4 << 20)
	writeRing(t, r, []byte("$ "))
	if got := allocatedChunks(r); got != 1 {
		t.Fatalf("a prompt allocated %d chunks, want 1", got)
	}
}

func allocatedChunks(r *Ring) int {
	n := 0
	for _, c := range r.chunks {
		if c != nil {
			n++
		}
	}
	return n
}

func writeRing(t *testing.T, ring *Ring, contents []byte) {
	t.Helper()
	if _, err := ring.Write(contents); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkRingWritePTYRead(b *testing.B) {
	r := NewRing(4 << 20)
	p := bytes.Repeat([]byte("x"), 32<<10)
	b.SetBytes(int64(len(p)))
	for b.Loop() {
		_, _ = r.Write(p)
	}
}

func BenchmarkRingWriteSmall(b *testing.B) {
	r := NewRing(4 << 20)
	p := []byte("output line 12345\r\n")
	b.SetBytes(int64(len(p)))
	for b.Loop() {
		_, _ = r.Write(p)
	}
}
