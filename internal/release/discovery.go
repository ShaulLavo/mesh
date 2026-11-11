package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	DiscoveryRootAssetName  = "mesh-update-root.json"
	UpdateMetadataAssetName = "mesh-update.json"
	MaxDiscoveryRootBytes   = 2 << 10
	MaxAnchorPageBytes      = 64 << 10
	AnchorsPerPage          = 64
	MaxPageBackrefs         = 64
)

// DiscoveryRoot is a stable location hint. It never authorizes installation.
type DiscoveryRoot struct {
	Format   int       `json:"format"`
	Epoch    uint32    `json:"epoch"`
	Head     AssetRef  `json:"head"`
	Previous *AssetRef `json:"previous,omitempty"`
}

type AnchorPage struct {
	Schema  int           `json:"schema"`
	Epoch   uint32        `json:"epoch"`
	Page    uint64        `json:"page"`
	Entries []AnchorEntry `json:"entries"`
	Back    []PageRef     `json:"back"`
}

type AnchorEntry struct {
	Ordinal     uint64    `json:"ordinal"`
	Version     string    `json:"version"`
	Metadata    string    `json:"metadata,omitempty"`
	Manifest    string    `json:"manifest,omitempty"`
	AnchorsFrom uint64    `json:"anchorsFrom"`
	PublishedAt time.Time `json:"publishedAt"`
}

type PageRef struct {
	Page uint64   `json:"page"`
	Ref  AssetRef `json:"ref"`
}

func (root DiscoveryRoot) Validate() error {
	if root.Format != 1 || root.Epoch == 0 {
		return errors.New("release: unsupported discovery root format or epoch")
	}
	if err := root.Head.Validate(); err != nil {
		return err
	}
	if root.Head.Name != UpdateMetadataAssetName {
		return errors.New("release: root head must name update metadata")
	}
	if root.Epoch == 1 && root.Previous != nil || root.Epoch > 1 && root.Previous == nil {
		return errors.New("release: root has an invalid previous epoch reference")
	}
	if root.Previous != nil {
		return validatePreviousRoot(root)
	}
	return nil
}

func validatePreviousRoot(root DiscoveryRoot) error {
	if err := root.Previous.Validate(); err != nil {
		return err
	}
	order, _ := CompareVersions(root.Previous.Tag, root.Head.Tag)
	if root.Previous.Name != DiscoveryRootAssetName || order >= 0 {
		return errors.New("release: previous root must precede the head")
	}
	return nil
}

func EncodeDiscoveryRoot(root DiscoveryRoot) ([]byte, error) {
	if err := root.Validate(); err != nil {
		return nil, err
	}
	return encodeDiscovery(root, MaxDiscoveryRootBytes)
}

func DecodeDiscoveryRoot(data []byte) (DiscoveryRoot, error) {
	var root DiscoveryRoot
	if err := decodeDiscovery(data, MaxDiscoveryRootBytes, &root); err != nil {
		return root, err
	}
	return root, root.Validate()
}

// BuildAnchorPage requires locations of already published pages. The publisher
// adds its current anchor only when building a subsequent release's page.
func BuildAnchorPage(epoch uint32, page uint64, entries []AnchorEntry, previous map[uint64]AssetRef, publishedBy string) (AnchorPage, error) {
	result := AnchorPage{Schema: 1, Epoch: epoch, Page: page, Entries: entries}
	for distance := uint64(1); distance <= page; distance *= 2 {
		result.Back = append(result.Back, PageRef{Page: page - distance, Ref: previous[page-distance]})
		if distance > page/2 {
			break
		}
	}
	_, err := EncodeAnchorPage(result, publishedBy)
	return result, err
}

func EncodeAnchorPage(page AnchorPage, publishedBy string) ([]byte, error) {
	if err := page.validate(publishedBy); err != nil {
		return nil, err
	}
	return encodeDiscovery(page, MaxAnchorPageBytes)
}

// DecodeAnchorPage checks the exact bytes before reading the authenticated index.
func DecodeAnchorPage(data []byte, ref AssetRef) (AnchorPage, error) {
	var page AnchorPage
	if err := ref.Validate(); err != nil {
		return page, err
	}
	if !matchesDigest(data, ref.SHA256) {
		return page, errors.New("release: anchor page SHA-256 differs from its reference")
	}
	if err := decodeDiscovery(data, MaxAnchorPageBytes, &page); err != nil {
		return page, err
	}
	return page, page.validate(ref.Tag)
}

func (page AnchorPage) validate(publishedBy string) error {
	if page.Schema != 1 || page.Epoch == 0 || page.Page > (^uint64(0))/AnchorsPerPage {
		return errors.New("release: invalid anchor page schema, epoch or number")
	}
	if err := validateMetadataVersion(publishedBy); err != nil {
		return err
	}
	if len(page.Entries) > AnchorsPerPage || len(page.Entries) == 0 && page.Page != 0 || len(page.Back) > MaxPageBackrefs {
		return errors.New("release: invalid anchor page entry or back reference count")
	}
	for index, entry := range page.Entries {
		if err := validateAnchorEntry(entry, page.Page*AnchorsPerPage+uint64(index), publishedBy); err != nil {
			return err
		}
		if index > 0 && !anchorsIncrease(page.Entries[index-1], entry) {
			return errors.New("release: anchor entries do not increase monotonically")
		}
	}
	return page.validateBackrefs(publishedBy)
}

func validateAnchorEntry(entry AnchorEntry, ordinal uint64, publishedBy string) error {
	if err := validateMetadataVersion(entry.Version); err != nil {
		return err
	}
	order, _ := CompareVersions(entry.Version, publishedBy)
	if entry.Ordinal != ordinal || entry.AnchorsFrom > ordinal || order >= 0 {
		return errors.New("release: anchor ordinal, direct range or publication order is invalid")
	}
	if !validDigest(entry.Metadata) && !validDigest(entry.Manifest) || entry.Metadata != "" && !validDigest(entry.Metadata) || entry.Manifest != "" && !validDigest(entry.Manifest) {
		return errors.New("release: anchor lacks a valid metadata or seed manifest digest")
	}
	if entry.PublishedAt.IsZero() || entry.PublishedAt.Year() < 1 || entry.PublishedAt.Year() > 9999 {
		return errors.New("release: invalid anchor publication time")
	}
	return nil
}

func anchorsIncrease(previous, next AnchorEntry) bool {
	order, _ := CompareVersions(previous.Version, next.Version)
	return order < 0 && previous.AnchorsFrom <= next.AnchorsFrom && !next.PublishedAt.Before(previous.PublishedAt)
}

func (page AnchorPage) validateBackrefs(publishedBy string) error {
	index := 0
	for distance := uint64(1); distance <= page.Page; distance *= 2 {
		if index >= len(page.Back) || page.Back[index].Page != page.Page-distance {
			return errors.New("release: anchor back references must have power-of-two distances")
		}
		ref := page.Back[index].Ref
		if err := ref.Validate(); err != nil {
			return err
		}
		order, _ := CompareVersions(ref.Tag, publishedBy)
		if order > 0 {
			return errors.New("release: anchor back reference tag must not follow the page publisher")
		}
		index++
		if distance > page.Page/2 {
			break
		}
	}
	if index != len(page.Back) {
		return errors.New("release: extra anchor back reference")
	}
	return nil
}

func encodeDiscovery(value any, limit int) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("release: encode discovery: %w", err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("release: discovery document exceeds %d bytes", limit)
	}
	return data, nil
}

func decodeDiscovery(data []byte, limit int, value any) error {
	if len(data) > limit {
		return fmt.Errorf("release: discovery document exceeds %d bytes", limit)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("release: decode discovery: %w", err)
	}
	return nil
}

func matchesDigest(data []byte, digest string) bool {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == digest
}
