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
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"
)

const (
	officialReleaseBaseURL = "https://github.com/shaul/mesh/releases"
	maximumManifestSize    = 1 << 20
	maximumArchiveSize     = 128 << 20
	maximumBinarySize      = 128 << 20
)

var releaseVersion string

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

	release := normalizeReleaseOptions(selection)
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

func normalizeReleaseOptions(selection binarySelection) releaseOptions {
	baseURL := strings.TrimRight(selection.baseURL, "/")
	if baseURL == "" {
		baseURL = officialReleaseBaseURL
	}
	version := selection.version
	if version == "" {
		version = runningVersion()
	}
	client := selection.httpClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return releaseOptions{baseURL: baseURL, version: version, httpClient: client}
}

func runningVersion() string {
	if releaseVersion != "" {
		return releaseVersion
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "latest"
	}
	return info.Main.Version
}

func fetchReleaseBinary(ctx context.Context, platform Platform, opts releaseOptions) (string, func(), error) {
	assetName := releaseAssetName(platform)
	releaseURL, err := releaseDownloadURL(opts.baseURL, opts.version)
	if err != nil {
		return "", func() {}, err
	}
	manifest, err := downloadBytes(ctx, opts.httpClient, releaseURL+"/checksums.txt", maximumManifestSize)
	if err != nil {
		return "", func() {}, fmt.Errorf("download checksums.txt: %w", err)
	}
	wantChecksum, err := checksumForAsset(manifest, assetName)
	if err != nil {
		return "", func() {}, err
	}
	archive, err := os.CreateTemp("", "mesh-release-*.tar.gz")
	if err != nil {
		return "", func() {}, fmt.Errorf("create release archive: %w", err)
	}
	archivePath := archive.Name()
	cleanupArchive := func() { _ = os.Remove(archivePath) }
	checksum := sha256.New()
	if err := downloadFile(ctx, opts.httpClient, releaseURL+"/"+assetName, archive, checksum, maximumArchiveSize); err != nil {
		_ = archive.Close()
		cleanupArchive()
		return "", func() {}, fmt.Errorf("download %s: %w", assetName, err)
	}
	if err := archive.Close(); err != nil {
		cleanupArchive()
		return "", func() {}, fmt.Errorf("close release archive: %w", err)
	}
	gotChecksum := checksum.Sum(nil)
	if !equalChecksum(gotChecksum, wantChecksum) {
		cleanupArchive()
		return "", func() {}, fmt.Errorf("checksum for %s is %x, want %x", assetName, gotChecksum, wantChecksum)
	}
	binaryPath, cleanupBinary, err := extractReleaseArchive(archivePath)
	cleanupArchive()
	if err != nil {
		return "", func() {}, fmt.Errorf("extract %s: %w", assetName, err)
	}
	return binaryPath, cleanupBinary, nil
}

func extractLocalReleaseBinary(archivePath, manifestPath, assetName string) (string, func(), error) {
	manifest, err := os.ReadFile(manifestPath)
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
	archive, err := os.Open(archivePath)
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
	if written > maximumArchiveSize {
		return "", func() {}, fmt.Errorf("release archive %s exceeds %d bytes", archivePath, maximumArchiveSize)
	}
	if got := checksum.Sum(nil); !equalChecksum(got, wantChecksum) {
		return "", func() {}, fmt.Errorf("checksum for %s is %x, want %x", assetName, got, wantChecksum)
	}
	return extractReleaseArchive(archivePath)
}

func releaseDownloadURL(baseURL, version string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("release base URL %q must be an HTTPS URL without credentials, a query, or a fragment", baseURL)
	}
	if version == "latest" {
		return strings.TrimRight(baseURL, "/") + "/latest/download", nil
	}
	if version == "" || strings.ContainsAny(version, "/\\?#") {
		return "", fmt.Errorf("release version %q is invalid", version)
	}
	return strings.TrimRight(baseURL, "/") + "/download/" + url.PathEscape(version), nil
}

func releaseAssetName(platform Platform) string {
	return fmt.Sprintf("mesh_%s_%s.tar.gz", platform.OS, platform.Arch)
}

func downloadBytes(ctx context.Context, client *http.Client, address string, maximum int64) ([]byte, error) {
	response, err := releaseResponse(ctx, client, address)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", address, err)
	}
	if int64(len(contents)) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", address, maximum)
	}
	return contents, nil
}

func downloadFile(ctx context.Context, client *http.Client, address string, destination io.Writer, checksum hash.Hash, maximum int64) error {
	response, err := releaseResponse(ctx, client, address)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	written, err := io.Copy(io.MultiWriter(destination, checksum), io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", address, err)
	}
	if written > maximum {
		return fmt.Errorf("%s exceeds %d bytes", address, maximum)
	}
	return nil
}

func releaseResponse(ctx context.Context, client *http.Client, address string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("create release request: %w", err)
	}
	request.Header.Set("User-Agent", "mesh-bootstrap")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.Request == nil || response.Request.URL.Scheme != "https" {
		response.Body.Close()
		return nil, fmt.Errorf("release request ended at a non-HTTPS URL")
	}
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		return nil, fmt.Errorf("GET %s: %s: %s", address, response.Status, strings.TrimSpace(string(detail)))
	}
	return response, nil
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
	archive, err := os.Open(archivePath)
	if err != nil {
		return "", func() {}, err
	}
	defer archive.Close()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return "", func() {}, err
	}
	defer gzipReader.Close()
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
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
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
