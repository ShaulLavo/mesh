package domainpolicy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/hostprofile"
	"golang.org/x/sys/unix"
)

const catalogPolicyFile = "catalog-policy.json"
const InitialCatalogGeneration uint64 = 1

var ErrPolicyLockBusy = errors.New("policy writer lock is busy")
var ErrPublicationPending = errors.New("domain publication is pending")

// CatalogToken identifies the immutable policy and the generation a consumer loaded.
type CatalogToken struct {
	Digest     string `json:"digest"`
	Generation uint64 `json:"generation"`
}
type CatalogSnapshot struct {
	Identity string `json:"identity"`
	Complete bool   `json:"complete"`
}
type CatalogPolicy struct {
	Schema int `json:"schema"`
	CatalogToken
	Pending   bool                       `json:"pending"`
	Affected  []string                   `json:"affected,omitempty"`
	Snapshots map[string]CatalogSnapshot `json:"snapshots"`
}

// CatalogGate serializes local advisory snapshots with domain publication.
// It never acquires update authority or a domain writer lock.
type CatalogGate struct {
	dir    *os.File
	Policy CatalogPolicy
}

// Consumers belong to the process, not a cache or watcher. State directories are
// separate installations, including the isolated installations used by tests.
var catalogConsumers = struct {
	sync.Mutex
	byDirectory map[string]*catalogConsumer
}{byDirectory: make(map[string]*catalogConsumer)}

type catalogConsumer struct {
	once  sync.Once
	token CatalogToken
	err   error
}

// ConsumerCatalogToken retains the first admitted generation even when every
// cache and watcher has closed. A failed initial observation cannot admit a
// later epoch just because its policy bytes happen to match.
func ConsumerCatalogToken(stateDir string) (CatalogToken, error) {
	key, err := filepath.Abs(stateDir)
	if err != nil {
		return CatalogToken{}, fmt.Errorf("locate catalog consumer: %w", err)
	}
	catalogConsumers.Lock()
	consumer := catalogConsumers.byDirectory[key]
	if consumer == nil {
		consumer = &catalogConsumer{}
		catalogConsumers.byDirectory[key] = consumer
	}
	catalogConsumers.Unlock()
	consumer.once.Do(func() {
		policy, err := ReadCatalogPolicy(stateDir)
		consumer.token, consumer.err = policy.CatalogToken, err
	})
	return consumer.token, consumer.err
}

// ReadCatalogPolicy observes an atomic publication without joining advisory
// SQLite writer admission. Missing state is the logical first generation and
// does not create a directory, lock, sidecar or database.
func ReadCatalogPolicy(stateDir string) (CatalogPolicy, error) {
	dir, err := openPolicyDirectory(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return initialCatalogPolicy(), nil
	}
	if err != nil {
		return CatalogPolicy{}, err
	}
	defer func() { _ = dir.Close() }()
	policy, err := readCatalogPolicy(dir)
	if errors.Is(err, os.ErrNotExist) {
		return initialCatalogPolicy(), nil
	}
	return policy, err
}

func initialCatalogPolicy() CatalogPolicy {
	return CatalogPolicy{Schema: 1, CatalogToken: CatalogToken{Digest: Current().Digest(), Generation: InitialCatalogGeneration}, Snapshots: make(map[string]CatalogSnapshot)}
}

func readCatalogPolicy(dir *os.File) (CatalogPolicy, error) {
	data, err := readPolicyFile(dir, catalogPolicyFile, 2<<20)
	if err != nil {
		return CatalogPolicy{}, err
	}
	var policy CatalogPolicy
	if err := decodePolicyJSON(data, &policy); err != nil {
		return CatalogPolicy{}, err
	}
	if policy.Schema != 1 || policy.Generation == 0 || len(policy.Digest) != 64 || len(policy.Snapshots) > 256 || len(policy.Affected) > 256 {
		return CatalogPolicy{}, errors.New("catalog policy: invalid generation record")
	}
	if err := validateCatalogPolicy(policy); err != nil {
		return CatalogPolicy{}, err
	}
	if policy.Snapshots == nil {
		policy.Snapshots = make(map[string]CatalogSnapshot)
	}
	return policy, nil
}

func (p Policy) Digest() string {
	data, _ := json.Marshal(p)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func WithCatalogGate(ctx context.Context, stateDir string, operation func(*CatalogGate) error) error {
	return withPolicyLock(ctx, stateDir, ".catalog-policy.lock", func(dir *os.File) error {
		gate, err := loadCatalogGate(dir)
		if err != nil {
			return err
		}
		return operation(gate)
	})
}
func loadCatalogGate(dir *os.File) (*CatalogGate, error) {
	policy, err := readCatalogPolicy(dir)
	if errors.Is(err, os.ErrNotExist) {
		gate := &CatalogGate{dir: dir, Policy: initialCatalogPolicy()}
		if err := gate.Save(); err != nil {
			return nil, err
		}
		return gate, nil
	}
	if err != nil {
		return nil, err
	}
	gate := &CatalogGate{dir: dir, Policy: policy}
	if err := settlePolicyDirectory(dir); err != nil {
		return nil, err
	}
	return gate, nil
}

func (g *CatalogGate) Save() error { return publishPolicyJSON(g.dir, catalogPolicyFile, g.Policy) }
func (g *CatalogGate) Prepare(digest string, affected []string) error {
	if g.Policy.Digest == digest {
		return settlePolicyDirectory(g.dir)
	}
	if g.Policy.Generation == math.MaxUint64 || len(digest) != 64 {
		return errors.New("catalog policy: generation exhausted or digest invalid")
	}
	for id := range g.Policy.Snapshots {
		affected = append(affected, id)
	}
	affected = append(affected, g.Policy.Affected...)
	slices.Sort(affected)
	affected = slices.Compact(affected)
	if len(affected) > 256 {
		return errors.New("catalog policy: affected host limit exceeded")
	}
	g.Policy.Generation++
	g.Policy.Digest, g.Policy.Affected, g.Policy.Pending = digest, affected, true
	g.Policy.Snapshots = make(map[string]CatalogSnapshot)
	return g.Save()
}
func (g *CatalogGate) Current(token CatalogToken) bool {
	return token == g.Policy.CatalogToken && !g.Policy.Pending
}
func (g *CatalogGate) Settle(invalidate func([]string) error) error {
	if !g.Policy.Pending {
		return nil
	}
	if err := invalidate(g.Policy.Affected); err != nil {
		return err
	}
	g.Policy.Pending, g.Policy.Affected = false, nil
	return g.Save()
}

// DomainFile is used only while its catalog gate and component writer are held.
type DomainFile struct {
	dir      *os.File
	name     string
	Policy   Policy
	Present  bool
	contents []byte
}

func WithDomainFile(ctx context.Context, path string, operation func(*DomainFile) error) error {
	return withPolicyLock(ctx, filepath.Dir(path), ".domains.lock", func(dir *os.File) error {
		file := &DomainFile{dir: dir, name: filepath.Base(path)}
		data, err := readPolicyFile(dir, file.name, 65536)
		if errors.Is(err, os.ErrNotExist) {
			return operation(file)
		}
		if err != nil {
			return err
		}
		file.Policy, err = Parse(bytes.NewReader(data))
		if err != nil {
			return err
		}
		file.Present, file.contents = true, data
		return operation(file)
	})
}
func (f *DomainFile) Save(policy Policy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if f.Present && policy.LegacyCertificateDomain != f.Policy.LegacyCertificateDomain {
		return errors.New("domain policy: legacy certificate domain cannot change")
	}
	for _, domain := range append(f.Policy.Domains(), Current().Domains()...) {
		if !slices.Contains(policy.Domains(), domain) {
			return errors.New("domain policy: every accepted private domain must be retained")
		}
	}
	fields := make(map[string]json.RawMessage)
	if len(f.contents) != 0 {
		if err := json.Unmarshal(f.contents, &fields); err != nil {
			return fmt.Errorf("decode domain fields: %w", err)
		}
	}
	fields["primary"], _ = json.Marshal(policy.Primary)
	if policy.Aliases == nil {
		delete(fields, "aliases")
	} else {
		fields["aliases"], _ = json.Marshal(policy.Aliases)
	}
	if !f.Present && policy.LegacyCertificateDomain != "" {
		fields["legacyCertificateDomain"], _ = json.Marshal(policy.LegacyCertificateDomain)
	}
	if err := publishPolicyJSON(f.dir, f.name, fields); err != nil {
		return err
	}
	f.Policy, f.Present = policy, true
	return nil
}
func (f *DomainFile) Settle() error { return settlePolicyDirectory(f.dir) }

func withPolicyLock(ctx context.Context, directory, lockName string, operation func(*os.File) error) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create policy directory: %w", err)
	}
	dir, err := openPolicyDirectory(directory)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	lock, err := openPolicyFile(dir, lockName, unix.O_RDWR|unix.O_CREAT, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	wait, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if err = acquirePolicyLock(wait, lock); err != nil {
		return err
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	if err = ctx.Err(); err != nil {
		return fmt.Errorf("policy operation cancelled: %w", err)
	}
	return operation(dir)
}
func openPolicyDirectory(directory string) (*os.File, error) {
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open policy directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), directory)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("inspect policy directory: %w", err)
	}
	if int(stat.Uid) != os.Geteuid() || stat.Mode&0022 != 0 {
		_ = dir.Close()
		return nil, errors.New("policy directory must be owned and writable only by this user")
	}
	return dir, nil
}
func acquirePolicyLock(ctx context.Context, file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("lock policy: %w", err)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %w", ErrPolicyLockBusy, ctx.Err())
		case <-timer.C:
		}
	}
}
func openPolicyFile(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, mode)
	if err != nil {
		return nil, fmt.Errorf("open policy file: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect policy file: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int(stat.Uid) != os.Geteuid() || stat.Nlink != 1 || stat.Mode&0077 != 0 {
		_ = file.Close()
		return nil, errors.New("policy files must be private regular files owned by this user with one link")
	}
	return file, nil
}
func readPolicyFile(dir *os.File, name string, limit int64) ([]byte, error) {
	file, err := openPolicyFile(dir, name, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, errors.New("policy file exceeds size limit")
	}
	return data, nil
}
func decodePolicyJSON(data []byte, target any) error {
	check := json.NewDecoder(bytes.NewReader(data))
	if err := hostprofile.CheckJSON(check, 0, 32); err != nil {
		return fmt.Errorf("policy JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode policy: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("policy JSON requires one value")
	}
	return nil
}
func publishPolicyJSON(dir *os.File, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode policy: %w", err)
	}
	if len(data)+1 > 2<<20 {
		return errors.New("policy record exceeds size limit")
	}
	temp := ".policy-" + rand.Text()
	file, err := openPolicyFile(dir, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), temp, 0) }()
	defer func() { _ = file.Close() }()
	if _, err = file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write policy: %w", err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("sync policy: %w", err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close policy: %w", err)
	}
	if err = unix.Renameat(int(dir.Fd()), temp, int(dir.Fd()), name); err != nil {
		return fmt.Errorf("publish policy: %w", err)
	}
	return settlePolicyDirectory(dir)
}
func settlePolicyDirectory(dir *os.File) error {
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync policy directory: %w", err)
	}
	for path := filepath.Dir(dir.Name()); ; path = filepath.Dir(path) {
		root, err := os.OpenRoot(path)
		if err != nil {
			return fmt.Errorf("open policy parent: %w", err)
		}
		parent, err := root.Open(".")
		_ = root.Close()
		if err != nil {
			return fmt.Errorf("open policy parent: %w", err)
		}
		err = parent.Sync()
		closeErr := parent.Close()
		if err != nil || closeErr != nil {
			return fmt.Errorf("sync policy parent: %w", errors.Join(err, closeErr))
		}
		if path == filepath.Dir(path) {
			return nil
		}
	}
}

func validateCatalogPolicy(policy CatalogPolicy) error {
	digest, err := hex.DecodeString(policy.Digest)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("catalog policy: invalid digest")
	}
	if !policy.Pending && len(policy.Affected) != 0 || policy.Pending && len(policy.Snapshots) != 0 {
		return errors.New("catalog policy: invalid pending snapshots")
	}
	for id, snapshot := range policy.Snapshots {
		if id == "" || snapshot.Identity == "" {
			return errors.New("catalog policy: invalid snapshot identity")
		}
	}
	for _, id := range policy.Affected {
		if id == "" {
			return errors.New("catalog policy: invalid affected identity")
		}
	}
	return nil
}

// SettleSavedPolicy repairs only the directory barriers of already visible bytes.
// Catalog readers use no authority or component writer lock.
func SettleSavedPolicy(path, digest string) error {
	fd, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open saved policy directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), filepath.Dir(path))
	defer func() { _ = dir.Close() }()
	data, err := readPolicyFile(dir, filepath.Base(path), 65536)
	if errors.Is(err, os.ErrNotExist) {
		return ErrPublicationPending
	}
	if err != nil {
		return err
	}
	policy, err := Parse(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if policy.Digest() != digest {
		return ErrPublicationPending
	}
	return settlePolicyDirectory(dir)
}

func ReadSavedPolicy(path string) (Policy, bool, error) {
	fd, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, fmt.Errorf("open saved policy directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), filepath.Dir(path))
	defer func() { _ = dir.Close() }()
	data, err := readPolicyFile(dir, filepath.Base(path), 65536)
	if errors.Is(err, os.ErrNotExist) {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, err
	}
	policy, err := Parse(bytes.NewReader(data))
	return policy, true, err
}
