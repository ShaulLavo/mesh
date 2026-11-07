package release

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/andybalholm/brotli"
)

// ArchiveFormat is the compression of a release tar archive.
type ArchiveFormat string

const (
	ArchiveGzip   ArchiveFormat = "tar.gz"
	ArchiveBrotli ArchiveFormat = "tar.br"
	// PublishedArchive remains gzip until the producer cutover.
	PublishedArchive = ArchiveGzip
	archiveChunk     = 32 << 10
)

// ArchiveName names the fixed archive for a platform and compression format.
func ArchiveName(platform Platform, format ArchiveFormat) string {
	return "mesh_" + platform.OS + "_" + platform.Arch + "." + string(format)
}

// Format validates the exact platform-specific archive name.
func (a Artifact) Format() (ArchiveFormat, error) {
	if !supportedPlatform(a.Platform) {
		return "", fmt.Errorf("release: unsupported artifact platform %s", a.Platform)
	}
	for _, format := range []ArchiveFormat{ArchiveGzip, ArchiveBrotli} {
		if a.Archive == ArchiveName(a.Platform, format) {
			return format, nil
		}
	}
	return "", fmt.Errorf("release: artifact for %s has unsupported archive name %q", a.Platform, a.Archive)
}

// Executable describes the single member of a verified release archive.
type Executable struct {
	Size   int64
	SHA256 string
}

// ReadExecutable validates the complete stream and copies its sole regular mesh
// member. The caller must verify the compressed archive's digest before decoding.
func ReadExecutable(ctx context.Context, archive io.Reader, format ArchiveFormat, destination io.Writer) (Executable, error) {
	if err := ctx.Err(); err != nil {
		return Executable{}, fmt.Errorf("release: read executable: %w", err)
	}
	compressed := &archiveLimitReader{source: archiveContextReader{ctx: ctx, source: archive}, remaining: maximumArchive}
	decoded, err := decompressor(format, bufio.NewReaderSize(compressed, 64<<10))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Executable{}, fmt.Errorf("release: construct archive decoder: %w", ctxErr)
	}
	if err != nil {
		return Executable{}, fmt.Errorf("release: open archive: %w", err)
	}
	expanded := &archiveLimitReader{source: archiveContextReader{ctx: ctx, source: decoded}, remaining: maximumExpandedArchive}
	return readArchiveExecutable(ctx, expanded, destination)
}

func readArchiveExecutable(ctx context.Context, expanded io.Reader, destination io.Writer) (Executable, error) {
	member := tar.NewReader(expanded)
	header, err := member.Next()
	if err != nil {
		return Executable{}, fmt.Errorf("release: read archive: %w", err)
	}
	if header.Name != "mesh" || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > maximumBinary {
		return Executable{}, errors.New("release: archive must contain one bounded regular file named mesh")
	}
	hash := sha256.New()
	written, err := io.CopyBuffer(archiveContextWriter{ctx: ctx, destination: io.MultiWriter(destination, hash)}, archiveContextReader{ctx: ctx, source: member}, make([]byte, archiveChunk))
	if err != nil {
		return Executable{}, fmt.Errorf("release: copy archive binary: %w", err)
	}
	if written != header.Size {
		return Executable{}, fmt.Errorf("release: archive binary size is %d, want %d", written, header.Size)
	}
	if _, err := member.Next(); !errors.Is(err, io.EOF) {
		return Executable{}, errors.Join(err, errors.New("release: archive contains more than one member or invalid trailing data"))
	}
	if err := validateArchiveTail(expanded); err != nil {
		return Executable{}, fmt.Errorf("release: read executable: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Executable{}, fmt.Errorf("release: read executable: %w", err)
	}
	return Executable{Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func decompressor(format ArchiveFormat, source io.Reader) (io.Reader, error) {
	switch format {
	case ArchiveGzip:
		reader, err := gzip.NewReader(source)
		if err != nil {
			return nil, fmt.Errorf("release: open gzip archive: %w", err)
		}
		return reader, nil
	case ArchiveBrotli:
		// The decoder rejects large-window headers before allocating its ring buffer.
		// Its format window is at most 16 MiB, not a bound on all decoder allocations.
		return brotli.NewReader(source), nil
	default:
		return nil, fmt.Errorf("release: unsupported archive format %q", format)
	}
}

type archiveContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r archiveContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, fmt.Errorf("release: archive operation canceled: %w", err)
	}
	n, err := r.source.Read(p[:min(len(p), archiveChunk)])
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, fmt.Errorf("release: archive operation canceled: %w", ctxErr)
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("release: archive I/O: %w", err)
	}
	return n, nil
}

type archiveContextWriter struct {
	ctx         context.Context
	destination io.Writer
}

func (w archiveContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, fmt.Errorf("release: archive operation canceled: %w", err)
	}
	n, err := w.destination.Write(p)
	if ctxErr := w.ctx.Err(); ctxErr != nil {
		return n, fmt.Errorf("release: archive operation canceled: %w", ctxErr)
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("release: archive I/O: %w", err)
	}
	return n, nil
}

// Exhaustion probes one byte beyond the budget. Only the source can report EOF.
type archiveLimitReader struct {
	source    io.Reader
	remaining int64
}

func (r *archiveLimitReader) Read(p []byte) (int, error) {
	if r.remaining < 0 {
		return 0, errors.New("release: archive exceeds size limit")
	}
	n, err := r.source.Read(p[:min(int64(len(p)), r.remaining+1, archiveChunk)])
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return n, errors.New("release: archive exceeds size limit")
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("release: read bounded archive: %w", err)
	}
	return n, nil
}

// Tar EOF can hide a terminal decoder error. Validate padding through actual EOF.
func validateArchiveTail(source io.Reader) error {
	tail := &archiveLimitReader{source: source, remaining: maximumArchiveTrailer}
	var buffer [archiveChunk]byte
	for {
		n, err := tail.Read(buffer[:])
		if bytes.Count(buffer[:n], []byte{0}) != n {
			return errors.New("release: archive contains nonzero trailing data")
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("release: read archive trailer: %w", err)
		}
	}
}

func extractBinary(ctx context.Context, archivePath string, format ArchiveFormat, directory, wantDigest string) (string, error) {
	archive, err := os.Open(archivePath) //nolint:gosec // freshly downloaded and digest-verified archive
	if err != nil {
		return "", fmt.Errorf("release: extract executable: %w", err)
	}
	defer archive.Close() //nolint:errcheck // read-only
	temporary, err := os.CreateTemp(directory, ".mesh-*")
	if err != nil {
		return "", fmt.Errorf("release: create cached executable: %w", err)
	}
	path := temporary.Name()
	success := false
	defer func() {
		_ = temporary.Close()
		if !success {
			_ = os.Remove(path)
		}
	}()
	executable, err := ReadExecutable(ctx, archive, format, temporary)
	if err != nil {
		return "", fmt.Errorf("release: extract executable: %w", err)
	}
	if executable.SHA256 != wantDigest {
		return "", fmt.Errorf("release: binary SHA-256 is %s, want %s", executable.SHA256, wantDigest)
	}
	if err := temporary.Chmod(0o700); err != nil {
		return "", fmt.Errorf("release: extract executable: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("release: extract executable: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("release: extract executable: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("release: extract executable: %w", err)
	}
	success = true
	return path, nil
}
