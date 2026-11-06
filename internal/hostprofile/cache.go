package hostprofile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

type ContextObservation struct {
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Daemon   string `json:"daemon"`
}

type CachedContext struct {
	HostID       string              `json:"hostId"`
	MeshIdentity string              `json:"meshIdentity"`
	ReceivedAt   time.Time           `json:"receivedAt"`
	Observed     *ContextObservation `json:"observed,omitempty"`
	Context      ContextReply        `json:"context"`
}

type ContextCache struct{ state string }

func OpenContextCache(stateDir string) *ContextCache { return &ContextCache{state: stateDir} }

func contextKey(hostID, meshIdentity string) string {
	digest := sha256.Sum256([]byte(hostID + "\x00" + meshIdentity))
	return hex.EncodeToString(digest[:]) + ".json"
}

func (c *ContextCache) directory(create bool) (*os.File, error) {
	state, err := openDirectory(c.state, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = state.Close() }()
	return openPrivateDirectoryChild(state, "context-cache", create)
}

func (c *ContextCache) Read(hostID, meshIdentity string) (*CachedContext, error) {
	dir, err := c.directory(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return readContextEntry(dir, hostID, meshIdentity)
}

func readContextEntry(dir *os.File, hostID, meshIdentity string) (*CachedContext, error) {
	data, err := readPrivate(dir, contextKey(hostID, meshIdentity), MaximumContextBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := strictObject(data, MaximumContextBytes, []string{"hostId", "meshIdentity", "receivedAt", "observed", "context"}); err != nil {
		return nil, err
	}
	var entry CachedContext
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, invalid("cache", err.Error())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, invalid("cache", err.Error())
	}
	reply, err := ParseContext(raw["context"], hostID)
	if err != nil {
		return nil, err
	}
	entry.Context = reply
	if raw["observed"] != nil {
		if err := strictObject(raw["observed"], 1024, []string{"version", "platform", "daemon"}); err != nil {
			return nil, err
		}
	}
	if err := validateEntry(entry, hostID, meshIdentity); err != nil {
		return nil, err
	}
	return &entry, nil
}

func validateEntry(entry CachedContext, hostID, meshIdentity string) error {
	if entry.HostID != hostID || entry.MeshIdentity != meshIdentity || entry.ReceivedAt.IsZero() {
		return invalid("cache", "does not match pinned identity or receipt time")
	}
	if entry.Observed != nil {
		if err := validateText("version", entry.Observed.Version, 256, false); err != nil {
			return err
		}
		if err := validateText("platform", entry.Observed.Platform, 64, false); err != nil {
			return err
		}
		if entry.Observed.Daemon != "running" {
			return invalid("daemon", "cache requires an actual running daemon observation")
		}
	}
	return ValidateContext(entry.Context, hostID)
}

// Save accepts only validated live observations. A matching summary retains full content.
func (c *ContextCache) Save(ctx context.Context, entry CachedContext) (bool, error) {
	if err := validateEntry(entry, entry.HostID, entry.MeshIdentity); err != nil {
		return false, err
	}
	dir, err := c.directory(true)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = dir.Close() }()
	lock, err := acquireLock(ctx, dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Close() }()
	previous, readErr := readContextEntry(dir, entry.HostID, entry.MeshIdentity)
	regressed := false
	if previous != nil {
		entry.Context.Host, regressed = mergeCachedSection(previous.Context.Host, entry.Context.Host)
		var fleetRegressed bool
		entry.Context.Fleet, fleetRegressed = mergeCachedSection(previous.Context.Fleet, entry.Context.Fleet)
		regressed = regressed || fleetRegressed
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("cache write cancelled: %w", err)
	}
	if err := publishContextEntry(dir, entry); err != nil {
		return regressed, err
	}
	return regressed, readErr
}

func mergeCachedSection(previous, next WireSection) (WireSection, bool) {
	regressed := previous.Revision != nil && next.Revision != nil && *next.Revision < *previous.Revision
	same := previous.Revision != nil && next.Revision != nil && *previous.Revision == *next.Revision && previous.Digest == next.Digest && previous.Author == next.Author && previous.Fleet == next.Fleet
	if next.Completeness == Summary && previous.Completeness == Full && same && (next.State == StateValid || next.State == StateUntrusted) {
		next.Completeness, next.Profile, next.Signature = Full, previous.Profile, previous.Signature
		next.UpdatedAt, next.InstructionsBytes = previous.UpdatedAt, previous.InstructionsBytes
	}
	return next, regressed
}

func publishContextEntry(dir *os.File, entry CachedContext) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode context cache: %w", err)
	}
	if len(data) > MaximumContextBytes {
		return invalid("cache", "exceeds 163840 encoded bytes")
	}
	file, name, err := temporaryFile(dir)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), name, 0) }()
	defer func() { _ = file.Close() }()
	if err := writeAll(file, data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync context cache: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close context cache: %w", err)
	}
	if err := unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), contextKey(entry.HostID, entry.MeshIdentity)); err != nil {
		return fmt.Errorf("publish context cache: %w", err)
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync context cache directory: %w", err)
	}
	return nil
}

// Prune removes only cache entries that no longer belong to the address book.
func (c *ContextCache) Prune(ctx context.Context, pins map[string]string) error {
	dir, err := c.directory(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	lock, err := acquireLock(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	keep := make(map[string]bool, len(pins))
	for id, pin := range pins {
		keep[contextKey(id, pin)] = true
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("list context cache: %w", err)
	}
	removed := false
	for _, name := range names {
		if !cacheFilename(name) || keep[name] {
			continue
		}
		if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("prune context cache: %w", err)
		}
		removed = true
	}
	if !removed {
		return nil
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync pruned context cache: %w", err)
	}
	return nil
}

func cacheFilename(name string) bool {
	if len(name) != 69 || name[64:] != ".json" {
		return false
	}
	decoded, err := hex.DecodeString(name[:64])
	return err == nil && hex.EncodeToString(decoded) == name[:64]
}
