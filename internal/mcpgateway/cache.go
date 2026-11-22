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
	"time"

	"golang.org/x/sys/unix"
)

// ListingCache persists complete listings privately so a fresh frontend can
// reuse them, and serve them as stale while a host is unreachable. Every
// record carries the host scope and service state it was fetched under; the
// gateway checks them against the host before use.
type ListingCache interface {
	// Load returns a record even past its 24-hour window, so the gateway
	// can report it as expired.
	Load(hostID, serviceID, instance, method, params string) (*Listing, bool)
	Save(l *Listing)
	// DropHost removes this caller's records of one host.
	DropHost(hostID string)
	DropRoute(hostID, serviceID string)
	// Expire makes a route's records revalidate on next use; no methods
	// means all of them.
	Expire(hostID, serviceID, instance string, methods ...string)
}

type nopCache struct{}

func (nopCache) Load(string, string, string, string, string) (*Listing, bool) { return nil, false }
func (nopCache) Save(*Listing)                                                {}
func (nopCache) DropHost(string)                                              {}
func (nopCache) DropRoute(string, string)                                     {}
func (nopCache) Expire(string, string, string, ...string)                     {}

// Section 2.5: at most 32 MiB of cached metadata per frontend state.
const (
	maxCacheBytes = 32 << 20
	cacheLockName = ".lock"
)

// FileCache keeps one private 0600 file per listing under a 0700 directory.
// Its scope binds records to the pinned host identity and application
// caller, so another identity or a re-pinned host never reads them, and a
// drop for one caller leaves other callers' records alone.
type FileCache struct {
	dir   string
	scope func(hostID string) string
}

type cacheRecord struct {
	Scope   string   `json:"scope"`
	Listing *Listing `json:"listing"`
}

// NewFileCache stores listings in dir. scope returns the caller and pin
// scope for a host; an empty scope disables caching for that host.
func NewFileCache(dir string, scope func(hostID string) string) *FileCache {
	return &FileCache{dir: dir, scope: scope}
}

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// prefix names one caller's files of one host.
func prefix(scope, hostID string) string { return digest(scope, hostID)[:16] + "-" }

func (c *FileCache) name(scope, hostID, serviceID, instance, method, params string) string {
	return prefix(scope, hostID) + digest(scope, hostID, serviceID, instance, method, params)[:32] + ".json"
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

// locked runs fn with the cache directory locked against other frontends,
// so saves, evictions and drops never interleave.
func (c *FileCache) locked(create bool, fn func(*os.Root)) {
	root, err := c.root(create)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	lock, err := root.OpenFile(cacheLockName, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = lock.Close() }()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX) != nil {
		return
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	fn(root)
}

func readRecord(root *os.Root, name string) (*cacheRecord, bool) {
	file, err := root.Open(name)
	if err != nil {
		return nil, false
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxListingBytes*2))
	if err != nil {
		return nil, false
	}
	var rec cacheRecord
	if json.Unmarshal(raw, &rec) != nil || rec.Listing == nil {
		return nil, false
	}
	return &rec, true
}

func writeRecord(root *os.Root, name string, rec *cacheRecord) {
	raw, err := json.Marshal(rec)
	if err != nil || len(raw) > maxCacheBytes {
		return
	}
	evict(root, name, len(raw))
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	temp := "." + name + "." + hex.EncodeToString(suffix[:])
	if root.WriteFile(temp, raw, 0o600) != nil {
		return
	}
	if root.Rename(temp, name) != nil {
		_ = root.Remove(temp)
	}
}

func (c *FileCache) Load(hostID, serviceID, instance, method, params string) (*Listing, bool) {
	scope := c.scope(hostID)
	if scope == "" {
		return nil, false
	}
	var out *Listing
	c.locked(false, func(root *os.Root) {
		rec, ok := readRecord(root, c.name(scope, hostID, serviceID, instance, method, params))
		l := rec.listingIf(ok, scope)
		if l != nil && l.HostID == hostID && l.ServiceID == serviceID && l.Instance == instance && l.Method == method && l.Params == params {
			out = l
		}
	})
	return out, out != nil
}

func (rec *cacheRecord) listingIf(ok bool, scope string) *Listing {
	if !ok || rec.Scope != scope {
		return nil
	}
	return rec.Listing
}

func (c *FileCache) Save(l *Listing) {
	scope := c.scope(l.HostID)
	if scope == "" {
		return
	}
	c.locked(true, func(root *os.Root) {
		writeRecord(root, c.name(scope, l.HostID, l.ServiceID, l.Instance, l.Method, l.Params), &cacheRecord{Scope: scope, Listing: l})
	})
}

// evict removes the oldest files until incoming bytes fit the budget.
func evict(root *os.Root, replacing string, incoming int) {
	type sized struct {
		name string
		size int64
		mod  time.Time
	}
	var files []sized
	total := int64(incoming)
	for _, name := range cacheFiles(root) {
		info, err := root.Stat(name)
		if err != nil || name == replacing {
			continue
		}
		files = append(files, sized{name, info.Size(), info.ModTime()})
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

func cacheFiles(root *os.Root) []string {
	dir, err := root.Open(".")
	if err != nil {
		return nil
	}
	names, _ := dir.Readdirnames(-1)
	_ = dir.Close()
	return slices.DeleteFunc(names, func(n string) bool { return !strings.HasSuffix(n, ".json") || strings.HasPrefix(n, ".") })
}

// each runs fn on every record of this caller for one host.
func (c *FileCache) each(hostID string, fn func(root *os.Root, name string, rec *cacheRecord)) {
	scope := c.scope(hostID)
	if scope == "" {
		return
	}
	mine := prefix(scope, hostID)
	c.locked(false, func(root *os.Root) {
		for _, name := range cacheFiles(root) {
			if !strings.HasPrefix(name, mine) {
				continue
			}
			if rec, ok := readRecord(root, name); ok && rec.Scope == scope {
				fn(root, name, rec)
			}
		}
	})
}

func (c *FileCache) DropHost(hostID string) {
	c.each(hostID, func(root *os.Root, name string, _ *cacheRecord) { _ = root.Remove(name) })
}

func (c *FileCache) DropRoute(hostID, serviceID string) {
	c.each(hostID, func(root *os.Root, name string, rec *cacheRecord) {
		if rec.Listing.ServiceID == serviceID {
			_ = root.Remove(name)
		}
	})
}

func (c *FileCache) Expire(hostID, serviceID, instance string, methods ...string) {
	c.each(hostID, func(root *os.Root, name string, rec *cacheRecord) {
		l := rec.Listing
		if l.ServiceID != serviceID || l.Instance != instance || (len(methods) > 0 && !slices.Contains(methods, l.Method)) {
			return
		}
		l.TTL = 0
		writeRecord(root, name, rec)
	})
}
