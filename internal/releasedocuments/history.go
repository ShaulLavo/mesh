package releasedocuments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shaul/mesh/internal/release"
)

// HistoricalIdentity contains only facts independently known before catalog authentication.
type HistoricalIdentity struct {
	Version   string `json:"version"`
	Source    string `json:"source"`
	ReleaseID int64  `json:"releaseId"`
}

type historicalSnapshot struct {
	Repository string          `json:"repository"`
	ID         int64           `json:"id"`
	Version    string          `json:"tag_name"`
	Source     string          `json:"target_commitish"`
	Draft      *bool           `json:"draft"`
	Assets     []physicalAsset `json:"assets"`
}

func (expected HistoricalIdentity) validate() error {
	if _, err := release.CompareVersions(expected.Version, expected.Version); err != nil {
		return documentError(err)
	}
	if len(expected.Source) != 40 || strings.Trim(expected.Source, "0123456789abcdef") != "" || expected.ReleaseID <= 0 {
		return errors.New("documents: historical source and positive release ID required")
	}
	return nil
}

func (expected HistoricalIdentity) matches(binding Binding) bool {
	return expected.Version == binding.Version && expected.Source == binding.Source && expected.ReleaseID == binding.ReleaseID
}

// MaterializeHistory learns the writer run only from the authenticated catalog.
func MaterializeHistory(ctx context.Context, intake string, expected HistoricalIdentity, trust release.TrustRoot, destination string) (Inventory, error) {
	if err := expected.validate(); err != nil {
		return Inventory{}, err
	}
	input, err := os.OpenRoot(intake)
	if err != nil {
		return Inventory{}, documentError(err)
	}
	defer func() { _ = input.Close() }()
	assets, err := verifyHistoricalSnapshot(ctx, input, expected)
	if err != nil {
		return Inventory{}, err
	}
	return materializeHistoryCapture(ctx, intake, input, assets, expected, trust, destination)
}

func verifyHistoricalSnapshot(ctx context.Context, root *os.Root, expected HistoricalIdentity) ([]physicalAsset, error) {
	data, err := readFile(ctx, root, "history.json", 1<<20)
	if err != nil {
		return nil, documentError(err)
	}
	var raw map[string]json.RawMessage
	if err := decodeJSON(ctx, data, &raw); err != nil {
		return nil, err
	}
	var value historicalSnapshot
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, documentError(err)
	}
	if value.Repository != "ShaulLavo/mesh" || value.ID != expected.ReleaseID || value.Version != expected.Version || value.Source != expected.Source || value.Draft == nil || *value.Draft {
		return nil, errors.New("documents: historical capture differs from expected published repository/source/release")
	}
	if len(value.Assets) < 4 || len(value.Assets) > 13 {
		return nil, errors.New("documents: historical metadata asset count exceeds bounds")
	}
	for _, asset := range value.Assets {
		if err := historicalAsset(asset); err != nil {
			return nil, err
		}
	}
	required := []string{ArchiveName, EnvelopeName, release.ManifestAssetName, release.DiscoveryRootAssetName}
	return value.Assets, verifyAssets(ctx, root, value.Assets, "history.json", required)
}

func historicalAsset(asset physicalAsset) error {
	limit := documentLimit(asset.Name)
	switch asset.Name {
	case ArchiveName:
		limit = MaxCompressed
	case EnvelopeName, release.ManifestAssetName, release.DiscoveryRootAssetName:
	default:
		if len(asset.Name) != 69 || !strings.HasSuffix(asset.Name, ".json") || strings.Trim(asset.Name[:64], "0123456789abcdef") != "" {
			return errors.New("documents: history capture requires physical metadata assets only")
		}
	}
	if asset.Size > limit {
		return errors.New("documents: historical metadata asset exceeds byte bound")
	}
	return nil
}

func materializeHistoryCapture(ctx context.Context, intake string, input *os.Root, assets []physicalAsset, expected HistoricalIdentity, trust release.TrustRoot, destination string) (Inventory, error) {
	var result Inventory
	envelope, err := readFile(ctx, input, EnvelopeName, release.MaxSignatureBytes)
	if err != nil {
		return result, err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("documents: destination must not exist")
	}
	temporary, err := os.MkdirTemp(filepath.Dir(destination), ".materialize-")
	if err != nil {
		return result, documentError(err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	result, err = unpack(ctx, filepath.Join(intake, ArchiveName), envelope, nil, trust, temporary)
	if err != nil {
		return result, err
	}
	if !expected.matches(result.Binding) {
		return Inventory{}, errors.New("documents: authenticated historical catalog differs from expected source/release")
	}
	output, err := os.OpenRoot(temporary)
	if err != nil {
		return result, documentError(err)
	}
	defer func() { _ = output.Close() }()
	if err := authenticateHistoricalDocuments(ctx, output, envelope, trust); err != nil {
		return result, err
	}
	if err := verifyProjection(ctx, input, output, assets); err != nil {
		return result, err
	}
	if err := writeFile(ctx, output, EnvelopeName, envelope); err != nil {
		return result, err
	}
	result.Documents = append(result.Documents, Document{Name: EnvelopeName, SHA256: digest(envelope), Size: int64(len(envelope))})
	if err := ctx.Err(); err != nil {
		return result, documentError(err)
	}
	return result, documentError(os.Rename(temporary, destination))
}

func authenticateHistoricalDocuments(ctx context.Context, root *os.Root, envelope []byte, trust release.TrustRoot) error {
	for _, name := range []string{release.ManifestAssetName, release.UpdateMetadataAssetName} {
		data, err := readFile(ctx, root, name, documentLimit(name))
		if err != nil {
			return err
		}
		if err := release.VerifySignature(trust, data, envelope); err != nil {
			return fmt.Errorf("documents: authenticate historical %s: %w", name, err)
		}
	}
	return nil
}
