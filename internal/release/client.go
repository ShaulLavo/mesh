package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	OfficialBaseURL    = "https://github.com/ShaulLavo/mesh/releases"
	ManifestAssetName  = "mesh-release.json"
	maximumManifest    = 1 << 20
	maximumArchive     = 128 << 20
	maximumBinary      = 128 << 20
	maximumErrorDetail = 4 << 10
)

// Client reads immutable release metadata and artifacts from a release origin.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// Manifest resolves selector once and validates the complete descriptor.
func (c Client) Manifest(ctx context.Context, selector string) (Manifest, error) {
	baseURL, client, err := c.normalized()
	if err != nil {
		return Manifest{}, err
	}
	releaseURL, err := releaseURL(baseURL, selector)
	if err != nil {
		return Manifest{}, err
	}
	contents, err := downloadBytes(ctx, client, releaseURL+"/"+ManifestAssetName, maximumManifest)
	if err != nil {
		return Manifest{}, fmt.Errorf("release: download manifest: %w", err)
	}
	manifest, err := decodeManifest(contents)
	if err != nil {
		return Manifest{}, err
	}
	if selector != "latest" && manifest.Version != selector {
		return Manifest{}, fmt.Errorf("release: manifest version %q does not match requested %q", manifest.Version, selector)
	}
	return manifest, nil
}

// Download stores a verified executable in a content-addressed cache directory.
func (c Client) Download(ctx context.Context, manifest Manifest, platform Platform, cacheDirectory string) (string, error) {
	artifact, err := manifest.Artifact(platform)
	if err != nil {
		return "", err
	}
	baseURL, client, err := c.normalized()
	if err != nil {
		return "", err
	}
	root, err := cacheRoot(cacheDirectory, manifest, platform)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("release: create cache directory %s: %w", root, err)
	}
	destination := filepath.Join(root, "mesh")
	if cachedExecutable(destination, artifact.BinarySHA256) {
		return destination, nil
	}
	archive, err := os.CreateTemp(root, ".archive-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("release: create cached archive: %w", err)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath) //nolint:errcheck // best-effort cleanup of unpublished cache data
	address := baseURL + "/download/" + url.PathEscape(manifest.Version) + "/" + url.PathEscape(artifact.Archive)
	if err := downloadTo(ctx, client, address, archive, artifact.SHA256, maximumArchive); err != nil {
		_ = archive.Close()
		return "", err
	}
	if err := archive.Close(); err != nil {
		return "", fmt.Errorf("release: close archive: %w", err)
	}
	temporary, err := extractBinary(archivePath, root, artifact.BinarySHA256)
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary) //nolint:errcheck // best-effort cleanup after atomic publication
	if err := os.Rename(temporary, destination); err != nil {
		return "", fmt.Errorf("release: publish cached executable: %w", err)
	}
	if err := syncDirectory(root); err != nil {
		return "", err
	}
	return destination, nil
}

func (c Client) normalized() (string, *http.Client, error) {
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		baseURL = OfficialBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", nil, fmt.Errorf("release: base URL %q must be HTTPS without credentials, query, or fragment", baseURL)
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return baseURL, client, nil
}

func releaseURL(baseURL, selector string) (string, error) {
	if selector == "latest" {
		return baseURL + "/latest/download", nil
	}
	if _, err := parseVersion(selector); err != nil {
		return "", err
	}
	return baseURL + "/download/" + url.PathEscape(selector), nil
}

func decodeManifest(contents []byte) (Manifest, error) {
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("release: decode manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Manifest{}, fmt.Errorf("release: decode manifest trailing data: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func cacheRoot(directory string, manifest Manifest, platform Platform) (string, error) {
	if directory == "" {
		return "", errors.New("release: cache directory is empty")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("release: resolve cache directory %s: %w", directory, err)
	}
	return filepath.Join(abs, manifest.Digest(), platform.OS+"_"+platform.Arch), nil
}

func cachedExecutable(path, wantDigest string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return false
	}
	digest, err := fileDigest(path, maximumBinary)
	return err == nil && digest == wantDigest
}

func downloadBytes(ctx context.Context, client *http.Client, address string, maximum int64) ([]byte, error) {
	response, err := response(ctx, client, address)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close() //nolint:errcheck // read result is authoritative
	contents, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", address, err)
	}
	if int64(len(contents)) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", address, maximum)
	}
	return contents, nil
}

func downloadTo(ctx context.Context, client *http.Client, address string, destination io.Writer, wantDigest string, maximum int64) error {
	response, err := response(ctx, client, address)
	if err != nil {
		return err
	}
	defer response.Body.Close() //nolint:errcheck // copy result is authoritative
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(response.Body, maximum+1))
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

func response(ctx context.Context, client *http.Client, address string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("release: create request: %w", err)
	}
	request.Header.Set("User-Agent", "mesh-release")
	reply, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if reply.Request == nil || reply.Request.URL.Scheme != "https" {
		_ = reply.Body.Close()
		return nil, errors.New("release: request ended at a non-HTTPS URL")
	}
	if reply.StatusCode == http.StatusOK {
		return reply, nil
	}
	detail, _ := io.ReadAll(io.LimitReader(reply.Body, maximumErrorDetail))
	_ = reply.Body.Close()
	return nil, fmt.Errorf("release: GET %s: %s: %s", address, reply.Status, strings.TrimSpace(string(detail)))
}

func extractBinary(archivePath, directory, wantDigest string) (string, error) {
	archive, err := os.Open(archivePath) //nolint:gosec // path is a freshly downloaded, verified archive
	if err != nil {
		return "", err
	}
	defer archive.Close() //nolint:errcheck // extraction result is authoritative
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return "", fmt.Errorf("release: open gzip archive: %w", err)
	}
	defer gzipReader.Close() //nolint:errcheck // reader has no pending writes
	tarReader := tar.NewReader(gzipReader)
	header, err := tarReader.Next()
	if err != nil {
		return "", fmt.Errorf("release: read archive: %w", err)
	}
	if header.Name != "mesh" || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > maximumBinary {
		return "", fmt.Errorf("release: archive must contain one bounded regular file named mesh")
	}
	temporary, err := os.CreateTemp(directory, ".mesh-*")
	if err != nil {
		return "", fmt.Errorf("release: create cached executable: %w", err)
	}
	path := temporary.Name()
	if err := writeBinary(temporary, tarReader, header.Size, wantDigest); err != nil {
		_ = temporary.Close()
		_ = os.Remove(path)
		return "", err
	}
	if _, err := tarReader.Next(); !errors.Is(err, io.EOF) {
		_ = os.Remove(path)
		return "", errors.New("release: archive contains more than one member")
	}
	return path, nil
}

func writeBinary(destination *os.File, source io.Reader, size int64, wantDigest string) error {
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(destination, hash), io.LimitReader(source, maximumBinary+1))
	chmodErr := destination.Chmod(0o700)
	syncErr := destination.Sync()
	closeErr := destination.Close()
	if copyErr != nil || chmodErr != nil || syncErr != nil || closeErr != nil || written != size {
		return errors.Join(copyErr, chmodErr, syncErr, closeErr, fmt.Errorf("release: archive binary size is %d, want %d", written, size))
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if digest != wantDigest {
		return fmt.Errorf("release: binary SHA-256 is %s, want %s", digest, wantDigest)
	}
	return nil
}

func fileDigest(path string, maximum int64) (string, error) {
	file, err := os.Open(path) //nolint:gosec // caller provides a fixed cache or executable path
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck // digest result is authoritative
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil {
		return "", err
	}
	if written > maximum {
		return "", fmt.Errorf("release: file exceeds %d bytes", maximum)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // path is the package-owned cache directory
	if err != nil {
		return fmt.Errorf("release: open cache directory: %w", err)
	}
	defer directory.Close() //nolint:errcheck // sync result is authoritative
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("release: sync cache directory: %w", err)
	}
	return nil
}
