package mcphost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/storage"
)

// WorkerStatus is what the launcher found for one reserved generation.
type WorkerStatus uint8

const (
	WorkerAbsent WorkerStatus = iota
	WorkerLive
	WorkerEnded
	WorkerInterrupted
	WorkerUncertain
)

// LaunchKey names one generation of one instance in the execution journal.
type LaunchKey struct {
	Service    string
	Instance   string
	Generation int64
}

// Principal and Opaque form the journal key, so a generation is reserved
// exactly once and recovery finds the same reservation again.
func (k LaunchKey) Principal() string { return "mcp/" + k.Service }
func (k LaunchKey) Opaque() string {
	return k.Instance + "/" + strconv.FormatInt(k.Generation, 10)
}

type LaunchSpec struct {
	Key  LaunchKey
	Argv []string
	Cwd  string
	Env  []string
}

// Launcher owns worker processes for the supervisor. The daemon implements it
// on the execution journal's reserved ownership path.
type Launcher interface {
	// Launch reserves and starts the generation's line worker. An error
	// wrapping ErrLaunchUncertain means a worker may exist.
	Launch(ctx context.Context, spec LaunchSpec) (WorkerRef, error)
	// Locate reports the worker reserved for key and, once ended, its exit code.
	Locate(ctx context.Context, key LaunchKey) (WorkerRef, WorkerStatus, *int, error)
	Stop(ctx context.Context, ref WorkerRef) error
	Stderr(ctx context.Context, ref WorkerRef, limit int) ([]byte, error)
	Dial(ctx context.Context, ref WorkerRef) (LineConn, error)
}

var (
	ErrLaunchUncertain = errors.New("mcp host: worker launch outcome is uncertain")
	ErrNotFound        = errors.New("mcp service not found")
	ErrDenied          = errors.New("mcp instance belongs to another caller")
	ErrLimit           = errors.New("mcp host limit reached")
	ErrConflict        = errors.New("mcp service changed")
)

const (
	drainGrace    = 30 * time.Second
	maxLeaseTTL   = 24 * time.Hour
	minBackoff    = time.Second
	maxBackoff    = 5 * time.Minute
	sweepInterval = 30 * time.Second
	stopTimeout   = 10 * time.Second
	stderrLimit   = 1 << 20
	exitNotFound  = 127
	exitNoPermit  = 126
	exitSettle    = 2 * time.Second
)

type Config struct {
	Store    Store
	Launcher Launcher
	Now      func() time.Time
	// AccountCurrent reports whether an account still has its grant. Leases,
	// continuations and isolated instances of a retired account are retired.
	AccountCurrent func(account string) bool
	// JobActive reports whether a native job a lease names is still running
	// and belongs to account.
	JobActive func(ctx context.Context, account, job string) bool
	// HostDefaultCwd is used for definitions without a working directory.
	HostDefaultCwd string
}

type Supervisor struct {
	cfg    Config
	router *Router
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	services  map[string]Definition
	byName    map[string]string
	instances map[string]*instance

	tasks   sync.Mutex
	closing bool
}

type instance struct {
	persisting sync.Mutex

	rec      InstanceRecord
	current  *generation
	starting chan struct{}
	idle     *time.Timer
	retry    *time.Timer
	backoff  time.Duration
}

type generation struct {
	number     int64
	revision   int64
	ref        WorkerRef
	upstream   *StdioUpstream
	serverInfo json.RawMessage
	active     int
	draining   bool
}

func NewSupervisor(cfg Config) (*Supervisor, error) {
	if cfg.Store == nil || cfg.Launcher == nil {
		return nil, errors.New("mcp host: supervisor needs a store and a launcher")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AccountCurrent == nil {
		cfg.AccountCurrent = func(string) bool { return true }
	}
	router, err := NewRouter()
	if err != nil {
		return nil, err
	}
	return &Supervisor{cfg: cfg, router: router, services: map[string]Definition{}, byName: map[string]string{}, instances: map[string]*instance{}}, nil
}

// Start loads the store, adopts surviving workers by exact reservation and
// starts kept services. ctx is the supervisor's whole lifetime.
func (s *Supervisor) Start(ctx context.Context) error {
	s.ctx, s.cancel = context.WithCancel(ctx)
	defs, records, err := loadRecords(ctx, s.cfg.Store)
	if err != nil {
		return err
	}
	s.mu.Lock()
	for _, d := range defs {
		s.services[d.ID], s.byName[d.Name] = d, d.ID
	}
	for _, r := range records {
		if _, ok := s.services[r.ServiceID]; ok {
			s.instances[r.ID] = &instance{rec: r}
		}
	}
	adopt := slices.Collect(maps.Values(s.instances))
	s.mu.Unlock()
	for _, inst := range adopt {
		s.recover(ctx, inst)
	}
	s.startKept()
	s.detach(s.sweepLoop)
	return nil
}

// Close stops supervision. Workers keep running for the next daemon to adopt.
func (s *Supervisor) Close() {
	s.tasks.Lock()
	s.closing = true
	s.tasks.Unlock()
	s.cancel()
	s.mu.Lock()
	for _, inst := range s.instances {
		stopTimer(inst.idle)
		stopTimer(inst.retry)
		if inst.current != nil {
			_ = inst.current.upstream.Close()
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func stopTimer(t *time.Timer) {
	if t != nil {
		t.Stop()
	}
}

// persist writes the instance's current record. Writes of one instance are
// serialized so an older snapshot never overwrites a newer one.
func (s *Supervisor) persist(ctx context.Context, inst *instance) error {
	inst.persisting.Lock()
	defer inst.persisting.Unlock()
	s.mu.Lock()
	inst.rec.UpdatedAt = s.cfg.Now()
	rec := inst.rec
	rec.Leases = slices.Clone(rec.Leases)
	s.mu.Unlock()
	row, err := instanceRow(rec)
	if err != nil {
		return err
	}
	if err := s.cfg.Store.PutMCPInstance(ctx, row); err != nil {
		return fmt.Errorf("record MCP instance %s: %w", inst.rec.ID, err)
	}
	return nil
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mcp host: create ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Publish stores a new definition or a new revision of an existing one.
// expected is the revision the caller last saw, or zero to create.
func (s *Supervisor) Publish(ctx context.Context, d Definition, expected int64) (Definition, error) {
	d, err := Normalize(d)
	if err != nil {
		return d, err
	}
	if d.Transport == TransportHTTP && newHTTPUpstream == nil {
		return d, fmt.Errorf("service %s: %w", d.Name, ErrHTTPUnavailable)
	}
	if d.Lifecycle == LifecycleKept && d.InstancePolicy != PolicyShared {
		return d, invalid("a kept service has no caller to own an isolated instance; use --shared")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, exists := s.services[s.byName[d.Name]]
	if err := s.checkPublishLocked(d, previous, exists, expected); err != nil {
		return d, err
	}
	now := s.cfg.Now()
	d.ID, d.Revision, d.CreatedAt, d.UpdatedAt = previous.ID, previous.Revision+1, previous.CreatedAt, now
	if !exists {
		if d.ID, err = newServiceID(); err != nil {
			return d, err
		}
		d.CreatedAt = now
	}
	row, err := serviceRow(d)
	if err != nil {
		return d, err
	}
	if err := s.cfg.Store.PutMCPService(ctx, row, expected); err != nil {
		if errors.Is(err, storage.ErrMCPRevision) {
			return d, fmt.Errorf("publish %s: %w: %w", d.Name, ErrConflict, err)
		}
		return d, fmt.Errorf("publish %s: %w", d.Name, err)
	}
	s.services[d.ID], s.byName[d.Name] = d, d.ID
	s.retireRevisionLocked(d)
	if d.Lifecycle == LifecycleKept && !d.Disabled {
		s.detach(func(ctx context.Context) { s.ensureKept(ctx, d.ID) })
	}
	return d, nil
}

func (s *Supervisor) checkPublishLocked(d Definition, previous Definition, exists bool, expected int64) error {
	if exists && previous.Revision != expected {
		return fmt.Errorf("service %s is at revision %d, not %d: %w", d.Name, previous.Revision, expected, ErrConflict)
	}
	if !exists && expected != 0 {
		return fmt.Errorf("service %s: %w", d.Name, ErrNotFound)
	}
	if !exists && len(s.services) >= MaxServices {
		return fmt.Errorf("at most %d services per host: %w", MaxServices, ErrLimit)
	}
	return nil
}

// retireRevisionLocked lets old generations finish their calls, then stops
// them. New calls start a generation at the new revision.
func (s *Supervisor) retireRevisionLocked(d Definition) {
	for _, inst := range s.instances {
		gen := inst.current
		if inst.rec.ServiceID != d.ID || gen == nil || gen.revision == d.Revision {
			continue
		}
		inst.current = nil
		s.drainLocked(gen)
	}
}

func (s *Supervisor) drainLocked(gen *generation) {
	gen.draining = true
	stop := func(ctx context.Context) { s.stopGeneration(ctx, gen) }
	if gen.active == 0 {
		s.detach(stop)
		return
	}
	time.AfterFunc(drainGrace, func() { s.detach(stop) })
}

// detach runs task under the supervisor's lifetime, never a caller's: a
// caller leaving must not abandon a launch, a stop or a restart half way.
// It reports false once the supervisor is closing.
func (s *Supervisor) detach(task func(context.Context)) bool {
	s.tasks.Lock()
	defer s.tasks.Unlock()
	if s.closing {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		task(s.ctx)
	}()
	return true
}

// stopGeneration stops one replaced generation's exact worker. Stopping twice
// is harmless: the launcher stops by reservation, never by label.
func (s *Supervisor) stopGeneration(ctx context.Context, gen *generation) {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	_ = gen.upstream.Close()
	_ = s.cfg.Launcher.Stop(ctx, gen.ref)
}

// Remove deletes a definition and stops its instances.
func (s *Supervisor) Remove(ctx context.Context, name string) error {
	s.mu.Lock()
	d, ok := s.services[s.byName[name]]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("service %s: %w", name, ErrNotFound)
	}
	var stopping []*instance
	for _, inst := range s.instances {
		if inst.rec.ServiceID == d.ID {
			stopping = append(stopping, inst)
		}
	}
	s.mu.Unlock()
	for _, inst := range stopping {
		if err := s.stopInstance(ctx, inst, "service removed"); err != nil {
			return err
		}
	}
	if err := s.cfg.Store.DeleteMCPService(ctx, d.ID, d.Revision); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.services, d.ID)
	delete(s.byName, name)
	for _, inst := range stopping {
		delete(s.instances, inst.rec.ID)
	}
	return nil
}

func (s *Supervisor) service(ref string) (Definition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serviceLocked(ref)
}

func (s *Supervisor) serviceLocked(ref string) (Definition, error) {
	if d, ok := s.services[ref]; ok {
		return d, nil
	}
	if d, ok := s.services[s.byName[ref]]; ok {
		return d, nil
	}
	return Definition{}, fmt.Errorf("service %s: %w", ref, ErrNotFound)
}
