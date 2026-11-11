package release

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	metadata "github.com/theupdateframework/go-tuf/v2/metadata"
	"github.com/theupdateframework/go-tuf/v2/metadata/trustedmetadata"
)

const trustedMaterialTarget = "trusted_root.json"

type witnessedRole struct {
	Name     string `json:"name"`
	Metadata []byte `json:"metadata"`
}

type materialWitness struct {
	Root     int64           `json:"root"`
	Snapshot []byte          `json:"snapshot"`
	Targets  []witnessedRole `json:"targets"`
}

type cachedMaterial struct {
	Material []byte          `json:"material"`
	Witness  materialWitness `json:"witness"`
}

type embeddedProvenance struct {
	Roots   [][]byte        `json:"roots"`
	Witness materialWitness `json:"witness"`
}

type materialOrder struct {
	Root     int64
	Snapshot int64
}

type authenticatedMaterial struct {
	contents []byte
	order    materialOrder
}

type rootHistory struct {
	chain  *trustedmetadata.TrustedMetadata
	roots  []*metadata.Metadata[metadata.RootType]
	latest []byte
}

func (trust TrustRoot) authenticatedBaseline(cache string) (authenticatedMaterial, *rootHistory, error) {
	var provenance embeddedProvenance
	if len(trust.provenance) != 0 {
		if err := json.Unmarshal(trust.provenance, &provenance); err != nil {
			return authenticatedMaterial{}, nil, fmt.Errorf("release: decode embedded trust provenance: %w", err)
		}
	}
	roots, err := loadTrustRoots(trust.BootstrapRoot, provenance.Roots, cache)
	if err != nil {
		return authenticatedMaterial{}, nil, err
	}
	if len(trust.provenance) == 0 {
		return authenticatedMaterial{contents: trust.Trusted}, roots, nil
	}
	baseline, err := authenticateMaterial(roots, cachedMaterial{Material: trust.Trusted, Witness: provenance.Witness})
	return baseline, roots, err
}

func loadTrustRoots(bootstrap []byte, embedded [][]byte, cache string) (*rootHistory, error) {
	chain, err := trustedmetadata.New(bootstrap)
	if err != nil {
		return nil, fmt.Errorf("release: parse pinned bootstrap root: %w", err)
	}
	history := &rootHistory{chain: chain, roots: []*metadata.Metadata[metadata.RootType]{chain.Root}, latest: bootstrap}
	for _, contents := range embedded {
		if err := history.accept(contents, history.chain.Root.Signed.Version+1); err != nil {
			return nil, err
		}
	}
	if cache == "" {
		return history, nil
	}
	paths, err := filepath.Glob(filepath.Join(cache, "roots", "*.root.json"))
	if err != nil {
		return nil, fmt.Errorf("release: locate saved trust roots: %w", err)
	}
	return history, history.loadSaved(paths)
}

func (history *rootHistory) loadSaved(paths []string) error {
	versions := make(map[int64]string, len(paths))
	for _, path := range paths {
		version, err := strconv.ParseInt(strings.TrimSuffix(filepath.Base(path), ".root.json"), 10, 64)
		if err != nil || version < 1 || filepath.Base(path) != strconv.FormatInt(version, 10)+".root.json" {
			return errors.New("release: invalid saved root filename")
		}
		versions[version] = path
	}
	ordered := make([]int64, 0, len(versions))
	for version := range versions {
		ordered = append(ordered, version)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, version := range ordered {
		contents, err := readTrustFile(versions[version])
		if err != nil {
			return err
		}
		if err := history.accept(contents, version); err != nil {
			return err
		}
	}
	return nil
}

func (history *rootHistory) accept(contents []byte, expected int64) error {
	parsed, err := metadata.Root().FromBytes(contents)
	if err != nil {
		return fmt.Errorf("release: parse saved root: %w", err)
	}
	version := parsed.Signed.Version
	if version != expected {
		return errors.New("release: saved root revision does not match its filename")
	}
	if version > history.chain.Root.Signed.Version {
		if _, err := history.chain.UpdateRoot(contents); err != nil {
			return fmt.Errorf("release: authenticate successive root: %w", err)
		}
		history.roots = append(history.roots, history.chain.Root)
		history.latest = contents
		return nil
	}
	index := version - history.roots[0].Signed.Version
	if index < 0 || index >= int64(len(history.roots)) {
		return errors.New("release: saved root predates pinned bootstrap")
	}
	trusted := history.roots[index]
	known, err := json.Marshal(trusted.Signed)
	if err != nil {
		return fmt.Errorf("release: encode authenticated root: %w", err)
	}
	want, err := json.Marshal(parsed.Signed)
	if err != nil {
		return fmt.Errorf("release: encode saved root: %w", err)
	}
	if !bytes.Equal(known, want) {
		return errors.New("release: saved root conflicts with embedded authenticated chain")
	}
	if err := trusted.VerifyDelegate(metadata.ROOT, parsed); err != nil {
		return fmt.Errorf("release: authenticate saved root signatures: %w", err)
	}
	return nil
}

// Historical authorization checks signatures and exact target bytes. It makes no freshness claim.
func authenticateMaterial(roots *rootHistory, saved cachedMaterial) (authenticatedMaterial, error) {
	parsed, err := parseMaterialWitness(saved.Witness, saved.Material)
	if err != nil {
		return authenticatedMaterial{}, err
	}
	if saved.Witness.Root < roots.roots[0].Signed.Version || saved.Witness.Root > roots.chain.Root.Signed.Version {
		return authenticatedMaterial{}, errors.New("release: material witness names an unauthenticated root revision")
	}
	for _, authority := range roots.roots {
		if authority.Signed.Version > saved.Witness.Root {
			break
		}
		if !parsed.authorizedBy(authority) {
			continue
		}
		// The earliest authorizing root is derived, never taken from the caller's revision claim.
		return authenticatedMaterial{contents: saved.Material, order: materialOrder{Root: authority.Signed.Version, Snapshot: parsed.snapshot.Signed.Version}}, nil
	}
	return authenticatedMaterial{}, errors.New("release: cached material has no authenticated signed metadata witness")
}

type parsedMaterialWitness struct {
	snapshot *metadata.Metadata[metadata.SnapshotType]
	top      *metadata.Metadata[metadata.TargetsType]
}

func (parsed parsedMaterialWitness) authorizedBy(authority *metadata.Metadata[metadata.RootType]) bool {
	return authority.VerifyDelegate(metadata.SNAPSHOT, parsed.snapshot) == nil && authority.VerifyDelegate(metadata.TARGETS, parsed.top) == nil
}

func parseMaterialWitness(witness materialWitness, contents []byte) (parsedMaterialWitness, error) {
	var result parsedMaterialWitness
	if len(witness.Targets) < 1 || len(witness.Targets) > 32 || witness.Targets[0].Name != metadata.TARGETS {
		return result, errors.New("release: missing or invalid material witness roles")
	}
	snapshot, err := metadata.Snapshot().FromBytes(witness.Snapshot)
	if err != nil {
		return result, fmt.Errorf("release: decode signed trust snapshot: %w", err)
	}
	if snapshot.Signed.Type != metadata.SNAPSHOT || snapshot.Signed.Version < 1 {
		return result, errors.New("release: invalid trust snapshot identity")
	}
	roles := make(map[string]*metadata.Metadata[metadata.TargetsType], len(witness.Targets))
	for _, role := range witness.Targets {
		if roles[role.Name] != nil {
			return result, errors.New("release: duplicate trust witness role")
		}
		current, err := authenticateWitnessRole(snapshot, role)
		if err != nil {
			return result, err
		}
		roles[role.Name] = current
	}
	result.top = roles[metadata.TARGETS]
	target, _, err := resolveWitnessTarget(roles, metadata.TARGETS, make(map[string]bool))
	if err != nil {
		return result, err
	}
	if target == nil || len(target.Hashes["sha256"]) != 32 || target.Length <= 0 {
		return result, errors.New("release: material witness has no SHA-256 target binding")
	}
	if err := target.VerifyLengthHashes(contents); err != nil {
		return result, fmt.Errorf("release: authenticated material target mismatch: %w", err)
	}
	result.snapshot = snapshot
	return result, nil
}

func authenticateWitnessRole(snapshot *metadata.Metadata[metadata.SnapshotType], role witnessedRole) (*metadata.Metadata[metadata.TargetsType], error) {
	if role.Name == "" {
		return nil, errors.New("release: invalid trust delegation")
	}
	current, err := metadata.Targets().FromBytes(role.Metadata)
	if err != nil {
		return nil, fmt.Errorf("release: decode signed trust targets: %w", err)
	}
	if current.Signed.Type != metadata.TARGETS || current.Signed.Version < 1 {
		return nil, errors.New("release: invalid trust targets identity")
	}
	meta := snapshot.Signed.Meta[role.Name+".json"]
	if meta == nil || meta.Version != current.Signed.Version {
		return nil, errors.New("release: trust snapshot does not bind role version")
	}
	if err := meta.VerifyLengthHashes(role.Metadata); err != nil {
		return nil, fmt.Errorf("release: signed trust role hash mismatch: %w", err)
	}
	return current, nil
}

func resolveWitnessTarget(roles map[string]*metadata.Metadata[metadata.TargetsType], name string, visited map[string]bool) (*metadata.TargetFiles, bool, error) {
	if visited[name] {
		return nil, false, nil
	}
	current := roles[name]
	if current == nil {
		return nil, false, errors.New("release: incomplete trust delegation witness")
	}
	visited[name] = true
	if target := current.Signed.Targets[trustedMaterialTarget]; target != nil {
		return target, false, nil
	}
	if current.Signed.Delegations == nil {
		return nil, false, nil
	}
	for _, role := range current.Signed.Delegations.GetRolesForTarget(trustedMaterialTarget) {
		target, stop, err := resolveWitnessChild(roles, current, role.Name, visited)
		if err != nil || target != nil || stop {
			return target, stop, err
		}
		if role.Terminating {
			return nil, true, nil
		}
	}
	return nil, false, nil
}

func resolveWitnessChild(roles map[string]*metadata.Metadata[metadata.TargetsType], parent *metadata.Metadata[metadata.TargetsType], name string, visited map[string]bool) (*metadata.TargetFiles, bool, error) {
	child := roles[name]
	if child == nil {
		return nil, false, errors.New("release: missing earlier trust delegation metadata")
	}
	if err := parent.VerifyDelegate(name, child); err != nil {
		return nil, false, fmt.Errorf("release: authenticate trust delegation: %w", err)
	}
	return resolveWitnessTarget(roles, name, visited)
}

func (fetch *trustFetcher) materialWitness() (materialWitness, error) {
	witness := materialWitness{Root: fetch.chain.Root.Signed.Version}
	roles := make(map[string][]byte)
	for path, contents := range fetch.documents {
		var header struct {
			Signed struct {
				Type    string `json:"_type"`
				Version int64  `json:"version"`
			} `json:"signed"`
		}
		if err := json.Unmarshal(contents, &header); err != nil {
			continue
		}
		if header.Signed.Type == metadata.SNAPSHOT {
			witness.Snapshot = contents
		}
		if header.Signed.Type != metadata.TARGETS {
			continue
		}
		name, err := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, strconv.FormatInt(header.Signed.Version, 10)+"."), ".json"))
		if err != nil {
			return witness, fmt.Errorf("release: decode trust role path: %w", err)
		}
		roles[name] = contents
	}
	if roles[metadata.TARGETS] == nil {
		return witness, errors.New("release: missing captured top-level trust targets")
	}
	witness.Targets = []witnessedRole{{Name: metadata.TARGETS, Metadata: roles[metadata.TARGETS]}}
	delete(roles, metadata.TARGETS)
	names := make([]string, 0, len(roles))
	for name := range roles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		witness.Targets = append(witness.Targets, witnessedRole{Name: name, Metadata: roles[name]})
	}
	return witness, nil
}
