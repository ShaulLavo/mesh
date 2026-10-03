package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ErrExecutableChecksumMismatch identifies image integrity failure independently of presentation.
var ErrExecutableChecksumMismatch = errors.New("executable checksum mismatch")

// VerifyExecutable checks an installed image without blocking on special files.
func VerifyExecutable(ctx context.Context, path, digest string) error {
	return CopyExecutable(ctx, path, digest, io.Discard)
}

// CopyExecutable writes verified, bounded image bytes to a caller-owned staging file.
// The caller publishes that file only after this function succeeds.
func CopyExecutable(ctx context.Context, path, digest string, destination io.Writer) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("verify executable: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // caller supplies its installed executable path
	if err != nil {
		return fmt.Errorf("open executable: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maximumBinary {
		return fmt.Errorf("executable must be a regular file within %d bytes", maximumBinary)
	}
	hash := sha256.New()
	stop := context.AfterFunc(ctx, func() { _ = file.Close() })
	defer stop()
	written, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(executableReader{ctx: ctx, file: file}, maximumBinary+1))
	if err != nil {
		return fmt.Errorf("read executable: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("verify executable: %w", err)
	}
	if written != info.Size() || hex.EncodeToString(hash.Sum(nil)) != digest {
		return ErrExecutableChecksumMismatch
	}
	return nil
}

type executableReader struct {
	ctx  context.Context
	file *os.File
}

func (r executableReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, fmt.Errorf("read executable: %w", err)
	}
	n, err := r.file.Read(data)
	if err == io.EOF {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("read executable: %w", err)
	}
	return n, nil
}
