package update

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

	"github.com/shaul/mesh/internal/release"
)

const maxDiscoveryCursorBytes = 512 << 10

// DiscoveryCursor is independent of installation journals and updater policy.
// Root and TraversalEpoch retain progress across arbitrarily many newer epochs.
type DiscoveryCursor struct {
	Epoch          uint32              `json:"epoch"`
	Latest         string              `json:"latest"`
	Root           *release.AssetRef   `json:"root,omitempty"`
	TraversalEpoch uint32              `json:"traversalEpoch"`
	Head           release.AssetRef    `json:"head"`
	Metadata       json.RawMessage     `json:"metadata,omitempty"`
	Route          release.RouteCursor `json:"route"`
}

type DiscoveryStep struct {
	Cursor    DiscoveryCursor         `json:"cursor"`
	Next      *release.PlannedRelease `json:"next,omitempty"`
	Budget    release.PassBudget      `json:"budget"`
	Exhausted bool                    `json:"exhausted"`
	Waiting   bool                    `json:"waiting"`
}

// DiscoverNextHop always reads the stable latest root first. Incompatible epoch
// metadata is never requested, let alone decoded. Signature verification belongs
// to the supplied source; this function issues no installation permission.
func DiscoverNextHop(ctx context.Context, from release.Build, at release.Position, epoch uint32, cursor DiscoveryCursor, source release.DocumentSource, budget release.PassBudget) (DiscoveryStep, error) {
	step := DiscoveryStep{Cursor: cursor, Budget: budget}
	if epoch == 0 {
		return step, errors.New("update: discovery reader epoch must be positive")
	}
	err := discoverHead(ctx, epoch, &step, source)
	if errors.Is(err, release.ErrPassExhausted) {
		step.Exhausted = true
		return step, nil
	}
	if err != nil {
		return step, err
	}
	metadata, err := discoveryMetadata(ctx, epoch, &step, source)
	if errors.Is(err, release.ErrPassExhausted) {
		step.Exhausted = true
		return step, nil
	}
	if err != nil {
		return step, err
	}
	route, err := release.NextHop(ctx, from, at, metadata, step.Cursor.Route, source, step.Budget)
	step.Cursor.Route, step.Next, step.Budget = route.Cursor, route.Next, route.Budget
	step.Exhausted, step.Waiting = route.Exhausted, route.Waiting
	if err != nil {
		return step, fmt.Errorf("update: discover next hop: %w", err)
	}
	return step, nil
}

func discoverHead(ctx context.Context, epoch uint32, step *DiscoveryStep, source release.DocumentSource) error {
	ref := release.AssetRef{Tag: "latest", Name: release.DiscoveryRootAssetName}
	data, err := release.ReadDocument(ctx, source, ref, release.RootDocument, &step.Budget)
	if err != nil {
		return err
	}
	root, err := release.DecodeDiscoveryRoot(data)
	if err != nil {
		return fmt.Errorf("update: latest discovery root: %w", err)
	}
	token := discoveryDigest(data)
	if step.Cursor.Latest != token || step.Cursor.Epoch != epoch {
		step.Cursor.Latest, step.Cursor.Epoch = token, epoch
		step.Cursor.Root, step.Cursor.TraversalEpoch = nil, 0
		if err := advanceRoot(epoch, root, step); err != nil {
			return err
		}
	}
	for step.Cursor.Root != nil {
		data, err := release.ReadDocument(ctx, source, *step.Cursor.Root, release.RootDocument, &step.Budget)
		if err != nil {
			return err
		}
		root, err := release.DecodeDiscoveryRoot(data)
		if err != nil {
			return fmt.Errorf("update: previous discovery root: %w", err)
		}
		if root.Epoch != step.Cursor.TraversalEpoch-1 {
			return errors.New("update: previous discovery root has the wrong epoch")
		}
		if err := advanceRoot(epoch, root, step); err != nil {
			return err
		}
	}
	return nil
}

func advanceRoot(epoch uint32, root release.DiscoveryRoot, step *DiscoveryStep) error {
	if root.Epoch < epoch {
		return errors.New("update: discovery origin is older than the reader epoch")
	}
	if root.Epoch > epoch {
		step.Cursor.Root, step.Cursor.TraversalEpoch = root.Previous, root.Epoch
		return nil
	}
	step.Cursor.Root, step.Cursor.TraversalEpoch = nil, epoch
	if step.Cursor.Head != root.Head {
		step.Cursor.Head, step.Cursor.Metadata = root.Head, nil
	}
	return nil
}

func discoveryMetadata(ctx context.Context, epoch uint32, step *DiscoveryStep, source release.DocumentSource) (release.UpdateMetadata, error) {
	var err error
	if len(step.Cursor.Metadata) == 0 {
		step.Cursor.Metadata, err = release.ReadDocument(ctx, source, step.Cursor.Head, release.MetadataDocument, &step.Budget)
	}
	if err != nil {
		return release.UpdateMetadata{}, err
	}
	metadata, err := release.AuthenticateMetadata(ctx, source, step.Cursor.Head, step.Cursor.Metadata, epoch)
	if err != nil {
		return metadata, fmt.Errorf("update: authenticate discovery head: %w", err)
	}
	return metadata, nil
}

func LoadDiscoveryCursor(path string) (DiscoveryCursor, error) {
	var cursor DiscoveryCursor
	data, err := readDiscoveryFile(path, maxDiscoveryCursorBytes)
	if errors.Is(err, os.ErrNotExist) {
		return cursor, nil
	}
	if err != nil {
		return cursor, err
	}
	if err := json.Unmarshal(data, &cursor); err != nil {
		return cursor, fmt.Errorf("update: decode discovery cursor: %w", err)
	}
	return cursor, nil
}

func SaveDiscoveryCursor(path string, cursor DiscoveryCursor) error {
	data, err := json.Marshal(cursor)
	if err != nil {
		return fmt.Errorf("update: encode discovery cursor: %w", err)
	}
	if len(data) > maxDiscoveryCursorBytes {
		return errors.New("update: discovery cursor exceeds 512 KiB")
	}
	return writeDiscoveryFile(path, data)
}

// CacheDiscoveryDocuments stores exact digest-addressed bytes. Metadata is still
// authenticated by VerifyMetadata on every use, including after a restart.
func CacheDiscoveryDocuments(directory string, upstream release.DocumentSource) release.DocumentSource {
	return release.DocumentSource{Read: func(ctx context.Context, ref release.AssetRef, kind release.DocumentKind) ([]byte, error) {
		return cachedDiscoveryDocument(ctx, directory, upstream, ref, kind)
	}, VerifyMetadata: upstream.VerifyMetadata}
}

func cachedDiscoveryDocument(ctx context.Context, directory string, upstream release.DocumentSource, ref release.AssetRef, kind release.DocumentKind) ([]byte, error) {
	if ref.SHA256 == "" {
		return upstream.Read(ctx, ref, kind)
	}
	if err := ref.Validate(); err != nil {
		return nil, fmt.Errorf("update: cache reference: %w", err)
	}
	path := filepath.Join(directory, ref.SHA256)
	data, err := readDiscoveryFile(path, release.DocumentLimit(kind))
	if err == nil && discoveryDigest(data) == ref.SHA256 {
		return data, nil
	}
	data, err = upstream.Read(ctx, ref, kind)
	if err != nil {
		return nil, fmt.Errorf("update: read discovery document: %w", err)
	}
	if len(data) > release.DocumentLimit(kind) || discoveryDigest(data) != ref.SHA256 {
		return nil, errors.New("update: discovery cache input differs from its reference")
	}
	if err := writeDiscoveryFile(path, data); err != nil {
		return nil, err
	}
	return data, nil
}

func readDiscoveryFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("update: open discovery file: %w", err)
	}
	defer file.Close() //nolint:errcheck // read result is authoritative
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("update: read discovery file: %w", err)
	}
	if len(data) > limit {
		return nil, errors.New("update: discovery file exceeds its byte bound")
	}
	return data, nil
}

func writeDiscoveryFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("update: create discovery directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".discovery-*")
	if err != nil {
		return fmt.Errorf("update: create discovery file: %w", err)
	}
	defer os.Remove(file.Name()) //nolint:errcheck // temporary publication cleanup
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("update: write discovery file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("update: sync discovery file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("update: close discovery file: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("update: publish discovery file: %w", err)
	}
	return syncDiscoveryDirectory(filepath.Dir(path))
}

func syncDiscoveryDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("update: open discovery directory: %w", err)
	}
	defer directory.Close() //nolint:errcheck // sync result is authoritative
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("update: sync discovery directory: %w", err)
	}
	return nil
}

func discoveryDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
