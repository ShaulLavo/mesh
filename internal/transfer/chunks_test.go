package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"
)

func TestUploadChunksAndOffsets(t *testing.T) {
	data := bytes.Repeat([]byte("x"), ChunkSize*2+7)
	var got []byte
	var offsets []int64
	err := Upload(context.Background(), bytes.NewReader(data), func(_ context.Context, offset int64, chunk []byte) error {
		if len(chunk) > ChunkSize {
			t.Fatalf("chunk length %d exceeds limit", len(chunk))
		}
		offsets = append(offsets, offset)
		got = append(got, chunk...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("uploaded data changed")
	}
	wantOffsets := []int64{0, ChunkSize, 2 * ChunkSize}
	if !slices.Equal(offsets, wantOffsets) {
		t.Fatalf("offsets = %v, want %v", offsets, wantOffsets)
	}
}

func TestUploadRejectsStalledReader(t *testing.T) {
	if err := Upload(context.Background(), errorReader{}, func(context.Context, int64, []byte) error { return nil }); !errors.Is(err, ErrStalled) {
		t.Fatalf("stalled upload error = %v", err)
	}
}

func TestUploadPropagatesReadAndSendErrors(t *testing.T) {
	readErr := errors.New("read failed")
	if err := Upload(context.Background(), errorReader{err: readErr}, func(context.Context, int64, []byte) error { return nil }); !errors.Is(err, readErr) {
		t.Fatalf("read error = %v", err)
	}
	sendErr := errors.New("send failed")
	if err := Upload(context.Background(), bytes.NewReader([]byte("x")), func(context.Context, int64, []byte) error { return sendErr }); !errors.Is(err, sendErr) {
		t.Fatalf("send error = %v", err)
	}
}

func TestUploadHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := Upload(ctx, bytes.NewReader([]byte("x")), func(context.Context, int64, []byte) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("upload cancellation = %v, callback called %t", err, called)
	}
}

func TestDownloadBoundariesAndCompletion(t *testing.T) {
	data := bytes.Repeat([]byte("y"), ChunkSize+3)
	chunks := []Chunk{{Data: data[:ChunkSize]}, {Data: data[ChunkSize:], Done: true}}
	var calls int
	var got bytes.Buffer
	total, err := Download(context.Background(), &got, func(_ context.Context, offset int64) (Chunk, error) {
		if offset != int64(calls*ChunkSize) {
			t.Fatalf("offset = %d, want %d", offset, calls*ChunkSize)
		}
		chunk := chunks[calls]
		calls++
		return chunk, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != int64(len(data)) || !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("download = %d bytes, data match %t", total, bytes.Equal(got.Bytes(), data))
	}
}

func TestDownloadRejectsInvalidProgressAndSize(t *testing.T) {
	tests := []struct {
		name  string
		chunk Chunk
		want  error
	}{
		{name: "stalled", want: ErrStalled},
		{name: "too large", chunk: Chunk{Data: make([]byte, ChunkSize+1), Done: true}, want: ErrChunkTooLarge},
	}
	for _, test := range tests {
		_, err := Download(context.Background(), io.Discard, func(context.Context, int64) (Chunk, error) { return test.chunk, nil })
		if !errors.Is(err, test.want) {
			t.Errorf("%s error = %v, want %v", test.name, err, test.want)
		}
	}
}

func TestDownloadPropagatesWriteAndReceiveErrors(t *testing.T) {
	receiveErr := errors.New("receive failed")
	if _, err := Download(context.Background(), io.Discard, func(context.Context, int64) (Chunk, error) { return Chunk{}, receiveErr }); !errors.Is(err, receiveErr) {
		t.Fatalf("receive error = %v", err)
	}
	writeErr := errors.New("write failed")
	if _, err := Download(context.Background(), errorWriter{err: writeErr}, func(context.Context, int64) (Chunk, error) { return Chunk{Data: []byte("x"), Done: true}, nil }); !errors.Is(err, writeErr) {
		t.Fatalf("write error = %v", err)
	}
}

func TestDownloadHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	total, err := Download(ctx, io.Discard, func(context.Context, int64) (Chunk, error) {
		called = true
		return Chunk{Done: true}, nil
	})
	if total != 0 || !errors.Is(err, context.Canceled) || called {
		t.Fatalf("download cancellation = %d, %v, callback called %t", total, err, called)
	}
}

func TestDownloadRejectsShortWrite(t *testing.T) {
	total, err := Download(context.Background(), shortWriter{}, func(context.Context, int64) (Chunk, error) {
		return Chunk{Data: []byte("payload"), Done: true}, nil
	})
	if total != 6 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %d, %v", total, err)
	}
}

func TestDownloadStopsBeforeWritingAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var output bytes.Buffer
	total, err := Download(ctx, &output, func(context.Context, int64) (Chunk, error) {
		cancel()
		return Chunk{Data: []byte("payload"), Done: true}, nil
	})
	if total != 0 || !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatalf("cancelled receive = %d, %v, wrote %d", total, err, output.Len())
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }
