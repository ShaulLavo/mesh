package releasedocuments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/shaul/mesh/internal/release"
)

type physicalAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	State  string `json:"state"`
}
type snapshot struct {
	Repository string          `json:"repository"`
	Run        int64           `json:"intake_run"`
	ID         int64           `json:"id"`
	Version    string          `json:"tag_name"`
	Source     string          `json:"target_commitish"`
	Draft      bool            `json:"draft"`
	Assets     []physicalAsset `json:"assets"`
}

func Materialize(ctx context.Context, intake string, expected Binding, trust release.TrustRoot, destination string) (Inventory, error) {
	var result Inventory
	if err := validateBinding(expected); err != nil {
		return result, documentError(err)
	}
	input, err := os.OpenRoot(intake)
	if err != nil {
		return result, documentError(err)
	}
	defer func() { _ = input.Close() }()
	value, err := verifySnapshot(ctx, input, expected)
	if err != nil {
		return result, documentError(err)
	}
	envelope, err := readFile(ctx, input, EnvelopeName, release.MaxSignatureBytes)
	if err != nil {
		return result, documentError(err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("documents: destination must not exist")
	}
	temporary, err := os.MkdirTemp(filepath.Dir(destination), ".materialize-")
	if err != nil {
		return result, documentError(err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	result, err = unpack(ctx, filepath.Join(intake, ArchiveName), envelope, &expected, trust, temporary)
	if err != nil {
		return result, documentError(err)
	}
	output, err := os.OpenRoot(temporary)
	if err != nil {
		return result, documentError(err)
	}
	defer func() { _ = output.Close() }()
	if err := verifyProjection(ctx, input, output, value.Assets); err != nil {
		return result, documentError(err)
	}
	if err := writeFile(ctx, output, EnvelopeName, envelope); err != nil {
		return result, documentError(err)
	}
	result.Documents = append(result.Documents, Document{Name: EnvelopeName, SHA256: digest(envelope), Size: int64(len(envelope))})
	if err := ctx.Err(); err != nil {
		return result, documentError(err)
	}
	return result, documentError(os.Rename(temporary, destination))
}

func verifySnapshot(ctx context.Context, root *os.Root, expected Binding) (snapshot, error) {
	var value snapshot
	data, err := readFile(ctx, root, "draft.json", 1<<20)
	if err != nil {
		return value, documentError(err)
	}
	var raw map[string]json.RawMessage
	if err := decodeJSON(ctx, data, &raw); err != nil {
		return value, documentError(err)
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, documentError(err)
	}
	if value.Repository != "ShaulLavo/mesh" || value.Run != expected.WriterRun || value.ID != expected.ReleaseID || value.Version != expected.Version || value.Source != expected.Source {
		return value, errors.New("documents: physical snapshot differs from expected repository/source/release/run")
	}
	if len(value.Assets) < 9 || len(value.Assets) > 128 {
		return value, errors.New("documents: physical asset count exceeds bounds")
	}
	required := []string{ArchiveName, EnvelopeName, release.ManifestAssetName, release.DiscoveryRootAssetName, "checksums.txt", "mesh_linux_amd64.tar.gz", "mesh_linux_arm64.tar.gz", "mesh_darwin_arm64.tar.gz"}
	return value, verifyAssets(ctx, root, value.Assets, "draft.json", required)
}

func verifyAssets(ctx context.Context, root *os.Root, assets []physicalAsset, record string, required []string) error {
	seen, ids := map[string]bool{}, map[int64]bool{}
	var total int64
	for _, asset := range assets {
		if seen[asset.Name] || ids[asset.ID] {
			return errors.New("documents: repeated physical name or ID")
		}
		if err := verifyPhysical(ctx, root, asset); err != nil {
			return documentError(err)
		}
		seen[asset.Name], ids[asset.ID] = true, true
		total += asset.Size
	}
	if total > 512<<20 {
		return errors.New("documents: physical snapshot exceeds bound")
	}
	for _, name := range required {
		if !seen[name] {
			return fmt.Errorf("documents: required physical asset %s missing", name)
		}
	}
	directory, err := root.Open(".")
	if err != nil {
		return documentError(err)
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return documentError(err)
	}
	if len(entries) != len(seen)+1 || seen[record] {
		return errors.New("documents: physical directory differs from inventory")
	}
	return nil
}

func verifyPhysical(ctx context.Context, root *os.Root, asset physicalAsset) error {
	if asset.ID <= 0 || asset.State != "uploaded" || asset.Size <= 0 || asset.Size > 128<<20 || asset.Name == "draft.json" || filepath.Base(asset.Name) != asset.Name || strings.ContainsAny(asset.Name, "\\/\x00") {
		return errors.New("documents: invalid physical asset identity")
	}
	if len(asset.Digest) != 71 || !strings.HasPrefix(asset.Digest, "sha256:") || strings.Trim(asset.Digest[7:], "0123456789abcdef") != "" {
		return errors.New("documents: physical SHA256 required")
	}
	file, err := root.OpenFile(asset.Name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return documentError(err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return documentError(err)
	}
	if !info.Mode().IsRegular() || info.Size() != asset.Size {
		return errors.New("documents: physical asset is not exact regular file")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(contextReader{ctx, file}, asset.Size+1))
	if err != nil {
		return documentError(err)
	}
	if n != asset.Size || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != asset.Digest {
		return errors.New("documents: physical asset differs from uploaded digest")
	}
	return nil
}

func verifyProjection(ctx context.Context, input, output *os.Root, assets []physicalAsset) error {
	aliases := 0
	for _, asset := range assets {
		if !strings.HasSuffix(asset.Name, ".json") || asset.Name == EnvelopeName {
			continue
		}
		if asset.Name != release.ManifestAssetName && asset.Name != release.DiscoveryRootAssetName {
			aliases++
		}
		if err := compareProjection(ctx, input, output, asset.Name); err != nil {
			return documentError(err)
		}
	}
	if aliases > 9 {
		return errors.New("documents: legacy receipt projection exceeds nine aliases")
	}
	return nil
}

func compareProjection(ctx context.Context, input, output *os.Root, name string) error {
	if name != release.ManifestAssetName && name != release.DiscoveryRootAssetName && (len(name) != 69 || strings.TrimSuffix(name, ".json") == name || strings.Trim(name[:64], "0123456789abcdef") != "") {
		return errors.New("documents: unexpected physical JSON asset")
	}
	physical, err := readFile(ctx, input, name, documentLimit(name))
	if err != nil {
		return documentError(err)
	}
	logical, err := readFile(ctx, output, name, documentLimit(name))
	if err != nil {
		return documentError(err)
	}
	if digest(physical) != digest(logical) {
		return errors.New("documents: physical projection differs from authenticated member")
	}
	return nil
}
