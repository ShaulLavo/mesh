// Package release resolves and verifies immutable Mesh releases.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const (
	ManifestSchema        = 1
	CurrentStateVersion   = 7
	CurrentWorkerProtocol = 1
	CurrentUpdateProtocol = 1
	CurrentJournalVersion = 1
)

var supportedPlatforms = []Platform{
	{OS: "darwin", Arch: "arm64"},
	{OS: "linux", Arch: "amd64"},
	{OS: "linux", Arch: "arm64"},
}

// Platform identifies one release build target.
type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// CurrentPlatform identifies the process build target.
func CurrentPlatform() Platform { return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// Build describes the executable that is running, rather than a file which may
// have replaced it on disk since process start.
type Build struct {
	Version        string   `json:"version"`
	Commit         string   `json:"commit"`
	Digest         string   `json:"digest"`
	Platform       Platform `json:"platform"`
	Modified       bool     `json:"modified"`
	StateVersion   int      `json:"stateVersion"`
	WorkerProtocol int      `json:"workerProtocol"`
	UpdateProtocol int      `json:"updateProtocol"`
}

// Artifact binds one supported platform to its archive and executable hashes.
type Artifact struct {
	Platform     Platform `json:"platform"`
	Archive      string   `json:"archive"`
	SHA256       string   `json:"sha256"`
	BinarySHA256 string   `json:"binarySha256"`
}

// Transition records an exact rollback-compatibility proof between executables.
type Transition struct {
	FromDigest string   `json:"fromDigest"`
	ToDigest   string   `json:"toDigest"`
	Platform   Platform `json:"platform"`
	Proof      string   `json:"proof"`
}

// Compatibility describes state and worker protocols accepted by a release.
type Compatibility struct {
	StateReadMin   int          `json:"stateReadMin"`
	StateReadMax   int          `json:"stateReadMax"`
	StateWrite     int          `json:"stateWrite"`
	WorkerMin      int          `json:"workerMin"`
	WorkerMax      int          `json:"workerMax"`
	WorkerWrite    int          `json:"workerWrite"`
	JournalVersion int          `json:"journalVersion"`
	Transitions    []Transition `json:"transitions"`
}

// Manifest is the complete, immutable release descriptor.
type Manifest struct {
	Schema        int           `json:"schema"`
	Version       string        `json:"version"`
	Commit        string        `json:"commit"`
	Artifacts     []Artifact    `json:"artifacts"`
	Compatibility Compatibility `json:"compatibility"`
}

// Validate rejects incomplete or ambiguous release metadata.
func (m Manifest) Validate() error {
	if m.Schema != ManifestSchema {
		return fmt.Errorf("release: manifest schema %d is unsupported", m.Schema)
	}
	if _, err := parseVersion(m.Version); err != nil {
		return err
	}
	if !validCommit(m.Commit) {
		return fmt.Errorf("release: commit %q is not a lowercase 40-character SHA", m.Commit)
	}
	if err := validateCompatibility(m.Compatibility); err != nil {
		return err
	}
	if err := validateArtifacts(m.Artifacts); err != nil {
		return err
	}
	return validateTransitionTargets(m)
}

// Digest returns the SHA-256 of the manifest's canonical JSON representation.
func (m Manifest) Digest() string {
	contents, _ := json.Marshal(m)
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

// Artifact returns the sole artifact for platform.
func (m Manifest) Artifact(platform Platform) (Artifact, error) {
	if err := m.Validate(); err != nil {
		return Artifact{}, err
	}
	for _, artifact := range m.Artifacts {
		if artifact.Platform == platform {
			return artifact, nil
		}
	}
	return Artifact{}, fmt.Errorf("release: manifest %s has no artifact for %s", m.Version, platform)
}

// Allows verifies that build has an exact tested path to this release.
func (m Manifest) Allows(build Build) error {
	artifact, err := m.Artifact(build.Platform)
	if err != nil {
		return err
	}
	if !validDigest(build.Digest) {
		return fmt.Errorf("release: current build digest %q is invalid", build.Digest)
	}
	if build.Digest == artifact.BinarySHA256 {
		return nil
	}
	if build.StateVersion < m.Compatibility.StateReadMin || build.StateVersion > m.Compatibility.StateReadMax {
		return fmt.Errorf("release: state version %d is outside target read range %d..%d", build.StateVersion, m.Compatibility.StateReadMin, m.Compatibility.StateReadMax)
	}
	if build.WorkerProtocol < m.Compatibility.WorkerMin || build.WorkerProtocol > m.Compatibility.WorkerMax {
		return fmt.Errorf("release: worker protocol %d is outside target range %d..%d", build.WorkerProtocol, m.Compatibility.WorkerMin, m.Compatibility.WorkerMax)
	}
	if hasTransition(m.Compatibility.Transitions, build.Digest, artifact) {
		return nil
	}
	return fmt.Errorf("release: no tested %s transition from %s to %s", build.Platform, build.Digest, artifact.BinarySHA256)
}

// CompareVersions compares two stable Mesh release tags.
func CompareVersions(a, b string) (int, error) {
	left, err := parseComparableVersion(a)
	if err != nil {
		return 0, err
	}
	right, err := parseComparableVersion(b)
	if err != nil {
		return 0, err
	}
	for index := range left {
		if left[index] < right[index] {
			return -1, nil
		}
		if left[index] > right[index] {
			return 1, nil
		}
	}
	return 0, nil
}

func parseComparableVersion(value string) ([3]uint64, error) {
	stable, metadata, hasMetadata := strings.Cut(value, "+")
	if !hasMetadata {
		return parseVersion(stable)
	}
	if !validBuildMetadata(metadata) || strings.Contains(metadata, "+") {
		return [3]uint64{}, fmt.Errorf("release: version %q has invalid build metadata", value)
	}
	return parseVersion(stable)
}

func validBuildMetadata(value string) bool {
	if value == "" {
		return false
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		for _, character := range identifier {
			if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func validateArtifacts(artifacts []Artifact) error {
	if len(artifacts) != len(supportedPlatforms) {
		return fmt.Errorf("release: manifest has %d artifacts, want %d", len(artifacts), len(supportedPlatforms))
	}
	seen := make(map[Platform]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if !supportedPlatform(artifact.Platform) {
			return fmt.Errorf("release: unsupported artifact platform %s", artifact.Platform)
		}
		if _, exists := seen[artifact.Platform]; exists {
			return fmt.Errorf("release: duplicate artifact platform %s", artifact.Platform)
		}
		if artifact.Archive != archiveName(artifact.Platform) {
			return fmt.Errorf("release: artifact for %s is named %q, want %q", artifact.Platform, artifact.Archive, archiveName(artifact.Platform))
		}
		if !validDigest(artifact.SHA256) || !validDigest(artifact.BinarySHA256) {
			return fmt.Errorf("release: artifact for %s has an invalid SHA-256", artifact.Platform)
		}
		seen[artifact.Platform] = struct{}{}
	}
	return nil
}

func validateCompatibility(compatibility Compatibility) error {
	if compatibility.StateReadMin <= 0 || compatibility.StateReadMax < compatibility.StateReadMin {
		return errors.New("release: invalid state read range")
	}
	if compatibility.StateWrite < compatibility.StateReadMin || compatibility.StateWrite > compatibility.StateReadMax {
		return errors.New("release: state write version is outside its read range")
	}
	if compatibility.WorkerMin <= 0 || compatibility.WorkerMax < compatibility.WorkerMin {
		return errors.New("release: invalid worker protocol range")
	}
	if compatibility.WorkerWrite < compatibility.WorkerMin || compatibility.WorkerWrite > compatibility.WorkerMax {
		return errors.New("release: worker write protocol is outside its accepted range")
	}
	if compatibility.JournalVersion <= 0 {
		return errors.New("release: journal version must be positive")
	}
	return validateTransitions(compatibility.Transitions)
}

func validateTransitions(transitions []Transition) error {
	seen := make(map[string]struct{}, len(transitions))
	for _, transition := range transitions {
		if !validDigest(transition.FromDigest) || !validDigest(transition.ToDigest) {
			return errors.New("release: transition has an invalid executable digest")
		}
		if !supportedPlatform(transition.Platform) {
			return fmt.Errorf("release: transition has unsupported platform %s", transition.Platform)
		}
		if !validDigest(transition.Proof) {
			return errors.New("release: transition proof is not a SHA-256 digest")
		}
		key := transition.Platform.String() + ":" + transition.FromDigest + ":" + transition.ToDigest
		if _, exists := seen[key]; exists {
			return fmt.Errorf("release: duplicate transition %s", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateTransitionTargets(manifest Manifest) error {
	artifacts := make(map[Platform]string, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		artifacts[artifact.Platform] = artifact.BinarySHA256
	}
	for _, transition := range manifest.Compatibility.Transitions {
		if transition.ToDigest != artifacts[transition.Platform] {
			return fmt.Errorf("release: transition target %s does not match the %s artifact", transition.ToDigest, transition.Platform)
		}
	}
	return nil
}

func hasTransition(transitions []Transition, from string, artifact Artifact) bool {
	for _, transition := range transitions {
		if transition.Platform == artifact.Platform && transition.FromDigest == from && transition.ToDigest == artifact.BinarySHA256 {
			return true
		}
	}
	return false
}

func supportedPlatform(platform Platform) bool {
	index := sort.Search(len(supportedPlatforms), func(index int) bool {
		return supportedPlatforms[index].String() >= platform.String()
	})
	return index < len(supportedPlatforms) && supportedPlatforms[index] == platform
}

func archiveName(platform Platform) string {
	return "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz"
}

func validCommit(value string) bool {
	return len(value) == 40 && validLowerHex(value)
}

func validDigest(value string) bool {
	return len(value) == sha256.Size*2 && validLowerHex(value)
}

func validLowerHex(value string) bool {
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return true
}

func parseVersion(value string) ([3]uint64, error) {
	var parsed [3]uint64
	if !strings.HasPrefix(value, "v") {
		return parsed, fmt.Errorf("release: version %q must be vMAJOR.MINOR.PATCH", value)
	}
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != len(parsed) {
		return parsed, fmt.Errorf("release: version %q must be vMAJOR.MINOR.PATCH", value)
	}
	for index, part := range parts {
		if part == "" || len(part) > 1 && part[0] == '0' {
			return parsed, fmt.Errorf("release: version %q is not canonical", value)
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return parsed, fmt.Errorf("release: version %q is invalid: %w", value, err)
		}
		parsed[index] = number
	}
	return parsed, nil
}
