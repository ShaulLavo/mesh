package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	UpdateMetadataSchema = 1
	MaxMetadataBytes     = 64 << 10
	MaxVersionBytes      = 64
	MaxAssetNameBytes    = 128
)

type UpdateMetadata struct {
	Schema      int           `json:"schema"`
	Epoch       uint32        `json:"epoch"`
	Trust       int           `json:"trust"`
	Version     string        `json:"version"`
	Manifest    string        `json:"manifest"`
	PublishedAt time.Time     `json:"publishedAt"`
	Position    Position      `json:"position"`
	Direct      DirectSources `json:"direct"`
	Rollback    []RollbackRef `json:"rollback"`
	Page        AssetRef      `json:"page"`
}

type Position struct {
	Anchor  bool   `json:"anchor"`
	Ordinal uint64 `json:"ordinal"`
}

type DirectSources struct {
	AnchorsFrom uint64   `json:"anchorsFrom"`
	Since       []string `json:"since"`
}

type RollbackRef struct {
	Forward  string `json:"forward"`
	Rollback string `json:"rollback"`
}

type AssetRef struct {
	Tag    string `json:"tag"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

func (ref AssetRef) Validate() error {
	if err := validateMetadataVersion(ref.Tag); err != nil {
		return err
	}
	if ref.Name == "" || len(ref.Name) > MaxAssetNameBytes || ref.Name == "." || ref.Name == ".." {
		return errors.New("release: invalid asset name length or path")
	}
	for _, char := range ref.Name {
		if char < '!' || char > '~' || strings.ContainsRune("/\\?#%", char) {
			return errors.New("release: asset name must be an ASCII filename")
		}
	}
	if !validDigest(ref.SHA256) {
		return errors.New("release: asset reference has an invalid SHA-256")
	}
	return nil
}

func DecodeUpdateMetadata(contents []byte) (UpdateMetadata, error) {
	var metadata UpdateMetadata
	if len(contents) > MaxMetadataBytes {
		return metadata, errors.New("release: update metadata exceeds 64 KiB")
	}
	if err := json.Unmarshal(contents, &metadata); err != nil {
		return metadata, fmt.Errorf("release: decode update metadata: %w", err)
	}
	return metadata, metadata.Validate()
}

func (metadata UpdateMetadata) Validate() error {
	if metadata.Schema != UpdateMetadataSchema || metadata.Epoch == 0 || metadata.Trust <= 0 {
		return errors.New("release: unsupported metadata schema, epoch or trust version")
	}
	if err := validateMetadataVersion(metadata.Version); err != nil {
		return err
	}
	if !validDigest(metadata.Manifest) || metadata.PublishedAt.IsZero() || metadata.PublishedAt.Year() < 1 || metadata.PublishedAt.Year() > 9999 {
		return errors.New("release: invalid manifest digest or publication time")
	}
	if metadata.Direct.AnchorsFrom > metadata.Position.Ordinal {
		return errors.New("release: direct anchor range exceeds release position")
	}
	if err := validateSince(metadata.Direct.Since, metadata.Version); err != nil {
		return err
	}
	if err := validateRollbackRefs(metadata.Rollback); err != nil {
		return err
	}
	if err := metadata.Page.Validate(); err != nil {
		return err
	}
	if order, _ := CompareVersions(metadata.Page.Tag, metadata.Version); order >= 0 {
		return errors.New("release: anchor page must belong to a prior published release")
	}
	contents, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("release: encode update metadata: %w", err)
	}
	if len(contents) > MaxMetadataBytes {
		return errors.New("release: update metadata exceeds 64 KiB")
	}
	return nil
}

func validateMetadataVersion(version string) error {
	if len(version) > MaxVersionBytes {
		return errors.New("release: version exceeds 64 bytes")
	}
	_, err := parseVersion(version)
	return err
}

func validateSince(versions []string, target string) error {
	previous := ""
	for _, version := range versions {
		if err := validateMetadataVersion(version); err != nil {
			return err
		}
		if order, _ := CompareVersions(version, target); order >= 0 {
			return errors.New("release: direct source must precede target")
		}
		order, _ := CompareVersions(previous, version)
		if previous != "" && order >= 0 {
			return errors.New("release: direct sources must increase without duplicates")
		}
		previous = version
	}
	return nil
}

func validateRollbackRefs(refs []RollbackRef) error {
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if !validDigest(ref.Forward) || !validDigest(ref.Rollback) || seen[ref.Forward] {
			return errors.New("release: invalid or duplicate rollback reference")
		}
		seen[ref.Forward] = true
	}
	return nil
}
