package bootstrap

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	meshrelease "github.com/shaul/mesh/internal/release"
)

const (
	maximumManifestSize = 1 << 20
	maximumArchiveSize  = 128 << 20
	maximumBinarySize   = 128 << 20
)

type binarySelection struct {
	explicitPath string
	artifactDir  string
	baseURL      string
	version      string
	httpClient   *http.Client
}

type resolvedBinary struct {
	path    string
	cleanup func()
}

type releaseOptions struct {
	baseURL    string
	version    string
	httpClient *http.Client
}

func resolvePlatformBinary(ctx context.Context, selection binarySelection, platform Platform) (resolvedBinary, error) {
	if selection.explicitPath != "" {
		if err := checkBinaryPlatform(selection.explicitPath, platform); err != nil {
			return resolvedBinary{}, err
		}
		return resolvedBinary{path: selection.explicitPath, cleanup: func() {}}, nil
	}

	executable, executableErr := os.Executable()
	if executableErr == nil {
		if err := checkBinaryPlatform(executable, platform); err == nil {
			return resolvedBinary{path: executable, cleanup: func() {}}, nil
		}
	}
	artifactDir := selection.artifactDir
	if artifactDir == "" && executableErr == nil {
		artifactDir = filepath.Dir(executable)
	}
	assetName := releaseAssetName(platform)
	if artifactDir != "" {
		rawPath := filepath.Join(artifactDir, strings.TrimSuffix(assetName, ".tar.gz"))
		if _, err := os.Stat(rawPath); err == nil {
			if err := checkBinaryPlatform(rawPath, platform); err != nil {
				return resolvedBinary{}, err
			}
			return resolvedBinary{path: rawPath, cleanup: func() {}}, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return resolvedBinary{}, diagnostic(DiagnosticWrongArch, fmt.Errorf("inspect local release artifact %s: %w", rawPath, err))
		}
		archivePath := filepath.Join(artifactDir, assetName)
		manifestPath := filepath.Join(artifactDir, "checksums.txt")
		if _, err := os.Stat(archivePath); err == nil {
			binaryPath, cleanup, err := extractLocalReleaseBinary(ctx, archivePath, manifestPath, assetName)
			if err != nil {
				return resolvedBinary{}, diagnostic(DiagnosticWrongArch, err)
			}
			if err := checkBinaryPlatform(binaryPath, platform); err != nil {
				cleanup()
				return resolvedBinary{}, err
			}
			return resolvedBinary{path: binaryPath, cleanup: cleanup}, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return resolvedBinary{}, diagnostic(DiagnosticWrongArch, fmt.Errorf("inspect local release archive %s: %w", archivePath, err))
		}
	}

	release, err := normalizeReleaseOptions(selection)
	if err != nil {
		return resolvedBinary{}, diagnostic(DiagnosticWrongArch, fmt.Errorf("select %s/%s Mesh release: %w", platform.OS, platform.Arch, err))
	}
	binaryPath, cleanup, err := fetchReleaseBinary(ctx, platform, release)
	if err != nil {
		return resolvedBinary{}, diagnostic(DiagnosticWrongArch, fmt.Errorf("fetch %s/%s Mesh release: %w", platform.OS, platform.Arch, err))
	}
	if err := checkBinaryPlatform(binaryPath, platform); err != nil {
		cleanup()
		return resolvedBinary{}, err
	}
	return resolvedBinary{path: binaryPath, cleanup: cleanup}, nil
}

func normalizeReleaseOptions(selection binarySelection) (releaseOptions, error) {
	baseURL := strings.TrimRight(selection.baseURL, "/")
	if baseURL == "" {
		baseURL = meshrelease.OfficialBaseURL
	}
	version := selection.version
	if version == "" {
		var err error
		version, err = runningVersion()
		if err != nil {
			return releaseOptions{}, err
		}
	}
	return releaseOptions{baseURL: baseURL, version: version, httpClient: selection.httpClient}, nil
}

func runningVersion() (string, error) {
	if version := meshrelease.Metadata().Version; version != "" {
		return version, nil
	}
	return "", errors.New("automatic release download is unsafe from an unversioned development build; place a matching release artifact beside the executable or run mesh add from a tagged release")
}

func fetchReleaseBinary(ctx context.Context, platform Platform, opts releaseOptions) (string, func(), error) {
	client := meshrelease.Client{BaseURL: opts.baseURL, HTTPClient: opts.httpClient}
	manifest, err := client.Manifest(ctx, opts.version)
	if err != nil {
		return "", func() {}, err
	}
	cache, err := os.MkdirTemp("", "mesh-bootstrap-release-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create release cache: %w", err)
	}
	binaryPath, err := client.Download(ctx, manifest, meshrelease.Platform{OS: platform.OS.String(), Arch: platform.Arch.String()}, cache)
	if err != nil {
		_ = os.RemoveAll(cache)
		return "", func() {}, err
	}
	return binaryPath, func() { _ = os.RemoveAll(cache) }, nil
}

func extractLocalReleaseBinary(ctx context.Context, archivePath, manifestPath, assetName string) (string, func(), error) {
	manifest, err := os.ReadFile(manifestPath) //nolint:gosec // path is the fixed checksum filename beside a caller-selected local release artifact
	if err != nil {
		return "", func() {}, fmt.Errorf("read release checksums %s: %w", manifestPath, err)
	}
	if len(manifest) > maximumManifestSize {
		return "", func() {}, fmt.Errorf("release checksums %s exceed %d bytes", manifestPath, maximumManifestSize)
	}
	wantChecksum, err := checksumForAsset(manifest, assetName)
	if err != nil {
		return "", func() {}, err
	}
	archive, err := os.Open(archivePath) //nolint:gosec // path is a caller-selected local release artifact
	if err != nil {
		return "", func() {}, fmt.Errorf("open release archive %s: %w", archivePath, err)
	}
	checksum := sha256.New()
	written, copyErr := io.Copy(checksum, io.LimitReader(localArchiveReader{ctx: ctx, source: archive}, maximumArchiveSize+1))
	closeErr := archive.Close()
	if copyErr != nil {
		return "", func() {}, fmt.Errorf("hash release archive %s: %w", archivePath, copyErr)
	}
	if closeErr != nil {
		return "", func() {}, fmt.Errorf("close release archive %s: %w", archivePath, closeErr)
	}
	if written > maximumArchiveSize {
		return "", func() {}, fmt.Errorf("release archive %s exceeds %d bytes", archivePath, maximumArchiveSize)
	}
	if got := checksum.Sum(nil); !equalChecksum(got, wantChecksum) {
		return "", func() {}, fmt.Errorf("checksum for %s is %x, want %x", assetName, got, wantChecksum)
	}
	return extractReleaseArchive(ctx, archivePath)
}

func releaseAssetName(platform Platform) string {
	return meshrelease.ArchiveName(meshrelease.Platform{OS: platform.OS.String(), Arch: platform.Arch.String()}, meshrelease.ArchiveGzip)
}

func checksumForAsset(manifest []byte, assetName string) ([]byte, error) {
	var checksum []byte
	scanner := bufio.NewScanner(strings.NewReader(string(manifest)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != assetName {
			continue
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("checksums.txt has an invalid SHA-256 for %s", assetName)
		}
		if checksum != nil {
			return nil, fmt.Errorf("checksums.txt lists %s more than once", assetName)
		}
		checksum = decoded
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read checksums.txt: %w", err)
	}
	if checksum == nil {
		return nil, fmt.Errorf("checksums.txt does not list %s", assetName)
	}
	return checksum, nil
}

func extractReleaseArchive(ctx context.Context, archivePath string) (string, func(), error) {
	archive, err := os.Open(archivePath) //nolint:gosec // caller verified the local archive checksum
	if err != nil {
		return "", func() {}, fmt.Errorf("extract local release archive: %w", err)
	}
	defer archive.Close() //nolint:errcheck // read-only
	binary, err := os.CreateTemp("", "mesh-bootstrap-binary-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("extract local release archive: %w", err)
	}
	path := binary.Name()
	cleanup := func() { _ = os.Remove(path) }
	_, readErr := meshrelease.ReadExecutable(ctx, archive, meshrelease.ArchiveGzip, binary)
	chmodErr := binary.Chmod(0o700)
	syncErr := binary.Sync()
	closeErr := binary.Close()
	if err := errors.Join(readErr, chmodErr, syncErr, closeErr, ctx.Err()); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("extract local release archive: %w", err)
	}
	return path, cleanup, nil
}

type localArchiveReader struct {
	ctx    context.Context
	source io.Reader
}

func (r localArchiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, fmt.Errorf("read local release archive: %w", err)
	}
	n, err := r.source.Read(p[:min(len(p), 32<<10)])
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, fmt.Errorf("read local release archive: %w", ctxErr)
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("read local release archive: %w", err)
	}
	return n, nil
}

func equalChecksum(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var difference byte
	for i := range a {
		difference |= a[i] ^ b[i]
	}
	return difference == 0
}
