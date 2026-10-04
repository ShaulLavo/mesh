package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

const (
	bridgeReleaseLimit = 32
	bridgeTimeout      = 10 * time.Second
	OfficialHistoryAPI = "https://api.github.com/repos/ShaulLavo/mesh/releases?per_page=32"
)

type TransitionReceipt struct {
	Schema                       int      `json:"schema"`
	Platform                     Platform `json:"platform"`
	FromDigest                   string   `json:"fromDigest"`
	ToDigest                     string   `json:"toDigest"`
	StateReadMin                 int      `json:"stateReadMin"`
	StateReadMax                 int      `json:"stateReadMax"`
	StateWrite                   int      `json:"stateWrite"`
	WorkerMin                    int      `json:"workerMin"`
	WorkerMax                    int      `json:"workerMax"`
	WorkerWrite                  int      `json:"workerWrite"`
	JournalVersion               int      `json:"journalVersion"`
	RetainedOpenedCandidateState bool     `json:"retainedOpenedCandidateState"`
	SessionsPreserved            bool     `json:"sessionsPreserved"`
	RecoveryRecordsPreserved     bool     `json:"recoveryRecordsPreserved"`
}

// Bridge finds one published intermediate with verified receipts for both hops.
// It reads a single bounded history page and never downloads executables.
func (c Client) Bridge(ctx context.Context, build Build, target Manifest) (Manifest, error) {
	if build.Modified {
		return Manifest{}, errors.New("release: modified installations require explicit handling")
	}
	if _, err := parseVersion(build.Version); err != nil {
		return Manifest{}, err
	}
	artifact, err := target.Artifact(build.Platform)
	if err != nil {
		return Manifest{}, err
	}
	if target.Allows(build) == nil {
		return Manifest{}, errors.New("release: installation already has a direct tested path")
	}
	ctx, cancel := context.WithTimeout(ctx, bridgeTimeout)
	defer cancel()
	base, client, err := c.normalized()
	if err != nil {
		return Manifest{}, err
	}
	tags, err := c.bridgeTags(ctx, client, base, build.Version, target.Version)
	if err != nil {
		return Manifest{}, err
	}
	var failures []error
	for _, tag := range tags {
		hop, err := c.verifiedBridge(ctx, build, target, artifact, tag)
		if err == nil {
			return hop, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", tag, err))
		if ctx.Err() != nil {
			break
		}
	}
	return Manifest{}, errors.Join(append([]error{errors.New("release: no verified two-hop path within the release search window")}, failures...)...)
}

func (c Client) bridgeTags(ctx context.Context, client *http.Client, base, installed, target string) ([]string, error) {
	address := c.HistoryAPI
	if address == "" && base == OfficialBaseURL {
		address = OfficialHistoryAPI
	}
	if address == "" {
		return nil, errors.New("release: release history is unavailable for this origin")
	}
	data, err := downloadBytes(ctx, client, address, maximumManifest)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("release: decode release history: %w", err)
	}
	entries = entries[:min(len(entries), bridgeReleaseLimit)]
	seen := make(map[string]bool, len(entries))
	var tags []string
	for _, entry := range entries {
		lower, lowerErr := CompareVersions(entry.Tag, installed)
		upper, upperErr := CompareVersions(entry.Tag, target)
		if entry.Draft || entry.Prerelease || seen[entry.Tag] || lowerErr != nil || upperErr != nil || lower <= 0 || upper >= 0 {
			continue
		}
		seen[entry.Tag] = true
		tags = append(tags, entry.Tag)
	}
	sort.Slice(tags, func(i, j int) bool { order, _ := CompareVersions(tags[i], tags[j]); return order < 0 })
	return tags, nil
}

func (c Client) verifiedBridge(ctx context.Context, build Build, target Manifest, targetArtifact Artifact, tag string) (Manifest, error) {
	hop, err := c.Manifest(ctx, tag)
	if err != nil {
		return Manifest{}, err
	}
	if hop.Compatibility.JournalVersion != CurrentJournalVersion || target.Compatibility.JournalVersion != CurrentJournalVersion {
		return Manifest{}, errors.New("release: bridge requires an unsupported installation journal format")
	}
	if err := hop.Allows(build); err != nil {
		return Manifest{}, err
	}
	artifact, err := hop.Artifact(build.Platform)
	if err != nil {
		return Manifest{}, err
	}
	next := Build{Version: hop.Version, Commit: hop.Commit, Digest: artifact.BinarySHA256, Platform: build.Platform,
		StateVersion: hop.Compatibility.StateWrite, WorkerProtocol: hop.Compatibility.WorkerWrite, UpdateProtocol: CurrentUpdateProtocol}
	if err := target.Allows(next); err != nil {
		return Manifest{}, err
	}
	if err := c.verifyBridgeTransition(ctx, hop, build, artifact); err != nil {
		return Manifest{}, err
	}
	if err := c.verifyBridgeTransition(ctx, target, next, targetArtifact); err != nil {
		return Manifest{}, err
	}
	return hop, nil
}

func (c Client) verifyBridgeTransition(ctx context.Context, manifest Manifest, build Build, artifact Artifact) error {
	var transition Transition
	for _, candidate := range manifest.Compatibility.Transitions {
		if candidate.Platform == build.Platform && candidate.FromDigest == build.Digest && candidate.ToDigest == artifact.BinarySHA256 {
			transition = candidate
			break
		}
	}
	if transition.Proof == "" {
		return errors.New("release: exact bridge transition is absent")
	}
	base, client, err := c.normalized()
	if err != nil {
		return err
	}
	address, err := releaseURL(base, manifest.Version)
	if err != nil {
		return err
	}
	data, err := downloadBytes(ctx, client, address+"/"+transition.Proof+".json", maximumManifest)
	if err != nil {
		return err
	}
	proof, err := VerifyTransitionReceipt(data, transition, manifest.Compatibility)
	if err != nil {
		return err
	}
	if build.StateVersion < proof.StateReadMin || build.StateVersion > proof.StateReadMax || build.WorkerProtocol < proof.WorkerMin || build.WorkerProtocol > proof.WorkerMax {
		return errors.New("release: source capabilities are outside the exact receipt's tested range")
	}
	return nil
}

// VerifyTransitionReceipt applies the publication checks to content-addressed evidence.
func VerifyTransitionReceipt(data []byte, transition Transition, compatibility Compatibility) (TransitionReceipt, error) {
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != transition.Proof {
		return TransitionReceipt{}, errors.New("release: transition receipt SHA-256 differs from its manifest")
	}
	var proof TransitionReceipt
	if err := readReceiptJSON(data, &proof); err != nil {
		return proof, err
	}
	if proof.Schema != 1 || proof.Platform != transition.Platform || proof.FromDigest != transition.FromDigest || proof.ToDigest != transition.ToDigest {
		return proof, errors.New("release: transition receipt identity differs from its manifest")
	}
	if proof.StateReadMin <= 0 || proof.StateReadMin > compatibility.StateReadMax || proof.StateReadMax != compatibility.StateReadMax || proof.StateWrite != compatibility.StateWrite {
		return proof, errors.New("release: transition receipt state evidence differs from declared compatibility")
	}
	if proof.WorkerMin <= 0 || proof.WorkerMin > compatibility.WorkerMax || proof.WorkerMax != compatibility.WorkerMax || proof.WorkerWrite != compatibility.WorkerWrite || proof.JournalVersion != compatibility.JournalVersion {
		return proof, errors.New("release: transition receipt protocol evidence differs from declared compatibility")
	}
	if !proof.RetainedOpenedCandidateState || !proof.SessionsPreserved || !proof.RecoveryRecordsPreserved {
		return proof, errors.New("release: transition receipt did not pass every rollback check")
	}
	return proof, nil
}

func readReceiptJSON(data []byte, proof *TransitionReceipt) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(proof); err != nil {
		return fmt.Errorf("release: decode transition receipt: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("release: trailing transition receipt data")
	}
	return nil
}
