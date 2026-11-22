package cli

import (
	"context"
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
	"golang.org/x/sys/unix"
)

const (
	mcpCatalogCacheDirName    = "mcp-catalog-cache"
	mcpCatalogCacheLockName   = ".cache.lock"
	mcpCatalogCacheStateName  = ".state.json"
	mcpCatalogCacheVersion    = 2
	mcpCatalogCacheMaxTotal   = 32 << 20
	mcpCatalogCacheMaxEntry   = 4 << 20
	mcpCatalogCacheStaleLimit = 24 * time.Hour
	mcpCatalogCacheLockWait   = 1500 * time.Millisecond
)

// mcpCatalogCacheEntry stores private, allowlisted hosted MCP catalog metadata
// scoped to the host, its pinned identity, and the querying caller scope.
type mcpCatalogCacheEntry struct {
	SchemaVersion int                      `json:"schemaVersion"`
	HostID        string                   `json:"hostId"`
	MeshIdentity  string                   `json:"meshIdentity"`
	MachineName   string                   `json:"machineName,omitempty"`
	CallerScope   string                   `json:"callerScope"`
	ReceivedAt    time.Time                `json:"receivedAt"`
	Services      []mcphost.CatalogService `json:"services"`
}

type mcpCatalogCacheState struct {
	SchemaVersion int               `json:"schemaVersion"`
	HostGen       map[string]uint64 `json:"hostGen"`
}

// mcpCatalogCache manages private metadata-only catalog files under stateDir
// with cross-process flock serialization, invalidation generations, and strict
// 0700/0600 permission verification.
type mcpCatalogCache struct {
	stateDir string
	maxTotal int64
}

func openMCPCatalogCache(stateDir string) *mcpCatalogCache {
	return &mcpCatalogCache{stateDir: stateDir, maxTotal: mcpCatalogCacheMaxTotal}
}

func (c *mcpCatalogCache) budgetLimit() int64 {
	if c != nil && c.maxTotal > 0 {
		return c.maxTotal
	}
	return mcpCatalogCacheMaxTotal
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
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("MCP catalog cache state path is not a real directory")
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
	if dirInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("MCP catalog cache directory has insecure permissions %04o; expected 0700", dirInfo.Mode().Perm())
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open MCP catalog cache root: %w", err)
	}
	return root, nil
}

func acquireMCPCacheLock(ctx context.Context, root *os.Root) (*os.File, error) {
	file, err := openMCPCacheLockFile(root)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat MCP catalog cache lock: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o177 != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("MCP catalog cache lock has insecure mode %04o", info.Mode().Perm())
	}
	if err := waitMCPCacheLock(ctx, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// openMCPCacheLockFile opens the never-removed cache lock file without calling
// openat(..., O_CREAT|O_NOFOLLOW) concurrently across threads: on Darwin/XNU,
// concurrent O_CREAT without O_EXCL on a not-yet-created file can spuriously
// fail with ENOENT.
func openMCPCacheLockFile(root *os.Root) (*os.File, error) {
	var lastErr error
	for range 8 {
		file, err := root.OpenFile(mcpCatalogCacheLockName, os.O_RDWR, 0)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("open MCP catalog cache lock: %w", err)
		}
		file, err = root.OpenFile(mcpCatalogCacheLockName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrExist) && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("open MCP catalog cache lock: %w", err)
		}
		lastErr = err
	}
	return nil, fmt.Errorf("open MCP catalog cache lock: %w", lastErr)
}

func waitMCPCacheLock(ctx context.Context, file *os.File) error {
	waitCtx, cancel := context.WithTimeout(ctx, mcpCatalogCacheLockWait)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("lock MCP catalog cache: %w", err)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("acquire MCP catalog cache lock: %w", waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func readMCPCacheState(root *os.Root) (mcpCatalogCacheState, error) {
	state := mcpCatalogCacheState{SchemaVersion: mcpCatalogCacheVersion, HostGen: map[string]uint64{}}
	info, err := root.Lstat(mcpCatalogCacheStateName)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("stat MCP catalog cache state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o177 != 0 {
		return state, fmt.Errorf("MCP catalog cache state has insecure mode %04o", info.Mode().Perm())
	}
	raw, err := root.ReadFile(mcpCatalogCacheStateName)
	if err != nil {
		return state, fmt.Errorf("read MCP catalog cache state: %w", err)
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, fmt.Errorf("decode MCP catalog cache state: %w", err)
	}
	if state.HostGen == nil {
		state.HostGen = map[string]uint64{}
	}
	state.SchemaVersion = mcpCatalogCacheVersion
	return state, nil
}

func writeMCPCacheState(root *os.Root, state mcpCatalogCacheState) error {
	state.SchemaVersion = mcpCatalogCacheVersion
	if state.HostGen == nil {
		state.HostGen = map[string]uint64{}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode MCP catalog cache state: %w", err)
	}
	return writeMCPCacheAtomically(root, mcpCatalogCacheStateName, raw)
}

// BeginObservation records the current invalidation generation for hostID
// before a live query runs so a later SaveIfCurrent refuses to publish if a
// concurrent denial or pin change invalidated the host while the query was in flight.
func (c *mcpCatalogCache) BeginObservation(ctx context.Context, hostID string) (uint64, error) {
	root, err := c.openRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = root.Close() }()
	lock, err := acquireMCPCacheLock(ctx, root)
	if err != nil {
		return 0, err
	}
	defer func() { _ = lock.Close() }()
	state, err := readMCPCacheState(root)
	if err != nil {
		return 0, err
	}
	return state.HostGen[hostID], nil
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

// SaveIfCurrent atomically publishes entry under the cross-process cache lock
// only if hostID's invalidation generation still equals obsGen and eviction
// succeeds within the 32 MiB total budget.
func (c *mcpCatalogCache) SaveIfCurrent(ctx context.Context, entry mcpCatalogCacheEntry, obsGen uint64) (bool, error) {
	if entry.HostID == "" || entry.MeshIdentity == "" || entry.CallerScope == "" || entry.ReceivedAt.IsZero() {
		return false, errors.New("MCP catalog cache entry requires host, pin, caller scope, and timestamp")
	}
	entry.SchemaVersion = mcpCatalogCacheVersion
	if entry.Services == nil {
		entry.Services = []mcphost.CatalogService{}
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return false, fmt.Errorf("encode MCP catalog cache entry: %w", err)
	}
	if len(raw) > mcpCatalogCacheMaxEntry || int64(len(raw)) > c.budgetLimit() {
		return false, fmt.Errorf("MCP catalog cache entry exceeds maximum size (%d bytes)", len(raw))
	}
	root, err := c.openRoot(true)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	lock, err := acquireMCPCacheLock(ctx, root)
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Close() }()

	state, err := readMCPCacheState(root)
	if err != nil {
		return false, err
	}
	if state.HostGen[entry.HostID] != obsGen {
		return false, nil
	}
	target := mcpCatalogCacheKey(entry.HostID, entry.MeshIdentity, entry.CallerScope)
	if err := enforceMCPCacheBudget(root, target, int64(len(raw)), c.budgetLimit()); err != nil {
		return false, err
	}
	if err := writeMCPCacheAtomically(root, target, raw); err != nil {
		return false, err
	}
	return true, nil
}

// InvalidateHost removes all cached entries for hostID across every caller scope
// and advances hostID's generation so racing refreshes cannot resurrect them.
func (c *mcpCatalogCache) InvalidateHost(ctx context.Context, hostID string) error {
	if hostID == "" {
		return nil
	}
	return c.invalidate(ctx, []string{hostID}, func(e *mcpCatalogCacheEntry) bool {
		return e.HostID == hostID
	})
}

// InvalidateHostCaller removes cached entries for one host and caller scope
// and advances hostID's generation so racing refreshes cannot resurrect them.
func (c *mcpCatalogCache) InvalidateHostCaller(ctx context.Context, hostID, callerScope string) error {
	if hostID == "" {
		return nil
	}
	return c.invalidate(ctx, []string{hostID}, func(e *mcpCatalogCacheEntry) bool {
		return e.HostID == hostID && (callerScope == "" || e.CallerScope == callerScope)
	})
}

// PruneMismatchedPins deletes cached entries whose host was removed or whose
// pinned MeshIdentity changed in hosts.json and advances those hosts' generations.
func (c *mcpCatalogCache) PruneMismatchedPins(ctx context.Context, pins map[string]string) error {
	root, err := c.openRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	lock, err := acquireMCPCacheLock(ctx, root)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	names, err := listMCPCacheFiles(root)
	if err != nil {
		return err
	}
	touched := map[string]struct{}{}
	for _, name := range names {
		prunedHost, err := pruneMismatchedEntry(root, name, pins)
		if err != nil {
			return err
		}
		if prunedHost != "" {
			touched[prunedHost] = struct{}{}
		}
	}
	return advanceCacheHosts(root, touched)
}

func pruneMismatchedEntry(root *os.Root, name string, pins map[string]string) (string, error) {
	entry, readErr := readMCPCacheFile(root, name)
	if readErr != nil {
		if rmErr := root.Remove(name); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return "", fmt.Errorf("remove unreadable MCP catalog cache entry %s: %w", name, rmErr)
		}
		return "", nil
	}
	if pin, ok := pins[entry.HostID]; ok && pin == entry.MeshIdentity {
		return "", nil
	}
	if rmErr := root.Remove(name); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return "", fmt.Errorf("remove stale-pin MCP catalog cache entry %s: %w", name, rmErr)
	}
	return entry.HostID, nil
}

func advanceCacheHosts(root *os.Root, touched map[string]struct{}) error {
	if len(touched) == 0 {
		return nil
	}
	state, err := readMCPCacheState(root)
	if err != nil {
		return err
	}
	for hostID := range touched {
		state.HostGen[hostID]++
	}
	return writeMCPCacheState(root, state)
}

func (c *mcpCatalogCache) invalidate(ctx context.Context, advanceHosts []string, match func(*mcpCatalogCacheEntry) bool) error {
	// Create the cache directory if needed so advancing HostGen is persisted even
	// when a racing writer is about to create the first snapshot for that host.
	root, err := c.openRoot(true)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	lock, err := acquireMCPCacheLock(ctx, root)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	names, err := listMCPCacheFiles(root)
	if err != nil {
		return err
	}
	for _, name := range names {
		entry, readErr := readMCPCacheFile(root, name)
		if readErr != nil || match(entry) {
			if rmErr := root.Remove(name); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return fmt.Errorf("remove MCP catalog cache entry %s: %w", name, rmErr)
			}
		}
	}
	state, err := readMCPCacheState(root)
	if err != nil {
		return err
	}
	for _, hostID := range advanceHosts {
		state.HostGen[hostID]++
	}
	return writeMCPCacheState(root, state)
}

func readMCPCacheFile(root *os.Root, name string) (*mcpCatalogCacheEntry, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("stat MCP catalog cache %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("MCP catalog cache entry is not a regular file")
	}
	if info.Mode().Perm()&0o177 != 0 {
		return nil, fmt.Errorf("MCP catalog cache entry %s has insecure permissions %04o; expected 0600", name, info.Mode().Perm())
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

func enforceMCPCacheBudget(root *os.Root, replacing string, incomingBytes, limit int64) error {
	files, total, err := collectMCPCacheSizes(root, replacing, incomingBytes)
	if err != nil || total <= limit {
		return err
	}
	slices.SortFunc(files, compareMCPCacheAge)
	for _, f := range files {
		if total <= limit {
			break
		}
		if err := root.Remove(f.name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("evict MCP catalog cache entry %s: %w", f.name, err)
		}
		total -= f.size
	}
	if total > limit {
		return fmt.Errorf("MCP catalog cache exceeds %d byte budget after eviction", limit)
	}
	return nil
}

func collectMCPCacheSizes(root *os.Root, replacing string, incomingBytes int64) ([]sizedMCPCacheFile, int64, error) {
	names, err := listMCPCacheFiles(root)
	if err != nil {
		return nil, 0, err
	}
	var files []sizedMCPCacheFile
	total := incomingBytes
	for _, name := range names {
		if name == replacing {
			continue
		}
		info, err := root.Lstat(name)
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o177 != 0 {
			return nil, 0, fmt.Errorf("MCP catalog cache entry %s has insecure mode %04o", name, info.Mode().Perm())
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
		return nil, fmt.Errorf("open MCP catalog cache directory: %w", err)
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("list MCP catalog cache directory: %w", err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".") {
			names = append(names, name)
		}
	}
	return names, nil
}
