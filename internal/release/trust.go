package release

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	tufmetadata "github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/config"
	"github.com/theupdateframework/go-tuf/v2/metadata/trustedmetadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/updater"
)

//go:embed trust/bootstrap-root.json trust/trusted-root.json trust/provenance.json
var embeddedTrust embed.FS

type SignerIdentity struct {
	Issuer       string
	Workflow     string
	Repository   string
	RepositoryID string
	OwnerID      string
	Ref          string
	Runner       string
}

type TrustRoot struct {
	Version       int
	BootstrapRoot []byte
	Trusted       []byte
	Identity      SignerIdentity
	history       []authenticatedMaterial
	provenance    []byte
}

func EmbeddedTrustRoot() TrustRoot {
	bootstrap, _ := embeddedTrust.ReadFile("trust/bootstrap-root.json")
	trusted, _ := embeddedTrust.ReadFile("trust/trusted-root.json")
	provenance, _ := embeddedTrust.ReadFile("trust/provenance.json")
	return TrustRoot{Version: 1, BootstrapRoot: bootstrap, Trusted: trusted, provenance: provenance, Identity: SignerIdentity{
		Issuer:     "https://token.actions.githubusercontent.com",
		Workflow:   "https://github.com/ShaulLavo/mesh/.github/workflows/release.yml@refs/heads/main",
		Repository: "https://github.com/ShaulLavo/mesh", RepositoryID: "1351591203", OwnerID: "68500209",
		Ref: "refs/heads/main", Runner: "github-hosted",
	}}
}

func (trust TrustRoot) material() (root.TrustedMaterial, error) {
	baseline, _, err := trust.authenticatedBaseline("")
	if err != nil {
		return nil, err
	}
	return effectiveTrust(append([]authenticatedMaterial{baseline}, trust.history...))
}

func LoadTrustRoot(cache string) (TrustRoot, error) {
	return loadTrustRoot(EmbeddedTrustRoot(), cache)
}

func loadTrustRoot(trust TrustRoot, cache string) (TrustRoot, error) {
	trust.history = nil
	_, roots, err := trust.authenticatedBaseline(cache)
	if err != nil {
		return trust, err
	}
	if cache == "" {
		return trust, nil
	}
	files, err := filepath.Glob(filepath.Join(cache, "material", "*.json"))
	if err != nil {
		return trust, fmt.Errorf("release: locate trust history: %w", err)
	}
	for _, path := range files {
		contents, err := readTrustFile(path)
		if err != nil {
			return trust, err
		}
		var saved cachedMaterial
		if err := json.Unmarshal(contents, &saved); err != nil {
			return trust, fmt.Errorf("release: decode cached material witness: %w", err)
		}
		if err := VerifyDigest(saved.Material, strings.TrimSuffix(filepath.Base(path), ".json")); err != nil {
			return trust, err
		}
		verified, err := authenticateMaterial(roots, saved)
		if err != nil {
			return trust, err
		}
		trust.history = append(trust.history, verified)
	}
	_, err = trust.material()
	return trust, err
}

type TrustRefreshBudget struct {
	// Eight requests leave room for the final root probe and top-level metadata.
	Documents int
	Duration  time.Duration
}

var ErrTrustRefreshBudget = errors.New("release: trust refresh budget exhausted; resume on next pass")

func RefreshTrust(ctx context.Context, cache string, budget TrustRefreshBudget) (TrustRoot, error) {
	return refreshTrust(ctx, EmbeddedTrustRoot(), cache, budget, http.DefaultClient)
}

func refreshTrust(ctx context.Context, trust TrustRoot, cache string, budget TrustRefreshBudget, client *http.Client) (TrustRoot, error) {
	if budget.Documents < 8 || budget.Documents > 64 || budget.Duration <= 0 || budget.Duration > 30*time.Second {
		return trust, errors.New("release: invalid trust refresh budget")
	}
	ctx, cancel := context.WithTimeout(ctx, budget.Duration)
	defer cancel()
	_, roots, err := trust.authenticatedBaseline(cache)
	if err != nil {
		return trust, err
	}
	fetch := &trustFetcher{ctx: ctx, client: client, remaining: budget.Documents, chain: roots.chain, cache: cache, documents: make(map[string][]byte)}
	// Sigstore's wrapper first attempts an unsafe local load even with caching disabled.
	// Start its maintained underlying updater directly with only the authenticated root.
	cfg, err := config.New(tuf.DefaultMirror, roots.latest)
	if err != nil {
		return trust, fmt.Errorf("release: configure fresh TUF refresh: %w", err)
	}
	cfg.DisableLocalCache = true
	cfg.Fetcher = fetch
	clientUpdater, err := updater.New(cfg)
	if err != nil {
		return trust, fmt.Errorf("release: refresh Sigstore trust: %w", err)
	}
	target, err := clientUpdater.GetTargetInfo(trustedMaterialTarget)
	if err != nil {
		return trust, fmt.Errorf("release: authenticate current Sigstore target: %w", err)
	}
	_, contents, err := clientUpdater.DownloadTarget(target, "", "")
	if err != nil {
		return trust, fmt.Errorf("release: fetch authenticated Sigstore material: %w", err)
	}
	if _, err := root.NewTrustedRootFromJSON(contents); err != nil {
		return trust, fmt.Errorf("release: validate new trust material: %w", err)
	}
	witness, err := fetch.materialWitness()
	if err != nil {
		return trust, err
	}
	if err := persistTrustMaterial(cache, contents, witness); err != nil {
		return trust, err
	}
	return loadTrustRoot(trust, cache)
}

type trustFetcher struct {
	ctx       context.Context
	client    *http.Client
	remaining int
	chain     *trustedmetadata.TrustedMetadata
	cache     string
	documents map[string][]byte
}

func (fetch *trustFetcher) DownloadFile(address string, maximum int64, _ time.Duration) ([]byte, error) {
	if fetch.remaining == 0 {
		return nil, ErrTrustRefreshBudget
	}
	fetch.remaining--
	if !strings.HasPrefix(address, tuf.DefaultMirror+"/") {
		return nil, errors.New("release: unexpected trust origin")
	}
	request, err := http.NewRequestWithContext(fetch.ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("release: trust request: %w", err)
	}
	response, err := fetch.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("release: trust download: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // The bounded read result is authoritative.
	if response.StatusCode != http.StatusOK {
		return nil, &tufmetadata.ErrDownloadHTTP{StatusCode: response.StatusCode, URL: address}
	}
	if maximum > 1<<20 {
		maximum = 1 << 20
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("release: read trust document: %w", err)
	}
	if int64(len(contents)) > maximum {
		return nil, errors.New("release: oversized trust document")
	}
	if err := fetch.saveRoot(address, contents); err != nil {
		return nil, err
	}
	if fetch.documents != nil {
		fetch.documents[strings.TrimPrefix(address, tuf.DefaultMirror+"/")] = contents
	}
	return contents, nil
}

func (fetch *trustFetcher) saveRoot(address string, contents []byte) error {
	name := strings.TrimPrefix(address, tuf.DefaultMirror+"/")
	if !strings.HasSuffix(name, ".root.json") {
		return nil
	}
	if _, err := fetch.chain.UpdateRoot(contents); err != nil {
		return fmt.Errorf("release: authenticate successive TUF root: %w", err)
	}
	want := strconv.FormatInt(fetch.chain.Root.Signed.Version, 10) + ".root.json"
	if name != want {
		return errors.New("release: TUF root version differs from its path")
	}
	return writeTrustFile(filepath.Join(fetch.cache, "roots", want), contents)
}

func persistTrustMaterial(cache string, contents []byte, witness materialWitness) error {
	digest := sha256.Sum256(contents)
	saved, err := json.Marshal(cachedMaterial{Material: contents, Witness: witness})
	if err != nil {
		return fmt.Errorf("release: encode material witness: %w", err)
	}
	if len(saved) > 1<<20 {
		return errors.New("release: material witness exceeds 1 MiB")
	}
	return writeTrustFile(filepath.Join(cache, "material", hex.EncodeToString(digest[:])+".json"), saved)
}

func writeTrustFile(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("release: create trust cache: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".trust-*")
	if err != nil {
		return fmt.Errorf("release: stage trust material: %w", err)
	}
	defer os.Remove(file.Name()) //nolint:errcheck // Rename consumes the temporary file on success.
	_, writeErr := file.Write(contents)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("release: persist trust material: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("release: publish trust material: %w", err)
	}
	parent, err := os.Open(filepath.Dir(path)) //nolint:gosec // The owner-selected trust cache contains this directory.
	if err != nil {
		return fmt.Errorf("release: open trust directory: %w", err)
	}
	err = errors.Join(parent.Sync(), parent.Close())
	if err != nil {
		return fmt.Errorf("release: sync trust directory: %w", err)
	}
	return nil
}

func readTrustFile(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // Paths are derived from the owner-selected cache and fixed trust filenames.
	if err != nil {
		return nil, fmt.Errorf("release: open trust file: %w", err)
	}
	defer file.Close() //nolint:errcheck // The bounded read result is authoritative.
	contents, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("release: read trust file: %w", err)
	}
	if len(contents) > 1<<20 {
		return nil, errors.New("release: trust file exceeds 1 MiB")
	}
	return contents, nil
}
