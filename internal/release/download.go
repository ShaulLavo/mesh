package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
)

func downloadBytes(ctx context.Context, client *http.Client, address string, maximum int64) ([]byte, error) {
	var contents []byte
	err := retryDownload(ctx, metadataAttemptTimeout, func(ctx context.Context) error {
		var err error
		contents, err = readDownload(ctx, client, address, maximum)
		return err
	})
	return contents, err
}

func readDownload(ctx context.Context, client *http.Client, address string, maximum int64) ([]byte, error) {
	response, err := response(ctx, client, address)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close() //nolint:errcheck // read result is authoritative
	contents, err := io.ReadAll(io.LimitReader(downloadReader{response.Body}, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", address, err)
	}
	if int64(len(contents)) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", address, maximum)
	}
	return contents, nil
}

func downloadTo(ctx context.Context, client *http.Client, address string, destination *os.File, wantDigest string, maximum int64) error {
	return retryDownload(ctx, downloadAttemptTimeout, func(ctx context.Context) error {
		// A failed read may have written a prefix; every attempt verifies one complete archive.
		if err := destination.Truncate(0); err != nil {
			return fmt.Errorf("release: reset archive: %w", err)
		}
		if _, err := destination.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("release: rewind archive: %w", err)
		}
		return writeDownload(ctx, client, address, destination, wantDigest, maximum)
	})
}

func writeDownload(ctx context.Context, client *http.Client, address string, destination io.Writer, wantDigest string, maximum int64) error {
	response, err := response(ctx, client, address)
	if err != nil {
		return err
	}
	defer response.Body.Close() //nolint:errcheck // copy result is authoritative
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(downloadReader{response.Body}, maximum+1))
	if err != nil {
		return fmt.Errorf("release: download %s: %w", address, err)
	}
	if written > maximum {
		return fmt.Errorf("release: %s exceeds %d bytes", address, maximum)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if digest != wantDigest {
		return fmt.Errorf("release: archive SHA-256 is %s, want %s", digest, wantDigest)
	}
	return nil
}

// Body transport failures are retryable; destination writes and digest failures stay terminal.
type downloadReadError struct{ err error }

func (e *downloadReadError) Error() string { return e.err.Error() }
func (e *downloadReadError) Unwrap() error { return e.err }

type downloadReader struct{ io.Reader }

func (r downloadReader) Read(contents []byte) (int, error) {
	n, err := r.Reader.Read(contents)
	if err == nil {
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	return n, &downloadReadError{err: err}
}
