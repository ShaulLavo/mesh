// Command manifest builds the immutable release descriptor from final archives
// and externally produced compatibility receipts.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/shaul/mesh/internal/release"
)

const maximumInputSize = 1 << 20

type options struct {
	dist          string
	version       string
	commit        string
	compatibility string
	proofs        string
	output        string
}

type receipt struct {
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

func main() {
	var config options
	flag.StringVar(&config.dist, "dist", "", "directory containing final release archives")
	flag.StringVar(&config.version, "version", "", "stable release tag")
	flag.StringVar(&config.commit, "commit", "", "exact source commit")
	flag.StringVar(&config.compatibility, "compatibility", "", "compatibility JSON produced by transition verification")
	flag.StringVar(&config.proofs, "proofs", "", "directory containing digest-named transition receipt JSON")
	flag.StringVar(&config.output, "output", "", "manifest output path")
	flag.Parse()
	if err := generate(config); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(config options) error {
	if config.dist == "" || config.version == "" || config.commit == "" || config.compatibility == "" || config.output == "" {
		return errors.New("manifest: -dist, -version, -commit, -compatibility, and -output are required")
	}
	compatibility, err := readCompatibility(config.compatibility)
	if err != nil {
		return err
	}
	artifacts, err := readArtifacts(config.dist)
	if err != nil {
		return err
	}
	manifest := release.Manifest{
		Schema: release.ManifestSchema, Version: config.version, Commit: config.commit,
		Artifacts: artifacts, Compatibility: compatibility,
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	if err := verifyProofs(config.proofs, compatibility); err != nil {
		return err
	}
	return writeManifest(config.output, manifest)
}

func readCompatibility(path string) (release.Compatibility, error) {
	var compatibility release.Compatibility
	if err := decodeFile(path, &compatibility); err != nil {
		return compatibility, fmt.Errorf("manifest: read compatibility: %w", err)
	}
	return compatibility, nil
}

func readArtifacts(directory string) ([]release.Artifact, error) {
	platforms := []release.Platform{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}
	artifacts := make([]release.Artifact, 0, len(platforms))
	for _, platform := range platforms {
		artifact, err := readArtifact(directory, platform)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

func readArtifact(directory string, platform release.Platform) (release.Artifact, error) {
	name := "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz"
	path := filepath.Join(directory, name)
	archiveDigest, err := hashFile(path)
	if err != nil {
		return release.Artifact{}, fmt.Errorf("manifest: hash %s: %w", name, err)
	}
	binaryDigest, err := hashArchiveBinary(path)
	if err != nil {
		return release.Artifact{}, fmt.Errorf("manifest: inspect %s: %w", name, err)
	}
	return release.Artifact{Platform: platform, Archive: name, SHA256: archiveDigest, BinarySHA256: binaryDigest}, nil
}

func hashArchiveBinary(path string) (string, error) {
	archive, err := os.Open(path) //nolint:gosec // path is a fixed archive name under the selected dist directory
	if err != nil {
		return "", err
	}
	defer archive.Close() //nolint:errcheck // hash result is authoritative
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return "", err
	}
	defer gzipReader.Close() //nolint:errcheck // reader has no pending writes
	tarReader := tar.NewReader(gzipReader)
	header, err := tarReader.Next()
	if err != nil {
		return "", err
	}
	if header.Name != "mesh" || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > 128<<20 {
		return "", errors.New("archive must contain one bounded regular file named mesh")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(tarReader, (128<<20)+1))
	if err != nil || written != header.Size {
		return "", errors.Join(err, fmt.Errorf("binary size is %d, want %d", written, header.Size))
	}
	if _, err := tarReader.Next(); !errors.Is(err, io.EOF) {
		return "", errors.New("archive contains more than one member")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyProofs(directory string, compatibility release.Compatibility) error {
	transitions := compatibility.Transitions
	if len(transitions) == 0 {
		return nil
	}
	if directory == "" {
		return errors.New("manifest: -proofs is required when compatibility declares transitions")
	}
	stateReadMin := compatibility.StateReadMax
	workerMin := compatibility.WorkerMax
	for _, transition := range transitions {
		proof, err := verifyProof(directory, transition, compatibility)
		if err != nil {
			return err
		}
		stateReadMin = min(stateReadMin, proof.StateReadMin)
		workerMin = min(workerMin, proof.WorkerMin)
	}
	if stateReadMin != compatibility.StateReadMin {
		return errors.New("manifest: transition proofs do not establish the declared state minimum")
	}
	if workerMin != compatibility.WorkerMin {
		return errors.New("manifest: transition proofs do not establish the declared worker minimum")
	}
	return nil
}

func verifyProof(directory string, transition release.Transition, compatibility release.Compatibility) (receipt, error) {
	path := filepath.Join(directory, transition.Proof+".json")
	digest, err := hashFile(path)
	if err != nil {
		return receipt{}, fmt.Errorf("manifest: read transition proof %s: %w", transition.Proof, err)
	}
	if digest != transition.Proof {
		return receipt{}, fmt.Errorf("manifest: transition proof file hashes to %s, want %s", digest, transition.Proof)
	}
	var proof receipt
	if err := decodeFile(path, &proof); err != nil {
		return receipt{}, fmt.Errorf("manifest: decode transition proof: %w", err)
	}
	if proof.Schema != 1 || proof.Platform != transition.Platform || proof.FromDigest != transition.FromDigest || proof.ToDigest != transition.ToDigest {
		return receipt{}, errors.New("manifest: transition proof identity does not match its transition")
	}
	if proof.StateReadMin <= 0 || proof.StateReadMin > compatibility.StateReadMax || proof.StateReadMax != compatibility.StateReadMax || proof.StateWrite != compatibility.StateWrite {
		return receipt{}, errors.New("manifest: transition proof state evidence differs from declared compatibility")
	}
	if proof.WorkerMin <= 0 || proof.WorkerMin > compatibility.WorkerMax || proof.WorkerMax != compatibility.WorkerMax || proof.WorkerWrite != compatibility.WorkerWrite || proof.JournalVersion != compatibility.JournalVersion {
		return receipt{}, errors.New("manifest: transition proof protocol evidence differs from declared compatibility")
	}
	if !proof.RetainedOpenedCandidateState || !proof.SessionsPreserved || !proof.RecoveryRecordsPreserved {
		return receipt{}, errors.New("manifest: transition proof did not pass every rollback check")
	}
	return proof, nil
}

func decodeFile(path string, destination any) error {
	file, err := os.Open(path) //nolint:gosec // caller explicitly selected release input
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // decode result is authoritative
	decoder := json.NewDecoder(io.LimitReader(file, maximumInputSize+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // caller explicitly selected release input
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck // hash result is authoritative
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeManifest(path string, manifest release.Manifest) error {
	contents, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".manifest-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck // best-effort cleanup after atomic publication
	if err := writeTemporary(temporary, contents); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func writeTemporary(file *os.File, contents []byte) error {
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path) //nolint:gosec // path is the output's parent directory
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck // sync result is authoritative
	return directory.Sync()
}
