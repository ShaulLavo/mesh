package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const registryName = "skills.json"

type registeredRoot struct {
	Path        string    `json:"path"`
	InstalledAt time.Time `json:"installedAt"`
}
type registry struct {
	Version int              `json:"version"`
	Roots   []registeredRoot `json:"roots"`
}

func readRegistry(dir *os.File) (registry, error) {
	result := registry{Version: 1, Roots: []registeredRoot{}}
	data, err := readAt(dir, registryName, maximumMetadata)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if err := decodeMetadata(data, &result); err != nil {
		return result, err
	}
	if result.Version != 1 || len(result.Roots) > maximumRoots {
		return result, errors.New("invalid skills registry version or root count")
	}
	seen := map[string]bool{}
	for _, root := range result.Roots {
		if !filepath.IsAbs(root.Path) || filepath.Clean(root.Path) != root.Path || len(root.Path) > 4096 || seen[root.Path] || root.InstalledAt.IsZero() {
			return result, errors.New("invalid or duplicate skills registry root")
		}
		seen[root.Path] = true
	}
	return result, nil
}
func (m *Manager) registrySnapshot() (registry, error) {
	dir, err := openDirectory(m.ConfigDir, false)
	if errors.Is(err, os.ErrNotExist) {
		return registry{Version: 1, Roots: []registeredRoot{}}, nil
	}
	if err != nil {
		return registry{}, err
	}
	defer func() { _ = dir.Close() }()
	return readRegistry(dir)
}
func isRegistered(registry registry, path string) bool {
	for _, root := range registry.Roots {
		if root.Path == path {
			return true
		}
	}
	return false
}
func (m *Manager) register(dir *os.File, current registry, path string, remove bool) error {
	present := isRegistered(current, path)
	if remove {
		current.Roots = slices.DeleteFunc(current.Roots, func(root registeredRoot) bool { return root.Path == path })
	}
	if !remove && !present {
		if len(current.Roots) >= maximumRoots {
			return errors.New("skills registry has 32 roots; uninstall a registered root first")
		}
		current.Roots = append(current.Roots, registeredRoot{Path: path, InstalledAt: time.Now().UTC()})
	}
	if present == !remove {
		return m.barrier(dir, registryName)
	}
	data, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("encode skill registry: %w", err)
	}
	return m.publish(dir, registryName, data, 0600)
}
