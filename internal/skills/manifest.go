package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const manifestName = ".mesh-skill.json"
const maximumMetadata = 64 << 10
const maximumFiles = 128
const maximumBundle = 256 << 10
const maximumRoots = 32
const stateCurrent = "current"
const stateModified = "modified"
const stateInterrupted = "interrupted"
const stateMissing = "missing"
const stateUnmanaged = "unmanaged"
const stateNotInstalled = "not-installed"
const actionUninstall = "uninstall"

type ManagedFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Pending struct {
	Bundle   string        `json:"bundle"`
	Files    []ManagedFile `json:"files"`
	Proposed []ManagedFile `json:"proposed,omitempty"`
	Remove   bool          `json:"remove,omitempty"`
}
type Manifest struct {
	Version     int           `json:"version"`
	MeshVersion string        `json:"meshVersion"`
	Bundle      string        `json:"bundle"`
	Files       []ManagedFile `json:"files"`
	Proposed    []ManagedFile `json:"proposed,omitempty"`
	Pending     *Pending      `json:"pending,omitempty"`
}

func hashBytes(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func validHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}
func validPath(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) || path.IsAbs(value) || path.Clean(value) != value || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, manifestName) {
			return false
		}
	}
	return true
}
func validateFiles(files []ManagedFile) error {
	if len(files) > maximumFiles {
		return errors.New("skill manifest has too many files")
	}
	seen := map[string]bool{}
	for _, file := range files {
		key := foldPath(file.Path)
		if !validPath(file.Path) || !validHash(file.SHA256) || seen[key] {
			return fmt.Errorf("invalid or duplicate managed path %q", file.Path)
		}
		seen[key] = true
	}
	for key := range seen {
		if hasFileAncestor(key, seen) {
			return errors.New("skill manifest file paths overlap")
		}
	}
	return nil
}
func hasFileAncestor(name string, seen map[string]bool) bool {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if seen[parent] {
			return true
		}
	}
	return false
}
func parseManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := decodeMetadata(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Version != 1 || !validHash(manifest.Bundle) || len(manifest.MeshVersion) > 128 {
		return manifest, errors.New("invalid skill manifest version or bundle")
	}
	if err := validateManifestPaths(manifest); err != nil {
		return manifest, err
	}
	if err := validateFiles(manifest.Files); err != nil {
		return manifest, err
	}
	if err := validateFiles(manifest.Proposed); err != nil {
		return manifest, err
	}
	if manifest.Pending != nil {
		if err := validatePending(*manifest.Pending); err != nil {
			return manifest, err
		}
	}
	return manifest, nil
}
func validatePending(pending Pending) error {
	if !validHash(pending.Bundle) {
		return errors.New("invalid pending bundle hash")
	}
	if err := validateFiles(pending.Files); err != nil {
		return err
	}
	return validateFiles(pending.Proposed)
}
func decodeMetadata(data []byte, value any) error {
	if len(data) > maximumMetadata || !utf8.Valid(data) {
		return errors.New("skill metadata exceeds 64 KiB or is not UTF-8")
	}
	if err := checkJSON(json.NewDecoder(bytes.NewReader(data)), 0); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode skill metadata: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("skill metadata has trailing data")
	}
	return nil
}
func checkJSON(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("skill metadata nesting exceeds 8")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read skill metadata token: %w", err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		if err := checkObjectKey(decoder, delimiter, seen); err != nil {
			return err
		}
		if err := checkJSON(decoder, depth+1); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("close skill metadata: %w", err)
	}
	return nil
}
func checkKey(decoder *json.Decoder, seen map[string]bool) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read skill metadata key: %w", err)
	}
	key, ok := token.(string)
	if !ok || seen[strings.ToLower(key)] {
		return errors.New("duplicate or case-ambiguous skill metadata key")
	}
	// Match the exact schema spelling; encoding/json otherwise accepts case variants.
	switch key {
	case "version", "meshVersion", "bundle", "files", "proposed", "pending", "remove", "path", "sha256", "roots", "installedAt":
	default:
		return fmt.Errorf("unknown skill metadata key %q", key)
	}
	seen[strings.ToLower(key)] = true
	return nil
}
func managedBundle(files []File) []ManagedFile {
	entries := make([]ManagedFile, 0, len(files))
	for _, file := range files {
		entries = append(entries, ManagedFile{file.Path, hashBytes(file.Data)})
	}
	return entries
}
func baselineMatches(name string, data []byte, entries []ManagedFile) bool {
	hash := hashBytes(data)
	for _, entry := range entries {
		if entry.Path == name && entry.SHA256 == hash {
			return true
		}
	}
	return false
}
func allManaged(manifest Manifest) []ManagedFile {
	entries := append([]ManagedFile(nil), manifest.Files...)
	if manifest.Pending != nil {
		entries = append(entries, manifest.Pending.Files...)
	}
	return entries
}

func foldPath(value string) string {
	return strings.Map(func(char rune) rune {
		lowest := char
		for next := unicode.SimpleFold(char); next != char; next = unicode.SimpleFold(next) {
			if next < lowest {
				lowest = next
			}
		}
		return lowest
	}, value)
}

func checkObjectKey(decoder *json.Decoder, delimiter json.Delim, seen map[string]bool) error {
	if delimiter != '{' {
		return nil
	}
	return checkKey(decoder, seen)
}
func validateManifestPaths(manifest Manifest) error {
	names := map[string]string{}
	groups := [][]ManagedFile{manifest.Files, manifest.Proposed}
	if manifest.Pending != nil {
		groups = append(groups, manifest.Pending.Files, manifest.Pending.Proposed)
	}
	for _, group := range groups {
		if err := addManifestPaths(group, names); err != nil {
			return err
		}
	}
	for folded := range names {
		if hasNamedAncestor(folded, names) {
			return errors.New("skill manifest paths overlap across baselines")
		}
	}
	return nil
}
func addManifestPaths(files []ManagedFile, names map[string]string) error {
	for _, file := range files {
		if !validPath(file.Path) {
			return errors.New("invalid skill manifest path")
		}
		folded := foldPath(file.Path)
		if existing, ok := names[folded]; ok && existing != file.Path {
			return errors.New("skill manifest paths collide across baselines")
		}
		names[folded] = file.Path
	}
	if len(names) > maximumFiles {
		return errors.New("skill manifest has too many distinct paths")
	}
	return nil
}
func hasNamedAncestor(name string, names map[string]string) bool {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if _, found := names[parent]; found {
			return true
		}
	}
	return false
}
