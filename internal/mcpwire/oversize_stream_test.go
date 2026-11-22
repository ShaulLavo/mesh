package mcpwire

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
)

func TestOversizedStdioScalarStreamDiscardBoundedMemory(t *testing.T) {
	const scalarBytes = 14 << 20
	// Single 14 MiB string with ID after it; stays within MaxMessage + MaxStdioDrain.
	prefix := `{"jsonrpc":"2.0","result":{"blob":"`
	suffix := `"},"id":"after-big-str"}` + "\n"
	next := envResult("2", `{"resultType":"complete","content":[]}`)
	makeReader := func() io.Reader {
		return io.MultiReader(
			strings.NewReader(prefix),
			io.LimitReader(repeatingByte{'a'}, scalarBytes),
			strings.NewReader(suffix),
			bytes.NewReader(append(next, '\n')),
		)
	}

	// Warm up once, then measure total heap bytes allocated by ReadReply.
	reader := NewStdioReader(makeReader())
	_, err := reader.ReadReply()
	var oversized *OversizedStdioReply
	if !errors.As(err, &oversized) || oversized.ID.raw != `"after-big-str"` {
		t.Fatalf("14 MiB string reply = %v (oversized=%+v), want id \"after-big-str\"", err, oversized)
	}
	if raw, err := reader.ReadReply(); err != nil || !bytes.Equal(raw, next) {
		t.Fatalf("following frame after 14 MiB string = %q, %v", raw, err)
	}

	// Compare heap allocations of ReadReply for a 4 MiB scalar vs a 14 MiB scalar:
	// StdioReader buffers only up to MaxMessage+2 (~3.5 MiB) before switching to
	// streaming discard, so adding 10 MiB to the scalar must add near-zero allocs.
	measureReadReply := func(size int, idLabel string) uint64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		r := NewStdioReader(io.MultiReader(
			strings.NewReader(prefix),
			io.LimitReader(repeatingByte{'a'}, int64(size)),
			strings.NewReader(`"},"id":"`+idLabel+`"}`+"\n"),
			bytes.NewReader(append(next, '\n')),
		))
		_, err := r.ReadReply()
		runtime.ReadMemStats(&after)
		if !errors.As(err, &oversized) || oversized.ID.raw != `"`+idLabel+`"` {
			t.Fatalf("%d-byte scalar reply = %v", size, err)
		}
		return after.TotalAlloc - before.TotalAlloc
	}
	alloc4 := measureReadReply(4<<20, "s4m")
	alloc14 := measureReadReply(scalarBytes, "s14m")
	if alloc14 > alloc4+(1<<20) {
		t.Fatalf("oversize attribution buffered scalar: 4 MiB alloc=%d, 14 MiB alloc=%d (delta=%d)", alloc4, alloc14, alloc14-alloc4)
	}

	// Direct DrainOversizedReply with a 14 MiB scalar must allocate tiny bounded
	// memory (< 1 MiB) since prefix is already supplied.
	drainPrefix := []byte(`{"jsonrpc":"2.0","result":{"blob":"`)
	drainTail := func() *bufio.Reader {
		return bufio.NewReaderSize(io.MultiReader(
			io.LimitReader(repeatingByte{'b'}, scalarBytes),
			strings.NewReader(`"},"id":"direct-after"}`+"\n"),
		), 4096)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	id, err := DrainOversizedReply(drainTail(), drainPrefix, false)
	runtime.ReadMemStats(&after)
	if err != nil || id.raw != `"direct-after"` {
		t.Fatalf("DrainOversizedReply 14 MiB string: id=%q err=%v", id.raw, err)
	}
	drainAlloc := after.TotalAlloc - before.TotalAlloc
	if drainAlloc > 256<<10 {
		t.Fatalf("DrainOversizedReply allocated %d bytes for 14 MiB scalar (want < 256 KiB)", drainAlloc)
	}

	// Single 14 MiB numeric scalar with ID after it.
	numReader := NewStdioReader(io.MultiReader(
		strings.NewReader(`{"jsonrpc":"2.0","result":{"n":1`),
		io.LimitReader(repeatingByte{'0'}, scalarBytes),
		strings.NewReader(`},"id":42}`+"\n"),
		bytes.NewReader(append(next, '\n')),
	))
	_, err = numReader.ReadReply()
	if !errors.As(err, &oversized) || oversized.ID.raw != "42" {
		t.Fatalf("14 MiB number reply = %v (oversized=%+v), want id 42", err, oversized)
	}
	if raw, err := numReader.ReadReply(); err != nil || !bytes.Equal(raw, next) {
		t.Fatalf("following frame after 14 MiB number = %q, %v", raw, err)
	}

	// A line exceeding MaxMessage + MaxStdioDrain fails with ErrFramingLost.
	tooBig := NewStdioReader(io.MultiReader(
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"blob":"`),
		io.LimitReader(repeatingByte{'z'}, MaxMessage+MaxStdioDrain+1024),
		strings.NewReader(`"}}`+"\n"),
	))
	if _, err := tooBig.ReadReply(); !errors.Is(err, ErrFramingLost) {
		t.Fatalf("line exceeding drain cap = %v, want ErrFramingLost", err)
	}
}

type repeatingByte struct{ b byte }

func (r repeatingByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}
