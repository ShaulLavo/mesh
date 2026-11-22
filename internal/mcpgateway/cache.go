package mcpgateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// ListingCache persists complete listings privately so a fresh frontend can
// serve stale entries while a host is unreachable.
type ListingCache interface {
	Load(hostID, serviceID, method string) (*Listing, bool)
	Save(l *Listing)
	DropHost(hostID string)
	DropRoute(hostID, serviceID string)
}

type nopCache struct{}

func (nopCache) Load(string, string, string) (*Listing, bool) { return nil, false }
func (nopCache) Save(*Listing)                                {}
func (nopCache) DropHost(string)                              {}
func (nopCache) DropRoute(string, string)                     {}

// Section 2.5: at most 32 MiB of cached metadata per frontend state.
const maxCacheBytes = 32 << 20

// FileCache keeps one private 0600 file per listing under a 0700 directory.
// Its scope binds entries to the pinned host identity and application
// caller, so another identity or a re-pinned host never reads them.
type FileCache struct {
	dir   string
	scope func(hostID string) string
	now   func() time.Time
	mu    sync.Mutex
}

type cacheRecord struct {
	Scope   string   `json:"scope"`
	Listing *Listing `json:"listing"`
}

// NewFileCache stores listings in dir. scope returns the caller and pin
// scope for a host; an empty scope disables caching for that host.
func NewFileCache(dir string, scope func(hostID string) string, now func() time.Time) *FileCache {
	if now == nil {
		now = time.Now
	}
	return &FileCache{dir: dir, scope: scope, now: now}
}

func (c *FileCache) name(scope, hostID, serviceID, method string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + hostID + "\x00" + serviceID + "\x00" + method))
	return hostTag(hostID) + "-" + hex.EncodeToString(sum[:16]) + ".json"
}

// hostTag lets DropHost find a host's files without reading them.
func hostTag(hostID string) string {
	sum := sha256.Sum256([]byte(hostID))
	return hex.EncodeToString(sum[:6])
}

func (c *FileCache) root(create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(c.dir, 0o700); err != nil {
			return nil, fmt.Errorf("create gateway cache: %w", err)
		}
	}
	info, err := os.Lstat(c.dir)
	if err != nil {
		return nil, fmt.Errorf("inspect gateway cache: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("gateway cache directory must be private")
	}
	root, err := os.OpenRoot(c.dir)
	if err != nil {
		return nil, fmt.Errorf("open gateway cache: %w", err)
	}
	return root, nil
}

func (c *FileCache) Load(hostID, serviceID, method string) (*Listing, bool) {
	scope := c.scope(hostID)
	if scope == "" {
		return nil, false
	}
	root, err := c.root(false)
	if err != nil {
		return nil, false
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(c.name(scope, hostID, serviceID, method))
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxListingBytes*2))
	if err != nil {
		return nil, false
	}
	var rec cacheRecord
	if json.Unmarshal(raw, &rec) != nil || rec.Scope != scope || rec.Listing == nil || rec.Listing.HostID != hostID ||
		rec.Listing.ServiceID != serviceID || rec.Listing.Method != method || !rec.Listing.usable(c.now()) {
		return nil, false
	}
	return rec.Listing, true
}

func (c *FileCache) Save(l *Listing) {
	scope := c.scope(l.HostID)
	if scope == "" {
		return
	}
	raw, err := json.Marshal(cacheRecord{Scope: scope, Listing: l})
	if err != nil || len(raw) > maxCacheBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	root, err := c.root(true)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	name := c.name(scope, l.HostID, l.ServiceID, l.Method)
	evict(root, name, len(raw))
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	temp := "." + name + "." + hex.EncodeToString(suffix[:])
	if err := root.WriteFile(temp, raw, 0o600); err != nil {
		return
	}
	if root.Rename(temp, name) != nil {
		_ = root.Remove(temp)
	}
}

// evict removes the oldest files until incoming bytes fit the budget.
func evict(root *os.Root, replacing string, incoming int) {
	dir, err := root.Open(".")
	if err != nil {
		return
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return
	}
	type sized struct {
		name string
		size int64
		mod  time.Time
	}
	var files []sized
	total := int64(incoming)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !strings.HasSuffix(e.Name(), ".json") || e.Name() == replacing {
			continue
		}
		files = append(files, sized{e.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	slices.SortFunc(files, func(a, b sized) int { return a.mod.Compare(b.mod) })
	for _, f := range files {
		if total <= maxCacheBytes {
			return
		}
		if root.Remove(f.name) == nil {
			total -= f.size
		}
	}
}

func (c *FileCache) drop(prefix string, match func(name string) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	root, err := c.root(false)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	dir, err := root.Open(".")
	if err != nil {
		return
	}
	names, _ := dir.Readdirnames(-1)
	_ = dir.Close()
	for _, name := range names {
		if strings.HasPrefix(name, prefix) && match(name) {
			_ = root.Remove(name)
		}
	}
}

func (c *FileCache) DropHost(hostID string) {
	c.drop(hostTag(hostID)+"-", func(string) bool { return true })
}

func (c *FileCache) DropRoute(hostID, serviceID string) {
	scope := c.scope(hostID)
	if scope == "" {
		return
	}
	names := map[string]bool{}
	for _, method := range []string{toolsKind.method, promptsKind.method, resourcesKind.method, templatesKind.method, "server/discover"} {
		names[c.name(scope, hostID, serviceID, method)] = true
	}
	c.drop(hostTag(hostID)+"-", func(name string) bool { return names[name] })
}
