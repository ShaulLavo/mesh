package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/mcphost"
)

const (
	mcpCatalogCacheDirName    = "mcp-catalog-cache"
	mcpCatalogCacheVersion    = 1
	mcpCatalogCacheMaxTotal   = 32 << 20
	mcpCatalogCacheMaxEntry   = 4 << 20
	mcpCatalogCacheStaleLimit = 24 * time.Hour
)

// mcpCatalogCacheEntry stores private, metadata-only hosted MCP catalog entries
// scoped to the host, its pinned identity, and the querying caller scope.
type mcpCatalogCacheEntry struct {
	SchemaVersion int                   `json:"schemaVersion"`
	HostID        string                `json:"hostId"`
	MeshIdentity  string                `json:"meshIdentity"`
	MachineName   string                `json:"machineName,omitempty"`
	CallerScope   string                `json:"callerScope"`
	ReceivedAt    time.Time             `json:"receivedAt"`
	Services      []mcphost.ServiceView `json:"services"`
}

// mcpCatalogCache manages private metadata-only catalog files under stateDir.
type mcpCatalogCache struct {
	stateDir string
}

func openMCPCatalogCache(stateDir string) *mcpCatalogCache {
	return &mcpCatalogCache{stateDir: stateDir}
}

func mcpCatalogCacheKey(hostID, meshIdentity, callerScope string) string {
	sum := sha256.Sum256([]byte(hostID + "\x00" + meshIdentity + "\x00" + callerScope))
	return hex.EncodeToString(sum[:]) + ".json"
}

func (c *mcpCatalogCache) openRoot(create bool) (*os.Root, error) {
	if c == nil || c.stateDir == "" {
		return nil, os.ErrNotExist
	}
	info, err := os.Lstat(c.stateDir)
	if err != nil {
		return nil, fmt.Errorf("stat state directory for MCP catalog cache: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("MCP catalog cache state path is not a directory")
	}
	dir := filepath.Join(c.stateDir, mcpCatalogCacheDirName)
	if create {
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create MCP catalog cache directory: %w", err)
		}
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("stat MCP catalog cache directory: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return nil, errors.New("MCP catalog cache directory must be a private directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open MCP catalog cache root: %w", err)
	}
	return root, nil
}

func (c *mcpCatalogCache) Read(hostID, meshIdentity, callerScope string) (*mcpCatalogCacheEntry, error) {
	root, err := c.openRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	name := mcpCatalogCacheKey(hostID, meshIdentity, callerScope)
	entry, err := readMCPCacheFile(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if entry.SchemaVersion != mcpCatalogCacheVersion || entry.HostID != hostID || entry.MeshIdentity != meshIdentity || entry.CallerScope != callerScope || entry.ReceivedAt.IsZero() {
		return nil, errors.New("MCP catalog cache entry scope mismatch")
	}
	return entry, nil
}

func (c *mcpCatalogCache) Save(entry mcpCatalogCacheEntry) error {
	if entry.HostID == "" || entry.MeshIdentity == "" || entry.CallerScope == "" || entry.ReceivedAt.IsZero() {
		return errors.New("MCP catalog cache entry requires host, pin, caller scope, and timestamp")
	}
	entry.SchemaVersion = mcpCatalogCacheVersion
	if entry.Services == nil {
		entry.Services = []mcphost.ServiceView{}
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode MCP catalog cache entry: %w", err)
	}
	if len(raw) > mcpCatalogCacheMaxEntry {
		return fmt.Errorf("MCP catalog cache entry exceeds %d bytes", mcpCatalogCacheMaxEntry)
	}
	root, err := c.openRoot(true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	target := mcpCatalogCacheKey(entry.HostID, entry.MeshIdentity, entry.CallerScope)
	if err := enforceMCPCacheBudget(root, target, len(raw)); err != nil {
		return err
	}
	return writeMCPCacheAtomically(root, target, raw)
}

// InvalidateHost removes all cached entries for hostID (used on pin change or
// host removal).
func (c *mcpCatalogCache) InvalidateHost(hostID string) error {
	return c.deleteMatching(func(e *mcpCatalogCacheEntry) bool {
		return e.HostID == hostID
	})
}

// InvalidateHostCaller removes cached entries for one host and caller scope
// (used immediately on explicit authorization denial).
func (c *mcpCatalogCache) InvalidateHostCaller(hostID, callerScope string) error {
	return c.deleteMatching(func(e *mcpCatalogCacheEntry) bool {
		return e.HostID == hostID && (callerScope == "" || e.CallerScope == callerScope)
	})
}

// PruneMismatchedPins deletes cached entries whose host was removed or whose
// pinned MeshIdentity changed in hosts.json.
func (c *mcpCatalogCache) PruneMismatchedPins(pins map[string]string) error {
	return c.deleteMatching(func(e *mcpCatalogCacheEntry) bool {
		pin, ok := pins[e.HostID]
		return !ok || pin != e.MeshIdentity
	})
}

func (c *mcpCatalogCache) deleteMatching(match func(*mcpCatalogCacheEntry) bool) error {
	root, err := c.openRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	names, err := listMCPCacheFiles(root)
	if err != nil {
		return err
	}
	for _, name := range names {
		entry, err := readMCPCacheFile(root, name)
		if err != nil || match(entry) {
			if rmErr := root.Remove(name); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return fmt.Errorf("remove MCP catalog cache entry %s: %w", name, rmErr)
			}
		}
	}
	return nil
}

func readMCPCacheFile(root *os.Root, name string) (*mcpCatalogCacheEntry, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("stat MCP catalog cache %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("MCP catalog cache entry is not a regular file")
	}
	if info.Size() > mcpCatalogCacheMaxEntry {
		return nil, errors.New("MCP catalog cache entry exceeds maximum size")
	}
	raw, err := root.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read MCP catalog cache %s: %w", name, err)
	}
	var entry mcpCatalogCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, fmt.Errorf("decode MCP catalog cache %s: %w", name, err)
	}
	return &entry, nil
}

func writeMCPCacheAtomically(root *os.Root, target string, raw []byte) error {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("create MCP catalog cache temp name: %w", err)
	}
	tmp := ".tmp-" + hex.EncodeToString(nonce[:])
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create MCP catalog cache temp file: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = root.Remove(tmp)
		}
	}()
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return fmt.Errorf("write MCP catalog cache temp file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync MCP catalog cache temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close MCP catalog cache temp file: %w", err)
	}
	if err := root.Rename(tmp, target); err != nil {
		return fmt.Errorf("publish MCP catalog cache entry: %w", err)
	}
	cleanup = false
	return nil
}

type sizedMCPCacheFile struct {
	name    string
	size    int64
	modTime time.Time
}

func enforceMCPCacheBudget(root *os.Root, replacing string, incomingBytes int) error {
	files, total, err := collectMCPCacheSizes(root, replacing, int64(incomingBytes))
	if err != nil || total <= mcpCatalogCacheMaxTotal {
		return err
	}
	slices.SortFunc(files, compareMCPCacheAge)
	for _, f := range files {
		if total <= mcpCatalogCacheMaxTotal {
			break
		}
		if err := root.Remove(f.name); err == nil || errors.Is(err, os.ErrNotExist) {
			total -= f.size
		}
	}
	return nil
}

func collectMCPCacheSizes(root *os.Root, replacing string, incoming int64) ([]sizedMCPCacheFile, int64, error) {
	names, err := listMCPCacheFiles(root)
	if err != nil {
		return nil, 0, err
	}
	var files []sizedMCPCacheFile
	total := incoming
	for _, name := range names {
		if name == replacing {
			continue
		}
		info, statErr := root.Lstat(name)
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, sizedMCPCacheFile{name: name, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	return files, total, nil
}

func compareMCPCacheAge(a, b sizedMCPCacheFile) int {
	if a.modTime.Before(b.modTime) {
		return -1
	}
	if a.modTime.After(b.modTime) {
		return 1
	}
	return strings.Compare(a.name, b.name)
}

func listMCPCacheFiles(root *os.Root) ([]string, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open MCP catalog cache dir: %w", err)
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("list MCP catalog cache dir: %w", err)
	}
	var out []string
	for _, name := range entries {
		if isMCPCacheFilename(name) {
			out = append(out, name)
		}
	}
	return out, nil
}

func isMCPCacheFilename(name string) bool {
	if len(name) != 69 || !strings.HasSuffix(name, ".json") {
		return false
	}
	decoded, err := hex.DecodeString(name[:64])
	return err == nil && hex.EncodeToString(decoded) == name[:64]
}
