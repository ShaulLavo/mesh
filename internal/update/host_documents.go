package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/release"
)

const hostReaderEpoch = HostReaderEpoch
const hostHTTPS = "https"

// HostDocuments binds discovery to the same immutable origin as installation.
// The daemon supplies only the official origin; isolated tests supply HTTPS fixtures.
type HostDocuments struct {
	Client release.Client
	Trust  release.TrustRoot
}

func (d HostDocuments) Source() release.DocumentSource {
	return release.DocumentSource{Read: func(ctx context.Context, ref release.AssetRef, kind release.DocumentKind) ([]byte, error) {
		return d.read(ctx, ref.Tag, ref.Name, release.DocumentLimit(kind))
	}, VerifyMetadata: d.verifyMetadata}
}

func (d HostDocuments) verifyMetadata(ctx context.Context, ref release.AssetRef, data []byte) error {
	envelope, err := d.read(ctx, ref.Tag, "mesh-update.sig.json", release.MaxSignatureBytes)
	if err != nil {
		return err
	}
	if err := release.VerifySignature(d.Trust, data, envelope); err != nil {
		return hostError(err)
	}
	metadata, err := release.DecodeUpdateMetadata(data)
	if err != nil {
		return hostError(err)
	}
	if metadata.Trust != d.Trust.Version {
		return errors.New("update: unsupported authenticated metadata trust version")
	}
	return nil
}

func (d HostDocuments) Current(ctx context.Context, current release.Build) (HostSource, error) {
	return d.current(ctx, current, hostReaderEpoch)
}

func (d HostDocuments) current(ctx context.Context, current release.Build, epoch uint32) (HostSource, error) {
	data, err := d.read(ctx, current.Version, release.UpdateMetadataAssetName, release.MaxMetadataBytes)
	if err != nil {
		return HostSource{}, err
	}
	ref := release.AssetRef{Tag: current.Version, Name: release.UpdateMetadataAssetName, SHA256: discoveryDigest(data)}
	publication, err := release.DecodeUpdateMetadata(data)
	if err != nil {
		return HostSource{}, hostError(err)
	}
	if publication.Epoch > epoch {
		return HostSource{}, errors.New("update: source publication epoch exceeds this reader")
	}
	metadata, err := release.AuthenticateMetadata(ctx, d.Source(), ref, data, publication.Epoch)
	if err != nil {
		return HostSource{}, hostError(err)
	}
	reader := metadata.Updater
	if reader == nil || reader.Epoch != epoch {
		return HostSource{}, errors.New("update: authenticated updater-capable source marker unavailable for this reader epoch")
	}
	manifest, err := d.read(ctx, current.Version, release.ManifestAssetName, release.DocumentLimit(release.ManifestDocument))
	if err != nil {
		return HostSource{}, err
	}
	if err := release.VerifyDigest(manifest, metadata.Manifest); err != nil {
		return HostSource{}, hostError(err)
	}
	var frozen release.Manifest
	if err := json.Unmarshal(manifest, &frozen); err != nil {
		return HostSource{}, fmt.Errorf("update: decode authenticated source manifest: %w", err)
	}
	source := HostSource{Epoch: reader.Epoch, Position: reader.Position, Manifest: frozen}
	return source, verifyHostSource(current, source)
}

func (d HostDocuments) read(ctx context.Context, tag, name string, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("update: unsupported host document kind")
	}
	ref := release.AssetRef{Tag: tag, Name: name, SHA256: strings.Repeat("0", 64)}
	if tag != release.LatestDiscoveryTag {
		if err := ref.Validate(); err != nil {
			return nil, hostError(err)
		}
	} else if name != release.DiscoveryRootAssetName {
		return nil, errors.New("update: unsupported latest discovery asset")
	}
	base := d.Client.BaseURL
	if base == "" {
		base = release.OfficialBaseURL
	}
	origin, err := url.Parse(base)
	if err != nil || origin.Scheme != hostHTTPS || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, errors.New("update: host documents require an HTTPS release origin")
	}
	path := "/download/" + url.PathEscape(tag) + "/" + url.PathEscape(name)
	if tag == release.LatestDiscoveryTag {
		path = "/latest/download/" + url.PathEscape(name)
	}
	client := hostHTTPClient(d.Client.HTTPClient)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+path, nil)
	if err != nil {
		return nil, fmt.Errorf("update: create host document request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("update: read host document %s: %w", name, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: host document %s: HTTP %d", name, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("update: read host document body: %w", err)
	}
	if len(data) > limit {
		return nil, errors.New("update: host document exceeds byte limit")
	}
	return data, nil
}

func hostHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	bound := *client
	bound.Timeout = 30 * time.Second
	prior := bound.CheckRedirect
	bound.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if prior != nil {
			if err := prior(request, via); err != nil {
				return err
			}
		}
		if len(via) >= 10 || request.URL.Scheme != hostHTTPS || request.URL.User != nil {
			return errors.New("update: refused host document redirect")
		}
		return nil
	}
	return &bound
}

func OfficialHostSource(stateDir string) func(context.Context, release.Build) (HostSource, release.DocumentSource, error) {
	return func(ctx context.Context, current release.Build) (HostSource, release.DocumentSource, error) {
		cache := filepath.Join(stateDir, "updates", "host-trust")
		trust, err := release.RefreshTrust(ctx, cache, release.TrustRefreshBudget{Documents: 8, Duration: 30 * time.Second})
		if err != nil {
			return HostSource{}, release.DocumentSource{}, fmt.Errorf("update: refresh host trust: %w", err)
		}
		documents := HostDocuments{Trust: trust}
		source, err := documents.Current(ctx, current)
		return source, CacheDiscoveryDocuments(filepath.Join(stateDir, "updates", "host-documents"), documents.Source()), err
	}
}
