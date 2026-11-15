package releasedocuments

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/shaul/mesh/internal/release"
)

type Store struct {
	client    *http.Client
	origin    string
	trust     release.TrustRoot
	directory string
	mu        sync.Mutex
	gate      chan struct{}
	sets      map[string]Inventory
	bytes     int64
	closed    bool
}

func Open(ctx context.Context, client release.Client, trust release.TrustRoot, scratch string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, documentError(err)
	}
	origin := strings.TrimRight(client.BaseURL, "/")
	if origin == "" {
		origin = release.OfficialBaseURL
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("documents: HTTPS release origin required")
	}
	directory, err := os.MkdirTemp(scratch, "mesh-documents-")
	if err != nil {
		return nil, documentError(err)
	}
	httpClient := client.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	secured := *httpClient
	prior := secured.CheckRedirect
	secured.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if prior != nil {
			if err := prior(request, via); err != nil {
				return documentError(err)
			}
		}
		if request.URL.Scheme != "https" || request.URL.User != nil || len(via) >= 10 {
			return errors.New("documents: refused redirect")
		}
		return nil
	}
	return &Store{client: &secured, origin: origin, trust: trust, directory: directory, gate: make(chan struct{}, 1), sets: map[string]Inventory{}}, nil
}

func (s *Store) Close() error {
	s.gate <- struct{}{}
	defer func() { <-s.gate }()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return documentError(os.RemoveAll(s.directory))
}

func (s *Store) Bootstrap(ctx context.Context, tag string) ([]byte, []byte, error) {
	if err := s.acquire(ctx, tag); err != nil {
		return nil, nil, documentError(err)
	}
	metadata, err := s.local(ctx, tag, release.UpdateMetadataAssetName, release.MaxMetadataBytes)
	if err != nil {
		return nil, nil, documentError(err)
	}
	envelope, err := s.local(ctx, tag, EnvelopeName, release.MaxSignatureBytes)
	return metadata, envelope, documentError(err)
}

func (s *Store) Read(ctx context.Context, ref release.AssetRef, kind release.DocumentKind) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, documentError(err)
	}
	if err := s.acquire(ctx, ref.Tag); err != nil {
		return nil, documentError(err)
	}
	data, err := s.local(ctx, ref.Tag, ref.Name, int64(release.DocumentLimit(kind)))
	if err != nil {
		return nil, documentError(err)
	}
	if err := release.VerifyDigest(data, ref.SHA256); err != nil {
		return nil, documentError(err)
	}
	return data, nil
}

func (s *Store) local(ctx context.Context, tag, name string, limit int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("documents: store closed")
	}
	root, err := os.OpenRoot(filepath.Join(s.directory, tag))
	if err != nil {
		return nil, documentError(err)
	}
	defer func() { _ = root.Close() }()
	return readFile(ctx, root, name, limit)
}

func (s *Store) acquire(ctx context.Context, tag string) error {
	if _, err := release.CompareVersions(tag, tag); err != nil {
		return documentError(err)
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return documentError(ctx.Err())
	}
	defer func() { <-s.gate }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("documents: store closed")
	}
	if _, ok := s.sets[tag]; ok {
		return nil
	}
	if len(s.sets) >= 32 {
		return errors.New("documents: operation release count exceeds bound")
	}
	return s.fetch(ctx, tag)
}

func (s *Store) fetch(ctx context.Context, tag string) error {
	directory, err := os.MkdirTemp(s.directory, ".incoming-")
	if err != nil {
		return documentError(err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	root, err := os.OpenRoot(directory)
	if err != nil {
		return documentError(err)
	}
	defer func() { _ = root.Close() }()
	if err := s.download(ctx, tag, EnvelopeName, root, release.MaxSignatureBytes); err != nil {
		return documentError(err)
	}
	envelope, err := readFile(ctx, root, EnvelopeName, release.MaxSignatureBytes)
	if err != nil {
		return documentError(err)
	}
	if err := s.download(ctx, tag, ArchiveName, root, MaxCompressed); err != nil {
		return documentError(err)
	}
	inventory, err := unpack(ctx, filepath.Join(directory, ArchiveName), envelope, nil, s.trust, directory)
	if err != nil {
		return documentError(err)
	}
	if inventory.Binding.Version != tag {
		return errors.New("documents: archive belongs to another release")
	}
	var size int64
	for _, document := range inventory.Documents {
		size += document.Size
	}
	if s.bytes+size > MaxExpanded {
		return errors.New("documents: operation backing exceeds bound")
	}
	if err := root.Remove(ArchiveName); err != nil {
		return documentError(err)
	}
	if err := os.Rename(directory, filepath.Join(s.directory, tag)); err != nil {
		return documentError(err)
	}
	s.bytes += size
	s.sets[tag] = inventory
	return nil
}

func (s *Store) download(ctx context.Context, tag, name string, root *os.Root, limit int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.origin+"/download/"+url.PathEscape(tag)+"/"+url.PathEscape(name), nil)
	if err != nil {
		return documentError(err)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return documentError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("documents: %s/%s HTTP %d", tag, name, response.StatusCode)
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return documentError(err)
	}
	n, copyErr := io.Copy(file, io.LimitReader(contextReader{ctx, response.Body}, limit+1))
	closeErr := file.Close()
	if n > limit {
		return errors.New("documents: download exceeds bound")
	}
	return errors.Join(copyErr, closeErr)
}
