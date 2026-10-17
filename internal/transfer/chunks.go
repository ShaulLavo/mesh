// Package transfer provides bounded, context-aware byte-stream transfers.
package transfer

import (
	"context"
	"errors"
	"io"
	"math"
)

const ChunkSize = 256 << 10

var (
	ErrChunkTooLarge  = errors.New("transfer: chunk exceeds limit")
	ErrStalled        = errors.New("transfer: stalled transfer")
	ErrOffsetOverflow = errors.New("transfer: offset overflow")
)

// Chunk is one response from a bounded download source.
type Chunk struct {
	Data []byte
	Done bool
}

// Upload reads reader in bounded chunks and sends them in order. The send
// callback receives the offset of each chunk and the context supplied by the
// caller. The chunk buffer is valid only until send returns.
func Upload(ctx context.Context, reader io.Reader, send func(context.Context, int64, []byte) error) error {
	buffer := make([]byte, ChunkSize)
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := readUploadChunk(ctx, reader, buffer)
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return readErr
		}
		if int64(n) > math.MaxInt64-offset {
			return ErrOffsetOverflow
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := send(ctx, offset, buffer[:n]); err != nil {
			return err
		}
		offset += int64(n)
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			return nil
		}
	}
}

func readUploadChunk(ctx context.Context, reader io.Reader, buffer []byte) (int, error) {
	var read int
	for read < len(buffer) {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		n, err := reader.Read(buffer[read:])
		if n < 0 || n > len(buffer)-read {
			return read, errors.New("transfer: invalid read count")
		}
		read += n
		if read == len(buffer) {
			return read, nil
		}
		if read > 0 && errors.Is(err, io.EOF) {
			return read, io.ErrUnexpectedEOF
		}
		if err != nil {
			return read, err
		}
		if n == 0 {
			return read, ErrStalled
		}
	}
	return read, nil
}

// Download receives bounded chunks in order and writes them to writer. The
// returned count is the number of bytes written before completion or error.
func Download(ctx context.Context, writer io.Writer, receive func(context.Context, int64) (Chunk, error)) (int64, error) {
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return offset, err
		}
		chunk, err := receive(ctx, offset)
		if err != nil {
			return offset, err
		}
		if len(chunk.Data) > ChunkSize {
			return offset, ErrChunkTooLarge
		}
		if len(chunk.Data) == 0 && !chunk.Done {
			return offset, ErrStalled
		}
		if int64(len(chunk.Data)) > math.MaxInt64-offset {
			return offset, ErrOffsetOverflow
		}
		if err := ctx.Err(); err != nil {
			return offset, err
		}
		n, err := writer.Write(chunk.Data)
		if n < 0 || n > len(chunk.Data) {
			return offset, errors.New("transfer: invalid write count")
		}
		offset += int64(n)
		if err != nil {
			return offset, err
		}
		if n != len(chunk.Data) {
			return offset, io.ErrShortWrite
		}
		if chunk.Done {
			return offset, nil
		}
	}
}
