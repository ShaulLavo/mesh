package mcphost

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
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
	// PlainPipe runs the process on a plain pipe worker (stdin closed,
	// stdout and stderr captured) instead of the MCP line worker.
	PlainPipe bool
}

// WorkerWaiter is optionally implemented by a Launcher to signal when a
// managed worker exits without polling.
type WorkerWaiter interface {
	Wait(ctx context.Context, ref WorkerRef) error
}

// ManagedHTTPGuard verifies loopback port availability before spawn and proves
// listener ownership for the exact reserved worker before any HTTP request or
// credential header is written.
type ManagedHTTPGuard interface {
	CheckPortFree(ctx context.Context, targets []netip.AddrPort) error
	VerifyManagedDial(ctx context.Context, ref WorkerRef, target netip.AddrPort, dial func() (net.Conn, error)) (net.Conn, error)
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
	// ErrNotOwned reports a process lifecycle operation against an external
	// HTTP server for which Mesh owns no process.
	ErrNotOwned = errors.New("external HTTP service has no Mesh-managed process")
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
	// HostID is the owning host's canonical Mesh ID, used to validate mi1
	// instance handles on dispatch and management controls.
	HostID string
	// GrantCurrent reports whether grant is still the account's current
	// grant incarnation. Leases, continuations and isolated instances of a
	// retired grant are retired, even if the device was approved again.
	GrantCurrent func(account, grant string) bool
	// JobActive reports whether a native job a lease names is still running
	// and belongs to the caller: the application app when it has one, or
	// the account's own work otherwise.
	JobActive func(ctx context.Context, account, app, job string) bool
	// HostDefaultCwd is used for definitions without a working directory.
	HostDefaultCwd string
	// SweepInterval is how often leases and retired grants are swept; zero
	// means 30 seconds.
	SweepInterval time.Duration
	// ListingChanged receives each service's new listing revision, for the
	// mcp topic of state.watch.
	ListingChanged func(ListingRevision)
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
	// endpoints maps each claimed managed-HTTP loopback target to the instance
	// ID that currently owns, starts, drains or has a pending stop on it.
	endpoints map[netip.AddrPort]string

	tasks   sync.Mutex
	closing bool
}

type drainingGen struct {
	gen  *generation
	done chan struct{}
	err  error
}

type discoverySnapshot struct {
	serverInfo   json.RawMessage
	capabilities json.RawMessage
}

type instance struct {
	persisting sync.Mutex

	rec             InstanceRecord
	current         *generation
	starting        chan struct{}
	draining        []*drainingGen
	idle            *time.Timer
	retry           *time.Timer
	backoff         time.Duration
	lastProbeState  string
	lastProbeReason string
	// stops counts explicit stops, so callers waiting across one give up.
	stops int
}

type generation struct {
	number   int64
	revision int64
	ref      WorkerRef
	upstream Upstream
	// tools holds the compiled x-mcp-header rules of an HTTP upstream's
	// current tools/list, read by the H5 client for every tools/call.
	tools     atomic.Pointer[ToolHeaders]
	discovery atomic.Pointer[discoverySnapshot]
	active    int
	requests  int
	listens   int
	draining  bool
	stopDrain func(context.Context)
}

func (g *generation) setDiscovery(serverInfo, capabilities json.RawMessage) {
	g.discovery.Store(&discoverySnapshot{
		serverInfo:   bytes.Clone(serverInfo),
		capabilities: bytes.Clone(capabilities),
	})
}

func (g *generation) discoveryInfo() (json.RawMessage, json.RawMessage) {
	snap := g.discovery.Load()
	if snap == nil {
		return nil, nil
	}
	return snap.serverInfo, snap.capabilities
}

func NewSupervisor(cfg Config) (*Supervisor, error) {
	if cfg.Store == nil || cfg.Launcher == nil {
		return nil, errors.New("mcp host: supervisor needs a store and a launcher")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.GrantCurrent == nil {
		cfg.GrantCurrent = func(string, string) bool { return true }
	}
	router, err := NewRouter()
	if err != nil {
		return nil, err
	}
	router.listings.notify = cfg.ListingChanged
	return &Supervisor{
		cfg:       cfg,
		router:    router,
		services:  map[string]Definition{},
		byName:    map[string]string{},
		instances: map[string]*instance{},
		endpoints: map[netip.AddrPort]string{},
	}, nil
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
		s.loadInstanceLocked(r)
	}
	adopt := slices.Collect(maps.Values(s.instances))
	s.mu.Unlock()
	// Recovery probes each adopted server, which can take its whole ready
	// timeout, so it runs in the background. Holding the start slot makes a
	// call that arrives meanwhile wait for the adoption instead of launching.
	for _, inst := range adopt {
		if s.claimStart(inst) {
			s.detach(func(ctx context.Context) {
				defer s.finishStart(inst)
				s.recover(ctx, inst)
			})
		}
	}
	s.startKept()
	s.detach(s.sweepLoop)
	return nil
}

func (s *Supervisor) loadInstanceLocked(r InstanceRecord) {
	d, ok := s.services[r.ServiceID]
	if !ok {
		return
	}
	inst := &instance{rec: r}
	s.instances[r.ID] = inst
	// Pre-claim managed HTTP endpoints for instances that may have a live or
	// pending-stop worker until recovery settles them.
	if d.Owned() && d.Transport == TransportHTTP && (r.Generation > 0 || r.StopPending != nil) {
		if ep, err := parseHTTPEndpoint(d.URL); err == nil {
			_ = s.claimEndpointsLocked(inst.rec.ID, ep.targets)
		}
	}
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

// claimStart takes the instance's start slot, which launches and recovery
// share, and reports false when another holder has it.
func (s *Supervisor) claimStart(inst *instance) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst.starting != nil {
		return false
	}
	inst.starting = make(chan struct{})
	return true
}

func (s *Supervisor) finishStart(inst *instance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(inst.starting)
	inst.starting = nil
	s.maybeReleaseEndpointsLocked(inst)
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
	d, err := publishable(d)
	if err != nil {
		return d, err
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
	s.router.listings.published(d)
	return d, nil
}

// publishable adds the rules this host's runtime enforces on top of Normalize.
func publishable(d Definition) (Definition, error) {
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
		s.drainLocked(inst, gen)
	}
}

func (s *Supervisor) drainLocked(inst *instance, gen *generation) {
	gen.draining = true
	drain := &drainingGen{gen: gen, done: make(chan struct{})}
	inst.draining = append(inst.draining, drain)
	var once sync.Once
	stop := func(ctx context.Context) {
		once.Do(func() {
			defer close(drain.done)
			err := s.stopGeneration(ctx, gen)
			s.mu.Lock()
			drain.err = err
			if err == nil {
				inst.draining = slices.DeleteFunc(inst.draining, func(d *drainingGen) bool { return d == drain })
				s.maybeReleaseEndpointsLocked(inst)
			} else if gen.ref.SessionID != "" {
				ref := gen.ref
				inst.rec.StopPending = &ref
				inst.rec.State = StateUncertain
				inst.rec.Reason = "previous worker termination is unresolved: " + err.Error()
			}
			s.mu.Unlock()
			if err != nil {
				_ = s.persist(ctx, inst)
			}
		})
	}
	gen.stopDrain = stop
	if gen.active == 0 {
		s.detach(stop)
		return
	}
	time.AfterFunc(drainGrace, func() { s.detach(stop) })
}

func (s *Supervisor) claimEndpointsLocked(instID string, targets []netip.AddrPort) error {
	for _, target := range targets {
		if owner, taken := s.endpoints[target]; taken && owner != instID {
			return fmt.Errorf("managed HTTP endpoint %s is already claimed by another instance", target)
		}
	}
	for _, target := range targets {
		s.endpoints[target] = instID
	}
	return nil
}

func (s *Supervisor) maybeReleaseEndpointsLocked(inst *instance) {
	if inst.current != nil || inst.starting != nil || len(inst.draining) > 0 || inst.rec.StopPending != nil {
		return
	}
	s.releaseEndpointsLocked(inst.rec.ID)
}

func (s *Supervisor) releaseEndpointsLocked(instID string) {
	for target, owner := range s.endpoints {
		if owner == instID {
			delete(s.endpoints, target)
		}
	}
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
func (s *Supervisor) stopGeneration(ctx context.Context, gen *generation) error {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	_ = gen.upstream.Close()
	return s.stopWorker(ctx, gen.ref)
}

// stopWorker stops the generation's process, if Mesh owns one.
func (s *Supervisor) stopWorker(ctx context.Context, ref WorkerRef) error {
	if ref.SessionID == "" {
		return nil
	}
	if err := s.cfg.Launcher.Stop(ctx, ref); err != nil {
		return fmt.Errorf("stop worker %s: %w", ref.SessionID, err)
	}
	return nil
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
	// Withdraw the service first, so no call starts an instance meanwhile.
	delete(s.services, d.ID)
	delete(s.byName, name)
	s.mu.Unlock()
	err := s.stopAll(ctx, stopping)
	if err == nil {
		err = s.cfg.Store.DeleteMCPService(ctx, d.ID, d.Revision)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.services[d.ID], s.byName[name] = d, d.ID
		return fmt.Errorf("remove %s: %w", name, err)
	}
	for _, inst := range stopping {
		delete(s.instances, inst.rec.ID)
	}
	s.router.listings.removed(d)
	return nil
}

func (s *Supervisor) stopAll(ctx context.Context, instances []*instance) error {
	for _, inst := range instances {
		if err := s.stopInstance(ctx, inst, "service removed"); err != nil {
			return err
		}
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
