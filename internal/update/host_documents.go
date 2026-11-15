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
	"github.com/shaul/mesh/internal/releasedocuments"
)

const hostReaderEpoch = HostReaderEpoch
const hostHTTPS = "https"

// HostDocuments binds discovery to the same immutable origin as installation.
// The daemon supplies only the official origin; isolated tests supply HTTPS fixtures.
type HostDocuments struct {
	Client release.Client
	Trust  release.TrustRoot
	store  *releasedocuments.Store
}

func OpenHostDocuments(ctx context.Context, client release.Client, trust release.TrustRoot, scratch string) (HostDocuments, error) {
	store, err := releasedocuments.Open(ctx, client, trust, scratch)
	if err != nil {
		return HostDocuments{}, fmt.Errorf("update: open release documents: %w", err)
	}
	return HostDocuments{Client: client, Trust: trust, store: store}, nil
}

func (d HostDocuments) Close() error { return hostError(d.store.Close()) }

func (d HostDocuments) Source() release.DocumentSource {
	return release.DocumentSource{Read: func(ctx context.Context, ref release.AssetRef, kind release.DocumentKind) ([]byte, error) {
		if ref.Tag == release.LatestDiscoveryTag {
			return d.latestHint(ctx)
		}
		if ref.SHA256 == "" && ref.Name == release.UpdateMetadataAssetName && kind == release.MetadataDocument {
			data, _, err := d.store.Bootstrap(ctx, ref.Tag)
			return data, hostError(err)
		}
		data, err := d.store.Read(ctx, ref, kind)
		return data, hostError(err)
	}, VerifyMetadata: d.verifyMetadata}
}

func (d HostDocuments) verifyMetadata(ctx context.Context, ref release.AssetRef, data []byte) error {
	_, envelope, err := d.store.Bootstrap(ctx, ref.Tag)
	if err != nil {
		return hostError(err)
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
	data, _, err := d.store.Bootstrap(ctx, current.Version)
	if err != nil {
		return HostSource{}, hostError(err)
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
	manifest, err := d.store.Read(ctx, release.AssetRef{Tag: current.Version, Name: release.ManifestAssetName, SHA256: metadata.Manifest}, release.ManifestDocument)
	if err != nil {
		return HostSource{}, hostError(err)
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

func (d HostDocuments) latestHint(ctx context.Context) ([]byte, error) {
	base := d.Client.BaseURL
	if base == "" {
		base = release.OfficialBaseURL
	}
	origin, err := url.Parse(base)
	if err != nil || origin.Scheme != hostHTTPS || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, errors.New("update: host documents require an HTTPS release origin")
	}
	path := "/latest/download/" + release.DiscoveryRootAssetName
	client := hostHTTPClient(d.Client.HTTPClient)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+path, nil)
	if err != nil {
		return nil, fmt.Errorf("update: create host document request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("update: read latest root: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: latest root: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, release.MaxDiscoveryRootBytes+1))
	if err != nil {
		return nil, fmt.Errorf("update: read host document body: %w", err)
	}
	if len(data) > release.MaxDiscoveryRootBytes {
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

func OfficialHostSource(stateDir string) func(context.Context, release.Build) (HostSource, release.DocumentSource, func() error, error) {
	return func(ctx context.Context, current release.Build) (HostSource, release.DocumentSource, func() error, error) {
		cache := filepath.Join(stateDir, "updates", "host-trust")
		trust, err := release.RefreshTrust(ctx, cache, release.TrustRefreshBudget{Documents: 8, Duration: 30 * time.Second})
		if err != nil {
			return HostSource{}, release.DocumentSource{}, nil, fmt.Errorf("update: refresh host trust: %w", err)
		}
		documents, err := OpenHostDocuments(ctx, release.Client{}, trust, stateDir)
		if err != nil {
			return HostSource{}, release.DocumentSource{}, nil, err
		}
		source, err := documents.Current(ctx, current)
		if err != nil {
			_ = documents.Close()
			return HostSource{}, release.DocumentSource{}, nil, err
		}
		return source, CacheDiscoveryDocuments(filepath.Join(stateDir, "updates", "host-documents"), documents.Source()), documents.Close, nil
	}
}
