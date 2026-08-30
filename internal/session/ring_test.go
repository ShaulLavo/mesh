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

func writeRing(t *testing.T, ring *Ring, contents []byte) {
	t.Helper()
	if _, err := ring.Write(contents); err != nil {
		t.Fatal(err)
	}
}
