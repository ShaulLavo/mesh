// Package releasedocuments owns the authenticated release document container.
package releasedocuments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/shaul/mesh/internal/release"
)

const (
	ArchiveName   = "mesh-documents.tar.gz"
	EnvelopeName  = "mesh-update.sig.json"
	MaxCompressed = 32 << 20
	MaxExpanded   = 128 << 20
	MaxMembers    = 32768
	maxCatalog    = 8 << 20
	catalogName   = "bundle.json"
)

type Binding struct {
	Version   string             `json:"version"`
	Source    string             `json:"source"`
	ReleaseID int64              `json:"releaseId"`
	WriterRun int64              `json:"writerRun"`
	Platforms []release.Platform `json:"platforms"`
}

type Document struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Inventory struct {
	Schema    int        `json:"schema"`
	Binding   Binding    `json:"binding"`
	Documents []Document `json:"documents"`
}

type Packed struct {
	Inventory
	Archive Document `json:"archive"`
}

type catalog struct {
	Schema    int        `json:"schema"`
	Binding   Binding    `json:"binding"`
	Documents []Document `json:"documents"`
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, documentError(err)
	}
	return r.reader.Read(data) //nolint:wrapcheck // io.Reader requires the original EOF sentinel.
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func validateBinding(binding Binding) error {
	if _, err := release.CompareVersions(binding.Version, binding.Version); err != nil {
		return documentError(err)
	}
	if len(binding.Source) != 40 || strings.Trim(binding.Source, "0123456789abcdef") != "" || binding.ReleaseID <= 0 || binding.WriterRun <= 0 {
		return errors.New("documents: invalid source, release or writer run")
	}
	want := []release.Platform{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}
	if !slices.Equal(binding.Platforms, want) {
		return errors.New("documents: exact sorted platform set required")
	}
	return nil
}

func validName(name string) bool {
	return name != catalogName && name != EnvelopeName && strings.HasSuffix(name, ".json") &&
		(release.AssetRef{Tag: "v0.1.1", Name: name, SHA256: strings.Repeat("0", 64)}).Validate() == nil
}

func documentLimit(name string) int64 {
	switch name {
	case release.ManifestAssetName:
		return 512 << 10
	case release.UpdateMetadataAssetName:
		return release.MaxMetadataBytes
	case release.DiscoveryRootAssetName:
		return release.MaxDiscoveryRootBytes
	case EnvelopeName:
		return release.MaxSignatureBytes
	case "mesh-retention.json":
		return 16 << 20
	case "mesh-workers.json":
		return 8 << 20
	default:
		if strings.HasPrefix(name, "mesh-anchors-") || strings.HasSuffix(name, ".anchors.json") {
			return release.MaxAnchorPageBytes
		}
		return 1 << 20
	}
}

func readFile(ctx context.Context, root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, documentError(err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, documentError(err)
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("documents: input is not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, file}, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("documents: input grew beyond byte bound")
	}
	return data, documentError(err)
}

func writeFile(ctx context.Context, root *os.Root, name string, contents []byte) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return documentError(err)
	}
	_, writeErr := io.Copy(file, contextReader{ctx, bytes.NewReader(contents)})
	return errors.Join(writeErr, file.Close())
}

func validateIdentity(ctx context.Context, root *os.Root, binding Binding) error {
	manifest, err := readFile(ctx, root, release.ManifestAssetName, documentLimit(release.ManifestAssetName))
	if err != nil {
		return documentError(err)
	}
	metadata, err := readFile(ctx, root, release.UpdateMetadataAssetName, release.MaxMetadataBytes)
	if err != nil {
		return documentError(err)
	}
	var frozen release.Manifest
	var update release.UpdateMetadata
	if err := json.Unmarshal(manifest, &frozen); err != nil {
		return documentError(err)
	}
	if err := json.Unmarshal(metadata, &update); err != nil {
		return documentError(err)
	}
	if frozen.Version != binding.Version || frozen.Commit != binding.Source || update.Version != binding.Version || update.Manifest != digest(manifest) {
		return errors.New("documents: catalog differs from exact manifest or metadata identity")
	}
	platforms := make([]release.Platform, 0, len(frozen.Artifacts))
	for _, artifact := range frozen.Artifacts {
		platforms = append(platforms, artifact.Platform)
	}
	slices.SortFunc(platforms, func(a, b release.Platform) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(platforms, binding.Platforms) {
		return errors.New("documents: manifest platforms differ from catalog")
	}
	rootBytes, err := readFile(ctx, root, release.DiscoveryRootAssetName, release.MaxDiscoveryRootBytes)
	if err != nil {
		return documentError(err)
	}
	var hint release.DiscoveryRoot
	if err := json.Unmarshal(rootBytes, &hint); err != nil {
		return documentError(err)
	}
	if hint.Head.Tag != binding.Version || hint.Head.Name != release.UpdateMetadataAssetName || hint.Head.SHA256 != digest(metadata) {
		return errors.New("documents: root differs from bundled metadata")
	}
	return nil
}

// DecodeJSON rejects ambiguous object keys, unknown fields and trailing values.
func DecodeJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueValue(decoder, 0); err != nil {
		return documentError(err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("documents: trailing JSON")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return documentError(decoder.Decode(destination))
}

func uniqueValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("documents: JSON nesting exceeds bound")
	}
	token, err := decoder.Token()
	if err != nil {
		return documentError(err)
	}
	if token == json.Delim('{') {
		return uniqueObject(decoder, depth+1)
	}
	if token != json.Delim('[') {
		return nil
	}
	for decoder.More() {
		if err := uniqueValue(decoder, depth+1); err != nil {
			return documentError(err)
		}
	}
	_, err = decoder.Token()
	return documentError(err)
}

func uniqueObject(decoder *json.Decoder, depth int) error {
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return documentError(err)
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return errors.New("documents: repeated JSON key")
		}
		seen[name] = true
		if err := uniqueValue(decoder, depth); err != nil {
			return documentError(err)
		}
	}
	_, err := decoder.Token()
	return documentError(err)
}

func sameBinding(a, b Binding) bool {
	return a.Version == b.Version && a.Source == b.Source && a.ReleaseID == b.ReleaseID && a.WriterRun == b.WriterRun && slices.Equal(a.Platforms, b.Platforms)
}

func checkDocument(document Document) error {
	if !validName(document.Name) || document.Size <= 0 || document.Size > documentLimit(document.Name) || len(document.SHA256) != 64 || strings.Trim(document.SHA256, "0123456789abcdef") != "" {
		return fmt.Errorf("documents: invalid document %q", document.Name)
	}
	return nil
}

func documentError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("release documents: %w", err)
}
