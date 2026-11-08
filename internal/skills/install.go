package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

type Root struct {
	Path, State, RegistrationState                            string
	Registered                                                bool
	MeshVersion, Bundle                                       *string
	ModifiedFiles, MissingFiles, ProposedFiles, RetainedFiles []string
	Problems                                                  []error
}
type Manager struct {
	ConfigDir, StateDir, MeshVersion string
	files                            []File
	syncDir                          func(*os.File, string) error
}

func (m *Manager) bundle() []File {
	if m.files != nil {
		return m.files
	}
	return Bundle()
}
func newRoot(path string, registered bool) Root {
	registration := "unregistered"
	if registered {
		registration = "registered"
	}
	return Root{Path: path, Registered: registered, RegistrationState: registration, ModifiedFiles: []string{}, MissingFiles: []string{}, ProposedFiles: []string{}, RetainedFiles: []string{}, Problems: []error{}}
}
func (m *Manager) Status(root string) ([]Root, error) {
	registry, err := m.registrySnapshot()
	if err != nil {
		return nil, err
	}
	if root != "" {
		selected, err := selectedRoots(root, registry)
		if err != nil {
			return nil, err
		}
		canonical := selected[0]
		result, err := m.status(canonical, isRegistered(registry, canonical))
		return []Root{result}, err
	}
	results := make([]Root, 0, len(registry.Roots))
	for _, root := range registry.Roots {
		result, err := m.status(root.Path, true)
		if err != nil {
			result.Problems = append(result.Problems, err)
		}
		results = append(results, result)
	}
	return results, nil
}
func (m *Manager) status(path string, registered bool) (Root, error) {
	result := newRoot(path, registered)
	root, err := openDirectory(path, false)
	if errors.Is(err, os.ErrNotExist) {
		result.State = stateMissing
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer func() { _ = root.Close() }()
	tree, manifest, err := readTree(root)
	if err != nil {
		return result, err
	}
	if tree == nil {
		result.State = stateNotInstalled
		if registered {
			result.State = stateMissing
		}
		return result, nil
	}
	defer func() { _ = tree.Close() }()
	if manifest == nil {
		result.State = stateUnmanaged
		return result, nil
	}
	result.MeshVersion, result.Bundle = &manifest.MeshVersion, &manifest.Bundle
	if err := classifyTree(tree, *manifest, &result); err != nil {
		return result, err
	}
	result.State = "outdated"
	switch {
	case manifest.Pending != nil:
		result.State = stateInterrupted
	case len(result.ModifiedFiles) > 0:
		result.State = stateModified
	case len(result.MissingFiles) > 0:
		result.State = stateMissing
	case manifest.Bundle == m.digest() && manifestMatchesBundle(*manifest, m.bundle()):
		result.State = stateCurrent
	}
	return result, nil
}
func manifestMatchesBundle(manifest Manifest, files []File) bool {
	if len(manifest.Files) != len(files) {
		return false
	}
	for _, file := range files {
		if !baselineMatches(file.Path, file.Data, manifest.Files) {
			return false
		}
	}
	return true
}
func readTree(root *os.File) (*os.File, *Manifest, error) {
	tree, err := childDirectory(root, "mesh", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := checkDirectory(tree); err != nil {
		_ = tree.Close()
		return nil, nil, err
	}
	data, err := readAt(tree, manifestName, maximumMetadata)
	if errors.Is(err, os.ErrNotExist) {
		return tree, nil, nil
	}
	if err != nil {
		_ = tree.Close()
		return nil, nil, err
	}
	manifest, err := parseManifest(data)
	if err != nil {
		_ = tree.Close()
		return nil, nil, unsafe(fmt.Errorf("read skill manifest: %w", err))
	}
	return tree, &manifest, nil
}
func classifyTree(tree *os.File, manifest Manifest, result *Root) error {
	seen := map[string]bool{}
	for _, entry := range allManaged(manifest) {
		if seen[entry.Path] {
			continue
		}
		seen[entry.Path] = true
		data, oversized, err := readManaged(tree, entry.Path)
		if errors.Is(err, os.ErrNotExist) {
			result.MissingFiles = append(result.MissingFiles, entry.Path)
			continue
		}
		if err != nil {
			return err
		}
		if oversized || !baselineMatches(entry.Path, data, allManaged(manifest)) {
			result.ModifiedFiles = append(result.ModifiedFiles, entry.Path)
		}
	}
	return nil
}
func (m *Manager) Apply(ctx context.Context, action, selected string) ([]Root, error) {
	config, err := openDirectory(m.ConfigDir, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = config.Close() }()
	lock, err := lockDirectory(ctx, config)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	registry, err := readRegistry(config)
	if err != nil {
		return nil, err
	}
	roots, err := selectedRoots(selected, registry)
	if err != nil {
		return nil, err
	}
	results := make([]Root, 0, len(roots))
	for _, path := range roots {
		results = append(results, m.applyRoot(config, registry, action, path))
	}
	return results, nil
}
func selectedRoots(selected string, registry registry) ([]string, error) {
	if selected == "" {
		return mapRoots(registry), nil
	}
	canonical, err := canonicalRoot(selected)
	if err == nil {
		return []string{canonical}, nil
	}
	absolute, absErr := filepath.Abs(selected)
	if absErr == nil && errors.Is(err, os.ErrNotExist) && isRegistered(registry, absolute) {
		return []string{absolute}, nil
	}
	return nil, err
}
func mapRoots(registry registry) []string {
	result := make([]string, 0, len(registry.Roots))
	for _, root := range registry.Roots {
		result = append(result, root.Path)
	}
	return result
}
func (m *Manager) applyRoot(config *os.File, registry registry, action, path string) Root {
	result := newRoot(path, isRegistered(registry, path))
	err := m.mutateTree(action, &result)
	if err != nil {
		result.Problems = append(result.Problems, err)
		return result
	}
	if err := m.register(config, registry, path, action == actionUninstall); err != nil {
		result.RegistrationState = "unconfirmed"
		result.Problems = append(result.Problems, fmt.Errorf("skill tree saved, but registry not confirmed; run the same command again: %w", err))
		return result
	}
	result.Registered = action != actionUninstall
	result.RegistrationState = "unregistered"
	if result.Registered {
		result.RegistrationState = "registered"
	}
	return result
}
func (m *Manager) mutateTree(action string, result *Root) error {
	root, err := openDirectory(result.Path, false)
	if errors.Is(err, os.ErrNotExist) && action == actionUninstall {
		result.State = stateNotInstalled
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tree, manifest, err := readTree(root)
	if err != nil {
		return err
	}
	if tree == nil {
		if action == actionUninstall {
			result.State = stateNotInstalled
			return m.barrier(root, "mesh")
		}
		return m.install(root, result)
	}
	defer func() { _ = tree.Close() }()
	if manifest == nil && action == actionUninstall {
		result.State = stateUnmanaged
		return m.barrier(root, "mesh")
	}
	if manifest == nil {
		return conflict("mesh/ exists and was not installed by Mesh")
	}
	return m.mutateManaged(root, tree, *manifest, action, result)
}

func (m *Manager) mutateManaged(root, tree *os.File, manifest Manifest, action string, result *Root) error {
	if manifest.Binding != m.bindingID() {
		return conflict("skill root belongs to another Mesh configuration or state directory; use its original paths")
	}
	if err := classifyTree(tree, manifest, result); err != nil {
		return err
	}
	if action == actionUninstall {
		return m.uninstall(root, tree, manifest, result)
	}
	if manifest.Pending != nil && manifest.Pending.Remove {
		return conflict("skill uninstall is interrupted; repeat mesh skill uninstall for this root first")
	}
	if err := m.barrier(root, "mesh"); err != nil {
		return err
	}
	return m.update(tree, manifest, result)
}

func (m *Manager) writeManifest(tree *os.File, manifest Manifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode skill manifest: %w", err)
	}
	if _, err := parseManifest(data); err != nil {
		return err
	}
	return m.publish(tree, manifestName, data, 0644)
}
func (m *Manager) install(root *os.File, result *Root) error {
	stage, err := temporaryName()
	if err != nil {
		return err
	}
	if err := unix.Mkdirat(int(root.Fd()), stage, 0755); err != nil {
		return fmt.Errorf("stage skill directory: %w", err)
	}
	tree, err := childDirectory(root, stage, false)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			cleanup := *m
			cleanup.syncDir = nil
			cleanup.discardStage(root, tree, stage)
		}
		_ = tree.Close()
	}()
	for _, file := range m.bundle() {
		if err := m.publish(tree, file.Path, file.Data, 0644); err != nil {
			return err
		}
	}
	manifest := Manifest{Version: 1, Binding: m.bindingID(), MeshVersion: m.MeshVersion, Bundle: m.digest(), Files: managedBundle(m.bundle())}
	manifest.Pending = &Pending{Bundle: manifest.Bundle, Files: manifest.Files}
	if err := m.writeManifest(tree, manifest); err != nil {
		return err
	}
	if err := renameNew(int(root.Fd()), stage, "mesh"); err != nil {
		return fmt.Errorf("publish new skill directory: %w", err)
	}
	published = true
	result.State = stateInterrupted
	if err := m.barrier(root, "mesh"); err != nil {
		return err
	}
	recovery := manifest
	manifest.Pending = nil
	if err := m.writeManifest(tree, manifest); err != nil {
		return errors.Join(err, m.writeManifest(tree, recovery))
	}
	result.State = stateCurrent
	result.Bundle = &manifest.Bundle
	result.MeshVersion = &manifest.MeshVersion
	return nil
}
func (m *Manager) discardStage(root, tree *os.File, stage string) {
	// Only the randomly named, unpublished staging tree belongs to this cleanup.
	for _, file := range m.bundle() {
		_ = m.removeAt(tree, file.Path)
	}
	_ = m.removeAt(tree, manifestName)
	_ = m.removeDirectories(tree, managedBundle(m.bundle()))
	_ = unix.Unlinkat(int(root.Fd()), stage, unix.AT_REMOVEDIR)
}
func (m *Manager) update(tree *os.File, manifest Manifest, result *Root) error {
	files := m.bundle()
	digest := m.digest()
	if manifest.Pending == nil && manifest.Bundle == digest && manifestMatchesBundle(manifest, files) && len(result.ModifiedFiles) == 0 && len(result.MissingFiles) == 0 {
		return m.settleCurrent(tree, manifest, result)
	}
	if err := m.preservePendingOwnership(tree, &manifest, result.Path); err != nil {
		return err
	}
	if err := classifyProposedPaths(tree, files, allManaged(manifest), result); err != nil {
		return err
	}
	pending := Pending{Bundle: digest, Files: managedBundle(files), Proposed: proposalPlan(files, result.ModifiedFiles)}
	old := allManaged(manifest)
	manifest.Pending = &pending
	result.State = stateInterrupted
	if err := m.writeManifest(tree, manifest); err != nil {
		return err
	}
	if err := m.replaceFiles(tree, old, manifest, result); err != nil {
		return err
	}
	recovery := manifest
	manifest.Files = retainedFiles(pending.Files, old, result.ModifiedFiles)
	manifest.Bundle = digest
	manifest.MeshVersion = m.MeshVersion
	manifest.Pending = nil
	manifest.Proposed = mergeProposals(manifest.Proposed, pending.Proposed)
	if err := m.writeManifest(tree, manifest); err != nil {
		return errors.Join(err, m.writeManifest(tree, recovery))
	}
	result.Bundle = &manifest.Bundle
	result.MeshVersion = &manifest.MeshVersion
	result.MissingFiles = []string{}
	result.State = stateCurrent
	if len(result.ModifiedFiles) > 0 {
		result.State = stateModified
		result.Problems = append(result.Problems, conflict("user-modified skill files were retained; inspect the proposed replacement paths"))
	}
	return nil
}
func (m *Manager) replaceFiles(tree *os.File, old []ManagedFile, manifest Manifest, result *Root) error {
	for _, file := range m.bundle() {
		if err := m.replaceFile(tree, file, old, manifest, result); err != nil {
			return err
		}
	}
	for _, entry := range uniqueFiles(old) {
		if bundleHas(m.bundle(), entry.Path) || slices.Contains(result.ModifiedFiles, entry.Path) {
			continue
		}
		if err := m.removeAt(tree, entry.Path); err != nil {
			return err
		}
	}
	return m.removeDirectories(tree, old)
}
func (m *Manager) replaceFile(tree *os.File, file File, old []ManagedFile, manifest Manifest, result *Root) error {
	data, oversized, err := readManaged(tree, file.Path)
	if errors.Is(err, os.ErrNotExist) {
		return m.publish(tree, file.Path, file.Data, 0644)
	}
	if err != nil {
		return err
	}
	if !oversized && string(data) == string(file.Data) {
		return m.settleFile(tree, file.Path)
	}
	if !oversized && baselineMatches(file.Path, data, old) {
		return m.publish(tree, file.Path, file.Data, 0644)
	}
	if !slices.Contains(result.ModifiedFiles, file.Path) {
		result.ModifiedFiles = append(result.ModifiedFiles, file.Path)
	}
	return m.propose(file, manifest, result)
}
func (m *Manager) settleFile(tree *os.File, name string) error {
	dir, _, err := fileParent(tree, name, false)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return m.barrier(dir, name)
}
func uniqueFiles(entries []ManagedFile) []ManagedFile {
	result := []ManagedFile{}
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.Path] {
			continue
		}
		seen[entry.Path] = true
		result = append(result, entry)
	}
	return result
}
func bundleHas(files []File, path string) bool {
	for _, file := range files {
		if file.Path == path {
			return true
		}
	}
	return false
}
func (m *Manager) removeDirectories(root *os.File, entries []ManagedFile) error {
	names := map[string]bool{}
	for _, entry := range entries {
		for parent := filepath.Dir(entry.Path); parent != "."; parent = filepath.Dir(parent) {
			names[parent] = true
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	slices.SortFunc(ordered, func(a, b string) int { return len(b) - len(a) })
	for _, name := range ordered {
		if err := m.removeEmptyDirectory(root, name); err != nil {
			return err
		}
	}
	return nil
}
func (m *Manager) removeEmptyDirectory(root *os.File, name string) error {
	dir, leaf, err := fileParent(root, name, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	child, err := childDirectory(dir, leaf, false)
	if errors.Is(err, os.ErrNotExist) {
		return m.barrier(dir, name)
	}
	if err != nil {
		return err
	}
	_ = child.Close()
	err = unix.Unlinkat(int(dir.Fd()), leaf, unix.AT_REMOVEDIR)
	if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove empty skill directory: %w", err)
	}
	return m.barrier(dir, name)
}
func (m *Manager) uninstall(root, tree *os.File, manifest Manifest, result *Root) error {
	entries := allManaged(manifest)
	pendingFiles := manifest.Files
	pendingProposed := manifest.Proposed
	if manifest.Pending != nil {
		pendingFiles = manifest.Pending.Files
		pendingProposed = manifest.Pending.Proposed
	}
	manifest.Pending = &Pending{Bundle: manifest.Bundle, Files: pendingFiles, Proposed: pendingProposed, Remove: true}
	result.State = stateInterrupted
	if err := m.writeManifest(tree, manifest); err != nil {
		return err
	}
	for _, entry := range uniqueFiles(entries) {
		if slices.Contains(result.ModifiedFiles, entry.Path) {
			result.RetainedFiles = append(result.RetainedFiles, entry.Path)
			continue
		}
		if err := m.removeAt(tree, entry.Path); err != nil {
			return err
		}
	}
	if err := m.removeProposals(manifest, result); err != nil {
		return err
	}
	if err := m.pruneProposalDirectories(manifest, result.Path); err != nil {
		return err
	}
	if err := m.removeDirectories(tree, entries); err != nil {
		return err
	}
	if err := m.removeAt(tree, manifestName); err != nil {
		return errors.Join(err, m.writeManifest(tree, manifest))
	}
	if err := m.removeEmptyDirectory(root, "mesh"); err != nil {
		return err
	}
	result.State = stateNotInstalled
	if len(result.RetainedFiles) > 0 {
		result.State = stateModified
	}
	return nil
}

func proposalPlan(files []File, modified []string) []ManagedFile {
	result := []ManagedFile{}
	for _, file := range files {
		if slices.Contains(modified, file.Path) {
			result = append(result, ManagedFile{file.Path, hashBytes(file.Data)})
		}
	}
	return result
}
func mergeProposals(old, next []ManagedFile) []ManagedFile {
	result := append([]ManagedFile(nil), old...)
	for _, entry := range next {
		index := slices.IndexFunc(result, func(old ManagedFile) bool { return old.Path == entry.Path })
		if index < 0 {
			result = append(result, entry)
			continue
		}
		result[index] = entry
	}
	return result
}
func (m *Manager) proposalDirectory(root string, create bool) (*os.File, string, error) {
	path := filepath.Join(m.StateDir, "skills", "proposed", hashBytes([]byte(root)))
	state, err := openDirectory(m.StateDir, create)
	if err != nil {
		return nil, path, err
	}
	defer func() { _ = state.Close() }()
	dir, _, err := fileParent(state, "skills/proposed/"+hashBytes([]byte(root))+"/anchor", create)
	return dir, path, err
}

func (m *Manager) propose(file File, manifest Manifest, result *Root) error {
	dir, path, err := m.proposalDirectory(result.Path, true)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	proposed := filepath.Join(path, filepath.FromSlash(file.Path))
	result.ProposedFiles = append(result.ProposedFiles, proposed)
	data, oversized, err := readManaged(dir, file.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if oversized || err == nil && !baselineMatches(file.Path, data, manifest.Proposed) && !baselineMatches(file.Path, data, manifest.Pending.Proposed) {
		result.RetainedFiles = append(result.RetainedFiles, proposed)
		return conflict("a user-modified proposal was retained at " + proposed)
	}
	return m.publish(dir, file.Path, file.Data, 0644)
}
func (m *Manager) removeProposals(manifest Manifest, result *Root) error {
	entries := manifest.Proposed
	if manifest.Pending != nil {
		entries = append(append([]ManagedFile(nil), entries...), manifest.Pending.Proposed...)
	}
	if len(entries) == 0 {
		return nil
	}
	dir, _, err := m.proposalDirectory(result.Path, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	for _, entry := range uniqueFiles(entries) {
		if err := m.removeProposal(dir, entries, entry.Path, result); err != nil {
			return err
		}
	}
	return m.removeDirectories(dir, entries)
}
func (m *Manager) removeProposal(dir *os.File, entries []ManagedFile, name string, result *Root) error {
	data, oversized, err := readManaged(dir, name)
	if errors.Is(err, os.ErrNotExist) {
		return m.removeAt(dir, name)
	}
	if err != nil {
		return err
	}
	if oversized || !baselineMatches(name, data, entries) {
		path := filepath.Join(m.StateDir, "skills", "proposed", hashBytes([]byte(result.Path)))
		result.RetainedFiles = append(result.RetainedFiles, filepath.Join(path, name))
		return nil
	}
	return m.removeAt(dir, name)
}

func classifyProposedPaths(tree *os.File, files []File, baseline []ManagedFile, result *Root) error {
	for _, file := range files {
		data, oversized, err := readManaged(tree, file.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if slices.Contains(result.ModifiedFiles, file.Path) || !oversized && (string(data) == string(file.Data) || baselineMatches(file.Path, data, baseline)) {
			continue
		}
		result.ModifiedFiles = append(result.ModifiedFiles, file.Path)
	}
	return nil
}

func (m *Manager) digest() string {
	if m.files == nil {
		return Digest()
	}
	return bundleDigest(m.files)
}

func retainedFiles(next, old []ManagedFile, modified []string) []ManagedFile {
	result := append([]ManagedFile(nil), next...)
	for _, entry := range uniqueFiles(old) {
		if !slices.Contains(modified, entry.Path) || slices.ContainsFunc(next, func(file ManagedFile) bool { return file.Path == entry.Path }) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func (m *Manager) settleCurrent(tree *os.File, manifest Manifest, result *Root) error {
	for _, entry := range manifest.Files {
		if err := m.settleFile(tree, entry.Path); err != nil {
			return err
		}
	}
	if err := m.barrier(tree, manifestName); err != nil {
		return err
	}
	result.State = stateCurrent
	result.MeshVersion = &manifest.MeshVersion
	result.Bundle = &manifest.Bundle
	return nil
}

func (m *Manager) pruneProposalDirectories(manifest Manifest, root string) error {
	if len(manifest.Proposed) == 0 && len(manifest.Pending.Proposed) == 0 {
		return nil
	}
	state, err := openDirectory(m.StateDir, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = state.Close() }()
	for _, name := range []string{"skills/proposed/" + hashBytes([]byte(root)), "skills/proposed", "skills"} {
		if err := m.removeEmptyDirectory(state, name); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) bindingID() string {
	paths := filepath.Clean(m.ConfigDir) + "\x00" + filepath.Clean(m.StateDir)
	return hashBytes([]byte("mesh-skill-registry\x00" + paths))
}

func (m *Manager) preservePendingOwnership(tree *os.File, manifest *Manifest, root string) error {
	if manifest.Pending == nil {
		return nil
	}
	managed, err := recoveryBaselines(tree, allManaged(*manifest))
	if err != nil {
		return err
	}
	proposals := append(append([]ManagedFile(nil), manifest.Proposed...), manifest.Pending.Proposed...)
	proposed, err := m.recoverProposalBaselines(root, proposals)
	if err != nil {
		return err
	}
	manifest.Files = managed
	manifest.Proposed = proposed
	return nil
}

func (m *Manager) recoverProposalBaselines(root string, entries []ManagedFile) ([]ManagedFile, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	dir, _, err := m.proposalDirectory(root, false)
	if errors.Is(err, os.ErrNotExist) {
		return uniqueFiles(entries), nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return recoveryBaselines(dir, entries)
}

func recoveryBaselines(root *os.File, entries []ManagedFile) ([]ManagedFile, error) {
	result := uniqueFiles(entries)
	for index, entry := range result {
		data, oversized, err := readManaged(root, entry.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if oversized {
			continue
		}
		hash := hashBytes(data)
		if slices.Contains(entries, ManagedFile{entry.Path, hash}) {
			result[index].SHA256 = hash
		}
	}
	return result, nil
}
