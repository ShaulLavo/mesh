package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

const (
	// demandSuperviseInterval is how quickly a crashed session shows as
	// ended and a listener whose port was taken gets bound again.
	demandSuperviseInterval = time.Second
)

// demandSessions is what on-demand routes need from the session lifecycle.
type demandSessions interface {
	startLabelled(ctx context.Context, label string, command []string, cwd string, env []string) (string, error)
	stopSession(ctx context.Context, id string) error
	// sessionExit reports whether the session has ended and, when the worker
	// recorded one, its exit code.
	sessionExit(id string) (code *int, ended bool)
	outputTail(ctx context.Context, id string) string
	findLabelled(ctx context.Context, label string) (labelledSession, bool, error)
	// pruneLabelled bounds a route's ended sessions after a start and returns
	// the ones it actually removed.
	pruneLabelled(ctx context.Context, label, current string) []string
}

// demandManager runs on-demand routes and the loopback listeners of every
// route that has them. The daemon owns the listeners; the session's worker
// owns the process, so a daemon restart drops connections but never the
// server behind them.
type demandManager struct {
	persistence *demandPersistence
	onChange    func(protocol.ServiceInfo, bool)
	ctx         context.Context
	sessions    demandSessions
	report      func(error)
	// Correlated diagnostics must bypass report's lossy queue.
	logger *log.Logger
	bind   func(port uint16) (net.Listener, error)
	dial   func(ctx context.Context, address string) error
	holder func(port uint16) string
	poll   time.Duration
	// entered runs in Enter between counting the connection and waiting for
	// its start. Tests use it to let that start finish first; nil otherwise.
	entered func()
	// stopLooks runs in Stop each time before it looks at the route: after
	// accepting the Stop and after each transition it waited for. Tests use
	// it to let a stop finish before the caller looks; nil otherwise.
	stopLooks func()
	// stopWaits runs after Stop captures a transition under the route lock.
	// Tests release its start only after that observation; nil otherwise.
	stopWaits func()
	// cleanupRetry is demandCleanupRetry, shortened by tests.
	cleanupRetry  time.Duration
	adoptionRetry time.Duration
	// now schedules kept restarts and measures stable runs; tests fake it.
	now func() time.Time

	// admission fences launches and kills at shutdown.
	admission demandAdmission
	// goroutines runs every start, stop and adoption, so tests can join them.
	goroutines sync.WaitGroup

	mu        sync.Mutex
	routes    map[string]*demandRoute
	listeners map[uint16]*demandListener
	// retiring holds removed routes that may still own a session, so the
	// next Sync retries their stop and a route added back under the same
	// name takes its old session over instead of launching beside it.
	retiring map[string]*demandRoute
	// published is a copy of routes for the connection path. A listener's
	// accept loop must never wait on mu: closing that listener under mu
	// waits for its accept loop to return.
	published atomic.Pointer[map[string]*demandRoute]
	// closed is atomic because route locks read it; the manager lock is
	// never taken under a route lock.
	closed atomic.Bool
}

func newDemandManager(ctx context.Context, sessions demandSessions, report func(error)) *demandManager {
	if report == nil {
		report = func(error) {}
	}
	return &demandManager{
		ctx: ctx, sessions: sessions, report: report, logger: log.Default(),
		bind: bindLoopback, dial: dialUpstream, holder: portHolder, poll: demandPollInterval, cleanupRetry: demandCleanupRetry, adoptionRetry: demandSuperviseInterval,
		now:    time.Now,
		routes: make(map[string]*demandRoute), listeners: make(map[uint16]*demandListener),
		retiring: make(map[string]*demandRoute),
	}
}

// Reserve binds every listener service needs that the manager does not
// already hold for it. It runs before the service is committed, so a port
// someone else holds refuses the request instead of producing a route that
// cannot be reached. Sync releases whatever the committed services do not use.
func (m *demandManager) Reserve(service meshserve.Service) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed.Load() {
		return errors.New("daemon: on-demand serving is shut down")
	}
	var bound []uint16
	for _, listen := range service.Listens {
		if existing := m.listeners[listen.Public]; existing != nil {
			if existing.owner != service.Name {
				m.releaseLocked(bound)
				return fmt.Errorf("daemon: port %d is already a listener of route %s", listen.Public, existing.route)
			}
			continue
		}
		listener, err := m.listenLocked(service.Name, service.Route(), listen.Public, true)
		if err != nil {
			m.releaseLocked(bound)
			return err
		}
		m.listeners[listen.Public] = listener
		bound = append(bound, listen.Public)
	}
	return nil
}

func (m *demandManager) releaseLocked(ports []uint16) {
	for _, port := range ports {
		if listener := m.listeners[port]; listener != nil {
			listener.close()
			delete(m.listeners, port)
		}
	}
}

// Sync makes routes and listeners match the committed services.
func (m *demandManager) Sync(services []meshserve.Service) {
	release, err := m.BeginDefinition(m.ctx)
	if err != nil {
		m.report(err)
		return
	}
	defer release()
	m.syncAccepted(services)
}

func (m *demandManager) syncAccepted(services []meshserve.Service) {
	m.mu.Lock()
	if m.closed.Load() {
		m.mu.Unlock()
		return
	}
	wanted := make(map[string]meshserve.Service)
	for _, service := range services {
		if service.Demand != nil || len(service.Listens) > 0 {
			wanted[service.Name] = service
		}
	}
	definitions := m.syncRoutesLocked(wanted)
	m.restoreRetiringRuntime()
	retiring := make([]*demandRoute, 0, len(m.retiring))
	for _, route := range m.retiring {
		retiring = append(retiring, route)
	}
	m.publishLocked()
	listens := make(map[uint16]meshserve.Service)
	for _, service := range wanted {
		for _, listen := range service.Listens {
			listens[listen.Public] = service
		}
	}
	for port, listener := range m.listeners {
		service, keep := listens[port]
		if !keep || service.Name != listener.owner {
			listener.close()
			delete(m.listeners, port)
		}
	}
	m.bindMissingLocked(listens)
	m.mu.Unlock()
	m.syncRuntimeBindings(services)
	for _, definition := range definitions {
		m.syncRoute(definition.route, definition.service, definition.fresh)
	}
	for _, route := range retiring {
		m.retireRoute(route)
	}
}

type demandDefinition struct {
	route   *demandRoute
	service meshserve.Service
	fresh   bool
}

func (m *demandManager) syncRoutesLocked(wanted map[string]meshserve.Service) []demandDefinition {
	for name, route := range m.routes {
		if _, keep := wanted[name]; !keep {
			delete(m.routes, name)
			m.retiring[name] = route
		}
	}
	var definitions []demandDefinition
	for name, service := range wanted {
		route := m.routes[name]
		readded := route == nil
		if readded {
			route = m.retiring[name]
			delete(m.retiring, name)
		}
		fresh := route == nil
		if fresh {
			route = newDemandRoute(m, service, false)
		}
		if !fresh {
			route = route.bindDefinition(service, readded)
		}
		m.routes[name] = route
		definitions = append(definitions, demandDefinition{route, service, fresh})
	}
	return definitions
}

func newDemandRoute(manager *demandManager, service meshserve.Service, removed bool) *demandRoute {
	return &demandRoute{demandRouteState: &demandRouteState{
		manager: manager, service: service, removed: removed,
		state: protocol.DemandStopped, unbound: map[uint16]string{},
	}}
}

func (r *demandRoute) bindDefinition(service meshserve.Service, readded bool) *demandRoute {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !readded && r.service.Equal(service) {
		return r
	}
	r.activeGeneration++
	return &demandRoute{demandRouteState: r.demandRouteState, generation: r.activeGeneration}
}

func (m *demandManager) syncRoute(route *demandRoute, service meshserve.Service, fresh bool) {
	if fresh {
		route.initRuntime()
		return
	}
	route.redefine(service)
}

func (m *demandManager) retireRoute(route *demandRoute) {
	if m.persistence != nil {
		record, exists := route.runtimeRecord()
		route.mu.Lock()
		uninitialized := route.owned == ownsNothing && route.pending == nil && route.sessionID == "" && exists && record.Attempt != nil
		route.mu.Unlock()
		if uninitialized {
			route.initRuntime()
			return
		}
	}
	if !route.retire() {
		return
	}
	if m.persistence != nil {
		m.deleteRuntimeRoute(route)
		return
	}
	m.mu.Lock()
	if m.retiring[route.service.Name] == route {
		delete(m.retiring, route.service.Name)
	}
	m.mu.Unlock()
}

func (m *demandManager) bindMissingLocked(listens map[uint16]meshserve.Service) {
	for port, service := range listens {
		route := m.routes[service.Name]
		if m.listeners[port] != nil {
			route.markBound(port)
			continue
		}
		// Naming the holder scans every process; a port still held since the
		// last attempt keeps the reason already recorded.
		listener, err := m.listenLocked(service.Name, service.Route(), port, !route.isUnbound(port))
		if err != nil {
			if errors.Is(err, errPortStillHeld) {
				continue
			}
			if route.markUnbound(port, err) {
				m.report(fmt.Errorf("daemon: route %s: %w", service.Route(), err))
			}
			continue
		}
		m.listeners[port] = listener
		route.markBound(port)
	}
}

func (m *demandManager) route(name string) *demandRoute {
	routes := m.published.Load()
	if routes == nil {
		return nil
	}
	return (*routes)[name]
}

func (m *demandManager) publishLocked() {
	routes := make(map[string]*demandRoute, len(m.routes))
	for name, route := range m.routes {
		routes[name] = route
	}
	m.published.Store(&routes)
}

// Enter admits one tailnet request to an on-demand route.
func (m *demandManager) Enter(ctx context.Context, name string) (func(), error) {
	route := m.route(name)
	if route == nil {
		return nil, fmt.Errorf("route /%s is not on-demand", name)
	}
	release, started := route.hold(causeTailnetRequest)
	if m.entered != nil {
		m.entered()
	}
	// The start this connection caused may already have failed by the time
	// it waits; answering with that failure keeps one connection from
	// starting the session twice.
	if err := route.awaitStart(ctx, started, causeTailnetRequest); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// Start starts an on-demand route now and waits until it is ready. It ends a
// kept route's hold and restart delay.
func (m *demandManager) Start(ctx context.Context, name string) error {
	route := m.route(name)
	if route == nil || !route.onDemand() {
		return fmt.Errorf("daemon: route %s is not on-demand", name)
	}
	return m.control(ctx, func() error { return route.start(ctx) })
}

// Stop stops an on-demand route now. Its listeners stay bound. A finite
// route starts again on its next connection; a kept route stays stopped until
// Start, a change of its definition, or the next daemon.
func (m *demandManager) Stop(ctx context.Context, name string) error {
	route := m.route(name)
	if route == nil || !route.onDemand() {
		return fmt.Errorf("daemon: route %s is not on-demand", name)
	}
	return m.control(ctx, func() error { return route.stop(ctx) })
}

func (m *demandManager) control(ctx context.Context, run func() error) error {
	if m.persistence == nil {
		return run()
	}
	result := make(chan error, 1)
	m.goroutines.Go(func() { result <- run() })
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return fmt.Errorf("daemon: waiting for service control: %w", ctx.Err())
	}
}

func (m *demandManager) beforeStopLook() {
	if m.stopLooks != nil {
		m.stopLooks()
	}
}

func (m *demandManager) beforeStopWait() {
	if m.stopWaits != nil {
		m.stopWaits()
	}
}

// Status reports what a route with listeners or a recipe is doing now.
func (m *demandManager) Status(name string) *protocol.ServiceDemand {
	route := m.route(name)
	if route == nil {
		return nil
	}
	return route.status()
}

// Run watches running sessions for an exit nobody asked for, restarts kept
// routes when their delay passes, and retries listeners whose port was held,
// until ctx ends.
func (m *demandManager) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.supervise()
		}
	}
}

func (m *demandManager) supervise() {
	m.reconcileRuntime()
	m.mu.Lock()
	routes := make([]*demandRoute, 0, len(m.routes))
	for _, route := range m.routes {
		routes = append(routes, route)
	}
	retiring := make([]*demandRoute, 0, len(m.retiring))
	for _, route := range m.retiring {
		retiring = append(retiring, route)
	}
	listens := make(map[uint16]meshserve.Service)
	for _, route := range routes {
		service := route.definition()
		for _, listen := range service.Listens {
			listens[listen.Public] = service
		}
	}
	if !m.closed.Load() {
		m.bindMissingLocked(listens)
	}
	m.mu.Unlock()
	if m.closed.Load() {
		return
	}
	for _, route := range retiring {
		m.retireRoute(route)
	}
	for _, route := range routes {
		route.checkSession()
		route.retryCleanup()
		route.retryAdoption()
		route.keepRunning()
	}
}

// Retiring describes removed routes that may still own a running session.
// They are no longer registered, so without this their owner could not see
// what is left running or why.
func (m *demandManager) Retiring() []protocol.ServiceInfo {
	m.mu.Lock()
	routes := make([]*demandRoute, 0, len(m.retiring))
	for _, route := range m.retiring {
		routes = append(routes, route)
	}
	m.mu.Unlock()
	var infos []protocol.ServiceInfo
	for _, route := range routes {
		if info, owning := route.retiringInfo(); owning {
			infos = append(infos, info)
		}
	}
	slices.SortFunc(infos, func(a, b protocol.ServiceInfo) int { return strings.Compare(a.Name, b.Name) })
	return infos
}

// Close releases every listener. Sessions keep running: the next daemon
// adopts them by their label. Close first refuses every new launch and kill,
// then waits for those already admitted, holding no lock while it waits. A
// launch admitted before shutdown finishes creating its worker, which the next
// daemon adopts; a kill already sent may finish.
func (m *demandManager) Close() {
	if m.persistence != nil {
		m.persistence.mu.Lock()
	}
	m.admission.mu.Lock()
	m.admission.closed = true
	m.closed.Store(true)
	m.admission.mu.Unlock()
	if m.persistence != nil {
		m.persistence.mu.Unlock()
	}
	m.mu.Lock()
	for port, listener := range m.listeners {
		listener.close()
		delete(m.listeners, port)
	}
	for _, route := range m.routes {
		route.mu.Lock()
		route.cancelIdleLocked()
		route.mu.Unlock()
	}
	m.mu.Unlock()
	m.admission.phases.Wait()
	if m.persistence != nil {
		m.persistence.phases.Wait()
	}
}
