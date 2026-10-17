// Package transfer provides bounded, context-aware byte-stream transfers.
package transfer

import (
	"context"
	"errors"
	"fmt"
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
		n, readErr := readUploadChunk(ctx, reader, buffer)
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return readErr
		}
		if err := sendUploadChunk(ctx, send, offset, buffer[:n]); err != nil {
			return err
		}
		offset += int64(n)
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			return nil
		}
	}
}

func sendUploadChunk(ctx context.Context, send func(context.Context, int64, []byte) error, offset int64, data []byte) error {
	if int64(len(data)) > math.MaxInt64-offset {
		return ErrOffsetOverflow
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("transfer: send upload chunk: %w", err)
	}
	if err := send(ctx, offset, data); err != nil {
		return fmt.Errorf("transfer: send upload chunk at %d: %w", offset, err)
	}
	return nil
}

func readUploadChunk(ctx context.Context, reader io.Reader, buffer []byte) (int, error) {
	var read int
	for read < len(buffer) {
		if err := ctx.Err(); err != nil {
			return read, fmt.Errorf("transfer: read upload chunk: %w", err)
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
			return read, fmt.Errorf("transfer: read upload chunk: %w", err)
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
			return offset, fmt.Errorf("transfer: receive download chunk: %w", err)
		}
		chunk, err := receive(ctx, offset)
		if err != nil {
			return offset, fmt.Errorf("transfer: receive download chunk at %d: %w", offset, err)
		}
		if err := validateDownloadChunk(chunk, offset); err != nil {
			return offset, err
		}
		offset, err = writeDownloadChunk(ctx, writer, offset, chunk.Data)
		if err != nil {
			return offset, err
		}
		if chunk.Done {
			return offset, nil
		}
	}
}

func validateDownloadChunk(chunk Chunk, offset int64) error {
	if len(chunk.Data) > ChunkSize {
		return ErrChunkTooLarge
	}
	if len(chunk.Data) == 0 && !chunk.Done {
		return ErrStalled
	}
	if int64(len(chunk.Data)) > math.MaxInt64-offset {
		return ErrOffsetOverflow
	}
	return nil
}

func writeDownloadChunk(ctx context.Context, writer io.Writer, offset int64, data []byte) (int64, error) {
	if err := ctx.Err(); err != nil {
		return offset, fmt.Errorf("transfer: write download chunk: %w", err)
	}
	n, err := writer.Write(data)
	if n < 0 || n > len(data) {
		return offset, errors.New("transfer: invalid write count")
	}
	offset += int64(n)
	if err != nil {
		return offset, fmt.Errorf("transfer: write download chunk: %w", err)
	}
	if n != len(data) {
		return offset, io.ErrShortWrite
	}
	return offset, nil
}
