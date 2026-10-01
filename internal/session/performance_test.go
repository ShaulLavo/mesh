package session

import (
	"bytes"
	"testing"
)

func BenchmarkRingReplay(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{{"32K", 32 << 10}, {"4M", 4 << 20}} {
		b.Run(size.name, func(b *testing.B) {
			ring := NewRing(4 << 20)
			_, _ = ring.Write(bytes.Repeat([]byte("x"), 4<<20))
			start := ring.Head() - uint64(size.bytes) //nolint:gosec // positive bounded test sizes
			b.SetBytes(int64(size.bytes))
			b.ReportAllocs()
			for b.Loop() {
				data, _, ok := ring.Since(start)
				if !ok || len(data) != size.bytes {
					b.Fatal("replay window mismatch")
				}
			}
		})
	}
}
