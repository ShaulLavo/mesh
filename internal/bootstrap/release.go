package bootstrap

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
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
			binaryPath, cleanup, err := extractLocalReleaseBinary(archivePath, manifestPath, assetName)
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
	if version := meshrelease.Current().Version; version != "" {
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

func extractLocalReleaseBinary(archivePath, manifestPath, assetName string) (string, func(), error) {
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
	written, copyErr := io.Copy(checksum, io.LimitReader(archive, maximumArchiveSize+1))
	closeErr := archive.Close()
	if copyErr != nil {
		return "", func() {}, fmt.Errorf("hash release archive %s: %w", archivePath, copyErr)
	}
	if closeErr != nil {
		return "", func() {}, fmt.Errorf("close release archive %s: %w", archivePath, closeErr)
	}
	if written > maximumBinarySize {
		return "", func() {}, fmt.Errorf("release archive %s exceeds %d bytes", archivePath, maximumBinarySize)
	}
	if got := checksum.Sum(nil); !equalChecksum(got, wantChecksum) {
		return "", func() {}, fmt.Errorf("checksum for %s is %x, want %x", assetName, got, wantChecksum)
	}
	return extractReleaseArchive(archivePath)
}

func releaseAssetName(platform Platform) string {
	return fmt.Sprintf("mesh_%s_%s.tar.gz", platform.OS, platform.Arch)
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

func extractReleaseArchive(archivePath string) (string, func(), error) {
	archive, err := os.Open(archivePath) //nolint:gosec // path is a verified local or freshly downloaded release archive
	if err != nil {
		return "", func() {}, err
	}
	defer archive.Close() //nolint:errcheck // extraction result takes precedence over read-only cleanup
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return "", func() {}, err
	}
	defer gzipReader.Close() //nolint:errcheck // the gzip reader has no pending writes to flush
	tarReader := tar.NewReader(gzipReader)

	var binaryPath string
	cleanup := func() {
		if binaryPath != "" {
			_ = os.Remove(binaryPath)
		}
	}
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return "", func() {}, err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			cleanup()
			return "", func() {}, fmt.Errorf("archive member %q is not a regular file", header.Name)
		}
		if filepath.Base(filepath.Clean(header.Name)) != "mesh" || binaryPath != "" {
			cleanup()
			return "", func() {}, fmt.Errorf("archive must contain one regular file named mesh, found %q", header.Name)
		}
		if header.Size < 0 || header.Size > maximumBinarySize {
			return "", func() {}, fmt.Errorf("archive binary size %d exceeds %d bytes", header.Size, maximumBinarySize)
		}
		binary, err := os.CreateTemp("", "mesh-bootstrap-binary-*")
		if err != nil {
			return "", func() {}, err
		}
		binaryPath = binary.Name()
		written, copyErr := io.Copy(binary, io.LimitReader(tarReader, maximumBinarySize+1))
		chmodErr := binary.Chmod(0o700)
		closeErr := binary.Close()
		if copyErr != nil || chmodErr != nil || closeErr != nil || written != header.Size {
			cleanup()
			return "", func() {}, errors.Join(copyErr, chmodErr, closeErr, fmt.Errorf("archive binary size is %d, want %d", written, header.Size))
		}
	}
	if binaryPath == "" {
		return "", func() {}, errors.New("archive contains no mesh binary")
	}
	return binaryPath, cleanup, nil
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
