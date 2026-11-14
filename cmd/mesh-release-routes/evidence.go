package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/release"
)

const manifestLimit = 512 << 10

type retainedAsset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type publishedEvidence struct {
	Tag           string                  `json:"tag"`
	PublishedAt   time.Time               `json:"publishedAt"`
	Assets        []retainedAsset         `json:"assets"`
	Manifest      release.Manifest        `json:"-"`
	ManifestBytes []byte                  `json:"-"`
	Metadata      *release.UpdateMetadata `json:"-"`
	MetadataBytes []byte                  `json:"-"`
}

func sum(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func loadHistory(directory string, inventory []publishedEvidence) ([]publishedEvidence, error) {
	slices.SortFunc(inventory, func(left, right publishedEvidence) int { return compareTags(left.Tag, right.Tag) })
	for index := range inventory {
		if err := loadPublished(directory, &inventory[index]); err != nil {
			return nil, err
		}
		if index > 0 && (compareTags(inventory[index-1].Tag, inventory[index].Tag) >= 0 || inventory[index].PublishedAt.Before(inventory[index-1].PublishedAt)) {
			return nil, errors.New("publisher: release history must increase in version and publication time")
		}
	}
	return inventory, nil
}

func loadPublished(directory string, item *publishedEvidence) error {
	if item.PublishedAt.IsZero() {
		return fmt.Errorf("publisher: %s lacks a publication date", item.Tag)
	}
	if _, err := release.CompareVersions(item.Tag, item.Tag); err != nil {
		return fmt.Errorf("publisher: invalid history tag: %w", err)
	}
	ref, exists := item.asset("mesh-release.json")
	if !exists {
		if _, signed := item.asset(release.UpdateMetadataAssetName); signed {
			return errors.New("publisher: published update metadata lacks its retained manifest")
		}
		return nil
	}
	data, err := readAsset(directory, ref, manifestLimit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &item.Manifest); err != nil {
		return fmt.Errorf("publisher: decode %s manifest: %w", item.Tag, err)
	}
	if err := item.Manifest.Validate(); err != nil {
		return fmt.Errorf("publisher: validate %s manifest: %w", item.Tag, err)
	}
	if item.Manifest.Version != item.Tag {
		return fmt.Errorf("publisher: %s manifest names another release", item.Tag)
	}
	item.ManifestBytes = data
	if err := checkArtifactRetention(*item); err != nil {
		return err
	}
	return loadPublishedMetadata(directory, item)
}

func checkArtifactRetention(item publishedEvidence) error {
	for _, artifact := range item.Manifest.Artifacts {
		ref, exists := item.asset(artifact.Archive)
		if !exists || ref.SHA256 != artifact.SHA256 {
			return fmt.Errorf("publisher: retained archive missing or changed: %s/%s", item.Tag, artifact.Archive)
		}
	}
	return nil
}

func loadPublishedMetadata(directory string, item *publishedEvidence) error {
	ref, exists := item.asset(release.UpdateMetadataAssetName)
	if !exists {
		return nil
	}
	data, err := readAsset(directory, ref, release.MaxMetadataBytes)
	if err != nil {
		return err
	}
	metadata, err := release.DecodeUpdateMetadata(data)
	if err != nil {
		return fmt.Errorf("publisher: decode %s metadata: %w", item.Tag, err)
	}
	if metadata.Manifest != sum(item.ManifestBytes) || metadata.Version != item.Tag {
		return fmt.Errorf("publisher: %s metadata does not bind its exact manifest", item.Tag)
	}
	item.Metadata, item.MetadataBytes = &metadata, data
	return nil
}

func (item publishedEvidence) asset(name string) (release.AssetRef, bool) {
	for _, asset := range item.Assets {
		if asset.Name == name {
			return release.AssetRef{Tag: item.Tag, Name: name, SHA256: asset.SHA256}, true
		}
	}
	return release.AssetRef{}, false
}

func readAsset(directory string, ref release.AssetRef, limit int) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, fmt.Errorf("publisher: invalid asset reference: %w", err)
	}
	data, err := readLocal(filepath.Join(directory, ref.Tag, ref.Name), limit)
	if err != nil {
		return nil, err
	}
	if sum(data) != ref.SHA256 {
		return nil, fmt.Errorf("publisher: retained %s/%s changed", ref.Tag, ref.Name)
	}
	return data, nil
}

func verifyHistoricalEdge(directory string, source, target publishedEvidence, platform release.Platform) (release.Transition, error) {
	from, err := source.Manifest.Artifact(platform)
	if err != nil {
		return release.Transition{}, fmt.Errorf("publisher: source artifact: %w", err)
	}
	to, err := target.Manifest.Artifact(platform)
	if err != nil {
		return release.Transition{}, fmt.Errorf("publisher: target artifact: %w", err)
	}
	transition, exists := findTransition(target.Manifest, from.BinarySHA256, to.BinarySHA256, platform)
	if !exists {
		return transition, errors.New("publisher: no declared transition")
	}
	ref := release.AssetRef{Tag: target.Tag, Name: transition.Proof + ".json", SHA256: transition.Proof}
	data, err := readAsset(directory, ref, 1<<20)
	if err != nil {
		return transition, err
	}
	var receipt release.TransitionReceipt
	if compareTags(target.Tag, "v0.1.232") < 0 {
		receipt, err = verifyPre232TransitionReceipt(data, transition, target.Manifest.Compatibility)
	} else {
		receipt, err = release.VerifyTransitionReceipt(data, transition, target.Manifest.Compatibility)
	}
	if err != nil {
		return transition, fmt.Errorf("publisher: %s -> %s receipt: %w", source.Tag, target.Tag, err)
	}
	compatibility := source.Manifest.Compatibility
	if compatibility.StateWrite < receipt.StateReadMin || compatibility.StateWrite > receipt.StateReadMax || compatibility.WorkerWrite < receipt.WorkerMin || compatibility.WorkerWrite > receipt.WorkerMax {
		return transition, errors.New("publisher: source capabilities exceed the receipt's tested range")
	}
	declared := target.Manifest.Compatibility
	if compatibility.StateWrite < declared.StateReadMin || compatibility.StateWrite > declared.StateReadMax || compatibility.WorkerWrite < declared.WorkerMin || compatibility.WorkerWrite > declared.WorkerMax {
		return transition, errors.New("publisher: source capabilities exceed the target's declared range")
	}
	return transition, nil
}

func findTransition(manifest release.Manifest, from, to string, platform release.Platform) (release.Transition, bool) {
	for _, transition := range manifest.Compatibility.Transitions {
		if transition.Platform == platform && transition.FromDigest == from && transition.ToDigest == to {
			return transition, true
		}
	}
	return release.Transition{}, false
}

type pre232Receipt struct {
	Schema                       int              `json:"schema"`
	Platform                     release.Platform `json:"platform"`
	FromDigest                   string           `json:"fromDigest"`
	ToDigest                     string           `json:"toDigest"`
	StateReadMin                 int              `json:"stateReadMin"`
	StateReadMax                 int              `json:"stateReadMax"`
	StateWrite                   int              `json:"stateWrite"`
	WorkerMin                    int              `json:"workerMin"`
	WorkerMax                    int              `json:"workerMax"`
	WorkerWrite                  int              `json:"workerWrite"`
	JournalVersion               int              `json:"journalVersion"`
	RetainedOpenedCandidateState bool             `json:"retainedOpenedCandidateState"`
	SessionsPreserved            bool             `json:"sessionsPreserved"`
	RecoveryRecordsPreserved     bool             `json:"recoveryRecordsPreserved"`
}

// The pre232 contract used retainedOpenedCandidateState. Its bytes establish
// historical forward connectivity, without a current reader or rollback grant.
func verifyPre232TransitionReceipt(data []byte, transition release.Transition, compatibility release.Compatibility) (release.TransitionReceipt, error) {
	var legacy pre232Receipt
	if sum(data) != transition.Proof {
		return release.TransitionReceipt{}, errors.New("publisher: pre232 receipt digest mismatch")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err != nil {
		return release.TransitionReceipt{}, fmt.Errorf("publisher: decode pre232 receipt: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return release.TransitionReceipt{}, errors.New("publisher: trailing pre232 receipt data")
	}
	if err := legacy.validate(transition, compatibility); err != nil {
		return release.TransitionReceipt{}, err
	}
	return release.TransitionReceipt{Schema: legacy.Schema, Platform: legacy.Platform, FromDigest: legacy.FromDigest, ToDigest: legacy.ToDigest, StateReadMin: legacy.StateReadMin, StateReadMax: legacy.StateReadMax, StateWrite: legacy.StateWrite, WorkerMin: legacy.WorkerMin, WorkerMax: legacy.WorkerMax, WorkerWrite: legacy.WorkerWrite, JournalVersion: legacy.JournalVersion, SessionsPreserved: legacy.SessionsPreserved, RecoveryRecordsPreserved: legacy.RecoveryRecordsPreserved}, nil
}

func (receipt pre232Receipt) validate(transition release.Transition, compatibility release.Compatibility) error {
	if receipt.Schema != 1 || receipt.Platform != transition.Platform || receipt.FromDigest != transition.FromDigest || receipt.ToDigest != transition.ToDigest {
		return errors.New("publisher: pre232 receipt identity mismatch")
	}
	if receipt.StateReadMin <= 0 || receipt.StateReadMin > compatibility.StateReadMax || receipt.StateReadMax != compatibility.StateReadMax || receipt.StateWrite != compatibility.StateWrite {
		return errors.New("publisher: pre232 state evidence differs from declared compatibility")
	}
	if receipt.WorkerMin <= 0 || receipt.WorkerMin > compatibility.WorkerMax || receipt.WorkerMax != compatibility.WorkerMax || receipt.WorkerWrite != compatibility.WorkerWrite || receipt.JournalVersion != compatibility.JournalVersion {
		return errors.New("publisher: pre232 protocol evidence differs from declared compatibility")
	}
	if !receipt.RetainedOpenedCandidateState || !receipt.SessionsPreserved || !receipt.RecoveryRecordsPreserved {
		return errors.New("publisher: pre232 receipt did not pass its historical forward checks")
	}
	return nil
}

func checkRetained(expected, current []publishedEvidence) error {
	byTag := make(map[string]publishedEvidence, len(current))
	for _, item := range current {
		byTag[item.Tag] = item
	}
	for _, item := range expected {
		if err := compareRetainedRelease(item, byTag[item.Tag]); err != nil {
			return err
		}
	}
	return nil
}

func compareRetainedRelease(expected, current publishedEvidence) error {
	for _, asset := range expected.Assets {
		ref, exists := current.asset(asset.Name)
		if !exists || asset.SHA256 == "" || ref.SHA256 != asset.SHA256 {
			return fmt.Errorf("publisher: retained asset missing or changed: %s/%s", expected.Tag, asset.Name)
		}
	}
	return nil
}

func writePublisherFile(directory, name string, data []byte) error {
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("publisher: create output: %w", err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fmt.Errorf("publisher: open output directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(name, data, 0o600); err != nil {
		return fmt.Errorf("publisher: write %s: %w", name, err)
	}
	return nil
}
