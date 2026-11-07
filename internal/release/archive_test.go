package release

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/andybalholm/brotli"
)

func archiveTar(t testing.TB, header tar.Header, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	if err := writer.WriteHeader(&header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func meshTar(t testing.TB, body []byte) []byte {
	t.Helper()
	return archiveTar(t, tar.Header{Name: "mesh", Typeflag: tar.TypeReg, Mode: 0700, Size: int64(len(body))}, body)
}

func encodeArchive(t testing.TB, format ArchiveFormat, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	var writer io.WriteCloser
	if format == ArchiveGzip {
		return testGzipBytes(t, raw)
	}
	writer = brotli.NewWriterLevel(&out, 1)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestReadExecutableFormats(t *testing.T) {
	for _, format := range []ArchiveFormat{ArchiveGzip, ArchiveBrotli} {
		t.Run(string(format), func(t *testing.T) { testReadExecutableFormat(t, format) })
	}
}

func testReadExecutableFormat(t *testing.T, format ArchiveFormat) {
	t.Helper()
	body := []byte("mesh executable")
	raw := meshTar(t, body)
	compressed := encodeArchive(t, format, raw)
	second := meshTar(t, []byte("extra mesh"))
	cases := []struct {
		name    string
		archive []byte
		valid   bool
	}{
		{"valid", compressed, true},
		{"bounded zero tail", encodeArchive(t, format, append(bytes.Clone(raw), make([]byte, 8192)...)), true},
		{"zero tail at limit", encodeArchive(t, format, append(bytes.Clone(raw), make([]byte, maximumArchiveTrailer)...)), true},
		{"excess zero tail", encodeArchive(t, format, append(bytes.Clone(raw), make([]byte, maximumArchiveTrailer+1)...)), false},
		{"nonzero tail", encodeArchive(t, format, append(bytes.Clone(raw), 'x')), false},
		{"second tar", encodeArchive(t, format, append(bytes.Clone(raw), second...)), false},
		{"second member", encodeArchive(t, format, append(bytes.Clone(raw[:len(raw)-1024]), second...)), false},
		{"second compressed tar", append(bytes.Clone(compressed), compressed...), false},
		{"compressed junk", append(bytes.Clone(compressed), []byte("JUNK")...), false},
		{"header truncated", compressed[:1], false},
		{"middle truncated", compressed[:len(compressed)/2], false},
		{"last byte truncated", compressed[:len(compressed)-1], false},
		{"missing body", encodeArchive(t, format, raw[:513]), false},
		{"complete member without end blocks", encodeArchive(t, format, raw[:len(raw)-1024]), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertExecutable(t, format, bytes.NewReader(tc.archive), body, tc.valid) })
	}
	assertExecutable(t, format, iotest.OneByteReader(bytes.NewReader(compressed)), body, true)
	for _, name := range []string{"./mesh", "dir/mesh"} {
		archive := archiveTar(t, tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(body))}, body)
		assertExecutable(t, format, bytes.NewReader(encodeArchive(t, format, archive)), body, false)
	}
	for _, flag := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeDir} {
		archive := archiveTar(t, tar.Header{Name: "mesh", Typeflag: flag, Linkname: "other"}, nil)
		assertExecutable(t, format, bytes.NewReader(encodeArchive(t, format, archive)), body, false)
	}
	archive := archiveTar(t, tar.Header{Name: "mesh/", Typeflag: tar.TypeDir}, nil)
	assertExecutable(t, format, bytes.NewReader(encodeArchive(t, format, archive)), nil, false)
	var oversized bytes.Buffer
	tw := tar.NewWriter(&oversized)
	if err := tw.WriteHeader(&tar.Header{Name: "mesh", Size: maximumBinary + 1}); err != nil {
		t.Fatal(err)
	}
	assertExecutable(t, format, bytes.NewReader(encodeArchive(t, format, oversized.Bytes())), nil, false)
}

func assertExecutable(t testing.TB, format ArchiveFormat, source io.Reader, body []byte, valid bool) {
	t.Helper()
	var destination bytes.Buffer
	executable, err := ReadExecutable(context.Background(), source, format, &destination)
	if !valid {
		if err == nil {
			t.Fatal("accepted invalid archive")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if executable.Size != int64(len(body)) || executable.SHA256 != testDigest(body) || !bytes.Equal(destination.Bytes(), body) {
		t.Fatalf("executable %+v, body %q", executable, destination.Bytes())
	}
}

func TestBrotliRejectsLargeWindowBeforeAllocation(t *testing.T) {
	// WINDOW_BITS=17 plus the large-window extension is disallowed by this decoder.
	input := bytes.NewReader(append([]byte{0x11}, make([]byte, 64<<10)...))
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	decoder, err := decompressor(ArchiveBrotli, iotest.OneByteReader(input))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, decoder)
	runtime.ReadMemStats(&after)
	if err == nil || err.Error() != "brotli: WINDOW_BITS" {
		t.Fatalf("large window error = %v", err)
	}
	if consumed := (64 << 10) + 1 - input.Len(); consumed != 1 {
		t.Fatalf("header consumed %d bytes, want 1", consumed)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= 4<<20 {
		t.Fatalf("allocated %d bytes before rejection", allocated)
	}
}

func TestArchiveLimitReaderBoundaries(t *testing.T) {
	for _, size := range []int{7, 8, 9, 10} {
		input := bytes.NewReader(make([]byte, size))
		bounded := &archiveLimitReader{source: input, remaining: 8}
		count, err := io.Copy(io.Discard, bounded)
		if (err == nil) != (size <= 8) || count > 9 {
			t.Fatalf("size %d: count %d, error %v", size, count, err)
		}
		if size-input.Len() > 9 {
			t.Fatal("read beyond the extra-byte probe")
		}
	}
}

func TestArchiveCountsHiddenMetadataAndTail(t *testing.T) {
	raw := archiveTar(t, tar.Header{Name: "mesh", Format: tar.FormatPAX, PAXRecords: map[string]string{"comment": strings.Repeat("metadata", 300)}}, nil)
	raw = append(raw, make([]byte, 137)...)
	for _, limit := range []int64{int64(len(raw) - 1), int64(len(raw))} {
		bounded := &archiveLimitReader{source: bytes.NewReader(raw), remaining: limit}
		_, err := readArchiveExecutable(t.Context(), bounded, io.Discard)
		if (err == nil) != (limit == int64(len(raw))) {
			t.Fatalf("physical byte budget %d: %v", limit, err)
		}
		if bounded.remaining != limit-int64(len(raw)) {
			t.Fatalf("hidden metadata not counted: %d remaining", bounded.remaining)
		}
	}
	for _, format := range []ArchiveFormat{ArchiveGzip, ArchiveBrotli} {
		assertExecutable(t, format, bytes.NewReader(encodeArchive(t, format, raw)), nil, true)
	}
}

type callbackReader struct {
	source io.Reader
	after  func()
}

func (r callbackReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	r.after()
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("fixture read: %w", err)
	}
	return n, nil
}

type callbackWriter struct{ write func([]byte) (int, error) }

func (w callbackWriter) Write(p []byte) (int, error) { return w.write(p) }

func TestArchiveCancellationAndWriteErrors(t *testing.T) {
	for _, format := range []ArchiveFormat{ArchiveGzip, ArchiveBrotli} {
		t.Run(string(format), func(t *testing.T) { testArchiveCancellation(t, format) })
	}
}

func testArchiveCancellation(t *testing.T, format ArchiveFormat) {
	t.Helper()
	body := bytes.Repeat([]byte("x"), 3*archiveChunk)
	input := encodeArchive(t, format, meshTar(t, body))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assertArchiveCanceled(ctx, t, format, bytes.NewReader(input), io.Discard)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	assertArchiveCanceled(ctx, t, format, callbackReader{source: bytes.NewReader(input), after: cancel}, io.Discard)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	written := 0
	writer := callbackWriter{write: func(p []byte) (int, error) {
		if len(p) > archiveChunk {
			t.Fatalf("unbounded copy chunk %d", len(p))
		}
		written += len(p)
		if written == len(body) {
			cancel()
		}
		return len(p), nil
	}}
	assertArchiveCanceled(ctx, t, format, bytes.NewReader(input), writer)
	if written != len(body) {
		t.Fatalf("final-write cancellation after %d bytes", written)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	// Cancel while already-buffered compressed bytes still contain decoded work.
	writer = callbackWriter{write: func(p []byte) (int, error) { cancel(); return len(p), nil }}
	assertArchiveCanceled(ctx, t, format, bytes.NewReader(input), writer)
	problem := errors.New("destination failed")
	_, err := ReadExecutable(t.Context(), bytes.NewReader(input), format, callbackWriter{write: func([]byte) (int, error) { return 0, problem }})
	if !errors.Is(err, problem) {
		t.Fatalf("writer failure: %v", err)
	}
	_, err = ReadExecutable(t.Context(), bytes.NewReader(input), format, callbackWriter{write: func([]byte) (int, error) { return 0, nil }})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short writer: %v", err)
	}
}

func assertArchiveCanceled(ctx context.Context, t *testing.T, format ArchiveFormat, source io.Reader, destination io.Writer) {
	t.Helper()
	if _, err := ReadExecutable(ctx, source, format, destination); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestArchiveTrailerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := callbackReader{source: bytes.NewReader(make([]byte, 17)), after: cancel}
	if err := validateArchiveTail(archiveContextReader{ctx: ctx, source: source}); !errors.Is(err, context.Canceled) {
		t.Fatalf("trailer cancellation: %v", err)
	}
}

func TestArchiveUnknownFormat(t *testing.T) {
	if _, err := ReadExecutable(t.Context(), bytes.NewReader(nil), ArchiveFormat("zip"), io.Discard); err == nil {
		t.Fatal("accepted unsupported format")
	}
}

func FuzzReadExecutable(f *testing.F) {
	for _, format := range []ArchiveFormat{ArchiveGzip, ArchiveBrotli} {
		f.Add(string(format), encodeArchive(f, format, meshTar(f, []byte("mesh"))))
	}
	f.Fuzz(func(t *testing.T, format string, input []byte) {
		_, _ = ReadExecutable(t.Context(), bytes.NewReader(input), ArchiveFormat(format), io.Discard)
	})
}
