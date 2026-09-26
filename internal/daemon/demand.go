package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	meshserve "github.com/shaul/mesh/internal/serve"
)

const (
	demandPollInterval = 100 * time.Millisecond
	demandProbeTimeout = 250 * time.Millisecond
	// demandSuperviseInterval is how quickly a crashed session shows as
	// ended and a listener whose port was taken gets bound again.
	demandSuperviseInterval = time.Second
	// demandListenerIdleTimeout closes a keep-alive connection nobody is
	// using, so an open browser tab without a live socket lets the route idle.
	demandListenerIdleTimeout = 2 * time.Minute
	demandStopTimeout         = 30 * time.Second
	demandOutputTimeout       = 5 * time.Second
	demandFailureLines        = 20
	demandFailureLineBytes    = 240
)

// demandSessions is what on-demand routes need from the session lifecycle.
type demandSessions interface {
	startLabelled(ctx context.Context, label string, command []string, cwd string, env []string) (string, error)
	stopSession(ctx context.Context, id string) error
	// sessionExit reports whether the session has ended and, when the worker
	// recorded one, its exit code.
	sessionExit(id string) (code *int, ended bool)
	outputTail(ctx context.Context, id string) string
	findLabelled(ctx context.Context, label string) (string, bool, error)
	forgetLabelled(ctx context.Context, label, keep string)
}

// demandManager runs on-demand routes and the loopback listeners of every
// route that has them. The daemon owns the listeners; the session's worker
// owns the process, so a daemon restart drops connections but never the
// server behind them.
type demandManager struct {
	ctx      context.Context
	sessions demandSessions
	report   func(error)
	bind     func(port uint16) (net.Listener, error)
	dial     func(ctx context.Context, address string) error
	holder   func(port uint16) string
	poll     time.Duration

	mu        sync.Mutex
	routes    map[string]*demandRoute
	listeners map[uint16]*demandListener
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
		ctx: ctx, sessions: sessions, report: report,
		bind: bindLoopback, dial: dialUpstream, holder: portHolder, poll: demandPollInterval,
		routes: make(map[string]*demandRoute), listeners: make(map[uint16]*demandListener),
	}
}

func bindLoopback(port uint16) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
}

func dialUpstream(ctx context.Context, address string) error {
	ctx, cancel := context.WithTimeout(ctx, demandProbeTimeout)
	defer cancel()
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return connection.Close()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed.Load() {
		return
	}
	wanted := make(map[string]meshserve.Service)
	for _, service := range services {
		if service.Demand != nil || len(service.Listens) > 0 {
			wanted[service.Name] = service
		}
	}
	for name, route := range m.routes {
		if _, keep := wanted[name]; !keep {
			route.retire()
			delete(m.routes, name)
		}
	}
	for name, service := range wanted {
		if route := m.routes[name]; route != nil {
			route.redefine(service)
			continue
		}
		route := &demandRoute{manager: m, service: service, state: protocol.DemandStopped, unbound: map[uint16]string{}}
		m.routes[name] = route
		route.adopt()
	}
	m.publishLocked()
	listens := make(map[uint16]meshserve.Service)
	for _, service := range wanted {
		for _, listen := range service.Listens {
			listens[listen.Public] = service
		}
	}
	for port, listener := range m.listeners {
		if service, keep := listens[port]; !keep || service.Name != listener.owner {
			listener.close()
			delete(m.listeners, port)
		}
	}
	m.bindMissingLocked(listens)
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

// errPortStillHeld is a retry that found the port taken again, when naming
// the holder was not asked for.
var errPortStillHeld = errors.New("port still held")

func (m *demandManager) listenLocked(owner, route string, port uint16, describe bool) (*demandListener, error) {
	raw, err := m.bind(port)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			if !describe {
				return nil, errPortStillHeld
			}
			return nil, fmt.Errorf("port %d is held by %s", port, m.holder(port))
		}
		return nil, fmt.Errorf("listen on 127.0.0.1:%d: %w", port, err)
	}
	listener := &demandListener{manager: m, owner: owner, route: route, port: port}
	listener.server = &http.Server{
		Handler:           http.HandlerFunc(listener.serveHTTP),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       demandListenerIdleTimeout,
	}
	listener.listener = &countingListener{Listener: raw, hold: func() func() {
		if route := m.route(owner); route != nil {
			return route.hold()
		}
		return func() {}
	}}
	go func() {
		if err := listener.server.Serve(listener.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			m.report(fmt.Errorf("daemon: route %s listener on port %d: %w", route, port, err))
		}
	}()
	return listener, nil
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
	release := route.hold()
	if err := route.ready(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// Start starts an on-demand route now and waits until it is ready.
func (m *demandManager) Start(ctx context.Context, name string) error {
	route := m.route(name)
	if route == nil || !route.onDemand() {
		return fmt.Errorf("daemon: route %s is not on-demand", name)
	}
	return route.ready(ctx)
}

// Stop stops an on-demand route now. Its listeners stay bound, so the next
// connection starts it again.
func (m *demandManager) Stop(ctx context.Context, name string) error {
	route := m.route(name)
	if route == nil || !route.onDemand() {
		return fmt.Errorf("daemon: route %s is not on-demand", name)
	}
	return route.stop(ctx)
}

// Status reports what a route with listeners or a recipe is doing now.
func (m *demandManager) Status(name string) *protocol.ServiceDemand {
	route := m.route(name)
	if route == nil {
		return nil
	}
	return route.status()
}

// Run watches running sessions for an exit nobody asked for and retries
// listeners whose port was held, until ctx ends.
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
	m.mu.Lock()
	routes := make([]*demandRoute, 0, len(m.routes))
	for _, route := range m.routes {
		routes = append(routes, route)
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
	for _, route := range routes {
		route.checkSession()
	}
}

// Close releases every listener. Sessions keep running: the next daemon
// adopts them by their label.
func (m *demandManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed.Store(true)
	for port, listener := range m.listeners {
		listener.close()
		delete(m.listeners, port)
	}
	for _, route := range m.routes {
		route.mu.Lock()
		route.cancelIdleLocked()
		route.mu.Unlock()
	}
}

// awaitReady waits until every upstream of service accepts, the session
// ends, or the ready timeout passes.
func (m *demandManager) awaitReady(ctx context.Context, service meshserve.Service, id string) error {
	deadline := time.Now().Add(service.Demand.ReadyTimeout)
	ports := service.UpstreamPorts()
	for {
		if code, ended := m.sessions.sessionExit(id); ended {
			return m.startFailure(ctx, service, id, exitDescription(code), false)
		}
		if m.allAccept(ctx, ports) {
			return nil
		}
		if !time.Now().Before(deadline) {
			reason := fmt.Sprintf("%s did not accept within %s", portList(ports), service.Demand.ReadyTimeout)
			return m.startFailure(ctx, service, id, reason, true)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("route %s: %w", service.Route(), ctx.Err())
		case <-time.After(m.poll):
		}
	}
}

func (m *demandManager) allAccept(ctx context.Context, ports []string) bool {
	for _, port := range ports {
		if m.dial(ctx, net.JoinHostPort("127.0.0.1", port)) != nil {
			return false
		}
	}
	return true
}

// startFailure is the answer a waiting connection gets. It names the route,
// what went wrong and what the command printed, because the person reading
// it is looking at a browser tab, not the session.
func (m *demandManager) startFailure(ctx context.Context, service meshserve.Service, id, reason string, stop bool) error {
	// A worker that accepts the request and never answers must not keep the
	// route starting forever.
	tailCtx, cancelTail := context.WithTimeout(ctx, demandOutputTimeout)
	tail := m.sessions.outputTail(tailCtx, id)
	cancelTail()
	if stop {
		stopCtx, cancel := context.WithTimeout(m.ctx, demandStopTimeout)
		if err := m.sessions.stopSession(stopCtx, id); err != nil {
			reason += fmt.Sprintf("; stopping it failed: %v", err)
		} else {
			reason += "; stopped it"
		}
		cancel()
	}
	message := fmt.Sprintf("route %s did not start: %s (session %s, command %q)", service.Route(), reason, id, service.Demand.Command)
	if tail != "" {
		message += "\n\nlast output:\n" + tail
	}
	return demandFailure{summary: fmt.Sprintf("%s (session %s)", reason, id), message: message}
}

// demandFailure keeps the one-line summary `mesh serve ls` shows apart from
// the full text a waiting connection gets.
type demandFailure struct{ summary, message string }

func (f demandFailure) Error() string { return f.message }

func exitDescription(code *int) string {
	if code == nil {
		return "the session ended without an exit status"
	}
	return fmt.Sprintf("the command exited with status %d", *code)
}

func portList(ports []string) string {
	if len(ports) == 1 {
		return "port " + ports[0]
	}
	return "ports " + strings.Join(ports, ", ")
}

// lastLines keeps the end of a session's output as plain text: the last
// non-blank lines, each cut to a readable width.
func lastLines(text string, count, width int) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		// A bare carriage return redraws the line; only what was left counts.
		if index := strings.LastIndexByte(line, '\r'); index >= 0 {
			line = line[index+1:]
		}
		line = strings.TrimRight(strings.ToValidUTF8(line, "?"), " \t")
		if line == "" {
			continue
		}
		if len(line) > width {
			cut := width
			for cut > 0 && !isRuneStart(line[cut]) {
				cut--
			}
			line = line[:cut] + "…"
		}
		lines = append(lines, line)
	}
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, "\n")
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// demandRoute is one route's live state. Its mutex orders every state change;
// starting and stopping run outside it and report back through a transition.
type demandRoute struct {
	manager *demandManager

	mu          sync.Mutex
	service     meshserve.Service
	removed     bool
	restart     bool
	state       string
	sessionID   string
	failure     string
	connections int
	idleTimer   *time.Timer
	idleToken   uint64
	pending     *demandTransition
	unbound     map[uint16]string
}

// demandTransition is one start or stop in progress. err is written before
// done closes, so a waiter reads it after <-done without the route lock.
type demandTransition struct {
	starting bool
	done     chan struct{}
	err      error
}

func (r *demandRoute) definition() meshserve.Service {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.service
}

func (r *demandRoute) onDemand() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.service.Demand != nil
}

// adopt takes over a session a previous daemon started for this route. The
// idle clock starts over at a full window: this daemon never saw the
// connections the old one counted.
func (r *demandRoute) adopt() {
	if r.service.Demand == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.manager.ctx, 5*time.Second)
	defer cancel()
	id, found, err := r.manager.sessions.findLabelled(ctx, r.service.Label())
	if err != nil {
		r.manager.report(fmt.Errorf("daemon: route %s: find its running session: %w", r.service.Route(), err))
		return
	}
	if !found {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = protocol.DemandRunning
	r.sessionID = id
	r.armIdleLocked()
}

func (r *demandRoute) redefine(service meshserve.Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	previous := r.service
	r.service = service
	for port := range r.unbound {
		if !slices.ContainsFunc(service.Listens, func(listen meshserve.Listen) bool { return listen.Public == port }) {
			delete(r.unbound, port)
		}
	}
	if sameLaunch(previous.Demand, service.Demand) && previous.Label() == service.Label() {
		// Only the timing changed. The idle clock picks up the new window
		// when it next starts; a running idle clock starts over with it.
		if r.idleTimer != nil {
			r.armIdleLocked()
		}
		return
	}
	// A new recipe takes effect on the next start; the running session was
	// started from the old one, so it stops now rather than lingering. A new
	// label counts too: the next daemon would not recognise the old one and
	// would leave it running unowned.
	switch r.state {
	case protocol.DemandStarting:
		r.restart = true
	case protocol.DemandRunning:
		r.beginStopLocked()
	case protocol.DemandFailed:
		r.state = protocol.DemandStopped
		r.failure = ""
	}
}

// sameLaunch reports whether two recipes start the same process. Idle and
// ready timeouts govern when it runs, not what runs.
func sameLaunch(a, b *meshserve.Demand) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Command == b.Command && a.Cwd == b.Cwd && slices.Equal(a.Env, b.Env)
}

func (r *demandRoute) retire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = true
	r.cancelIdleLocked()
	if r.state == protocol.DemandRunning {
		r.beginStopLocked()
	}
}

// hold counts one open connection until the returned release runs. A
// connection to a stopped or failed route starts it: this is the wake signal.
func (r *demandRoute) hold() func() {
	r.mu.Lock()
	r.connections++
	r.cancelIdleLocked()
	if r.service.Demand != nil && !r.removed && r.pending == nil &&
		(r.state == protocol.DemandStopped || r.state == protocol.DemandFailed) {
		r.beginStartLocked()
	}
	r.mu.Unlock()
	var once sync.Once
	return func() { once.Do(r.release) }
}

func (r *demandRoute) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connections--
	if r.connections == 0 {
		r.armIdleLocked()
	}
}

// ready waits until the route's session accepts on every upstream. A start
// that fails answers everyone who waited on it; the next call tries again.
func (r *demandRoute) ready(ctx context.Context) error {
	for {
		r.mu.Lock()
		route := r.service.Route()
		if r.removed {
			r.mu.Unlock()
			return fmt.Errorf("route %s was removed", route)
		}
		if r.service.Demand == nil || r.state == protocol.DemandRunning && r.pending == nil {
			r.mu.Unlock()
			return nil
		}
		transition := r.pending
		if transition == nil {
			r.beginStartLocked()
			transition = r.pending
		}
		r.mu.Unlock()
		select {
		case <-transition.done:
		case <-ctx.Done():
			return fmt.Errorf("waiting for route %s: %w", route, ctx.Err())
		}
		if transition.starting {
			return transition.err
		}
	}
}

func (r *demandRoute) stop(ctx context.Context) error {
	for {
		r.mu.Lock()
		route := r.service.Route()
		transition := r.pending
		if transition == nil {
			if r.state != protocol.DemandRunning {
				r.mu.Unlock()
				return nil
			}
			r.cancelIdleLocked()
			r.beginStopLocked()
			transition = r.pending
		}
		r.mu.Unlock()
		select {
		case <-transition.done:
		case <-ctx.Done():
			return fmt.Errorf("stopping route %s: %w", route, ctx.Err())
		}
		if !transition.starting {
			return transition.err
		}
	}
}

func (r *demandRoute) beginStartLocked() {
	transition := &demandTransition{starting: true, done: make(chan struct{})}
	r.pending = transition
	r.state = protocol.DemandStarting
	r.failure = ""
	r.restart = false
	service := r.service
	go r.runStart(transition, service)
}

func (r *demandRoute) runStart(transition *demandTransition, service meshserve.Service) {
	manager := r.manager
	command := []string{hostShell(), "-lc", service.Demand.Command}
	id, err := manager.sessions.startLabelled(manager.ctx, service.Label(), command, service.Demand.Cwd, service.Demand.Env)
	if err != nil {
		err = demandFailure{
			summary: fmt.Sprintf("could not start a session: %v", err),
			message: fmt.Sprintf("route %s did not start: could not start a session for %q: %v", service.Route(), service.Demand.Command, err),
		}
	} else {
		err = manager.awaitReady(manager.ctx, service, id)
	}
	r.finishStart(transition, id, err)
	if id != "" {
		// Each start is a new session. Earlier ones for this route are over
		// and would otherwise pile up in `mesh ls --all` and on disk, which a
		// browser retrying a broken server would do once a second. The latest
		// stays, failed or not: its output is what explains the failure.
		manager.sessions.forgetLabelled(manager.ctx, service.Label(), id)
	}
}

func (r *demandRoute) finishStart(transition *demandTransition, id string, err error) {
	r.mu.Lock()
	r.pending = nil
	if id != "" {
		r.sessionID = id
	}
	switch {
	case err != nil:
		r.state = protocol.DemandFailed
		r.failure = err.Error()
		var failure demandFailure
		if errors.As(err, &failure) {
			r.failure = failure.summary
		}
		transition.err = err
	case r.removed || r.restart:
		r.state = protocol.DemandRunning
		r.beginStopLocked()
		if r.removed {
			transition.err = fmt.Errorf("route %s was removed while it started", r.service.Route())
		} else {
			transition.err = fmt.Errorf("route %s changed while it started; retry", r.service.Route())
		}
	default:
		r.state = protocol.DemandRunning
		if r.connections == 0 {
			r.armIdleLocked()
		}
	}
	r.mu.Unlock()
	close(transition.done)
}

func (r *demandRoute) beginStopLocked() {
	transition := &demandTransition{done: make(chan struct{})}
	r.pending = transition
	r.state = protocol.DemandStopping
	id := r.sessionID
	route := r.service.Route()
	go func() {
		ctx, cancel := context.WithTimeout(r.manager.ctx, demandStopTimeout)
		err := r.manager.sessions.stopSession(ctx, id)
		cancel()
		if err != nil {
			err = fmt.Errorf("daemon: route %s: stop session %s: %w", route, id, err)
			r.manager.report(err)
		}
		r.mu.Lock()
		r.pending = nil
		r.state = protocol.DemandStopped
		if err != nil {
			r.state = protocol.DemandFailed
			r.failure = err.Error()
		}
		transition.err = err
		r.mu.Unlock()
		close(transition.done)
	}()
}

func (r *demandRoute) armIdleLocked() {
	r.cancelIdleLocked()
	if r.service.Demand == nil || r.state != protocol.DemandRunning || r.pending != nil || r.manager.closed.Load() {
		return
	}
	token := r.idleToken
	r.idleTimer = time.AfterFunc(r.service.Demand.Idle, func() { r.idleExpired(token) })
}

func (r *demandRoute) cancelIdleLocked() {
	if r.idleTimer != nil {
		r.idleTimer.Stop()
		r.idleTimer = nil
	}
	r.idleToken++
}

func (r *demandRoute) idleExpired(token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if token != r.idleToken || r.connections > 0 || r.state != protocol.DemandRunning || r.pending != nil {
		return
	}
	r.idleTimer = nil
	r.beginStopLocked()
}

// checkSession notices a running session that ended on its own: a crash, or
// someone running `mesh kill`. Its connections are already gone; the next
// one starts it again.
func (r *demandRoute) checkSession() {
	r.mu.Lock()
	id := r.sessionID
	running := r.state == protocol.DemandRunning && r.pending == nil
	r.mu.Unlock()
	if !running || id == "" {
		return
	}
	code, ended := r.manager.sessions.sessionExit(id)
	if !ended {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != protocol.DemandRunning || r.pending != nil || r.sessionID != id {
		return
	}
	r.cancelIdleLocked()
	r.state = protocol.DemandStopped
	if code == nil || *code != 0 {
		r.state = protocol.DemandFailed
		r.failure = fmt.Sprintf("%s while serving (session %s)", exitDescription(code), id)
	}
}

func (r *demandRoute) markUnbound(port uint16, err error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	message := err.Error()
	changed := r.unbound[port] != message
	r.unbound[port] = message
	return changed
}

func (r *demandRoute) isUnbound(port uint16) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, unbound := r.unbound[port]
	return unbound
}

func (r *demandRoute) markBound(port uint16) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.unbound, port)
}

func (r *demandRoute) status() *protocol.ServiceDemand {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := &protocol.ServiceDemand{State: r.state, Connections: r.connections, SessionID: r.sessionID}
	if r.service.Demand == nil {
		status.State = ""
		status.SessionID = ""
	}
	if r.state == protocol.DemandFailed {
		status.Failure = boundedServiceProblem(r.failure)
	}
	ports := make([]uint16, 0, len(r.unbound))
	for port := range r.unbound {
		ports = append(ports, port)
	}
	slices.Sort(ports)
	for _, port := range ports {
		status.Unbound = append(status.Unbound, boundedServiceProblem(r.unbound[port]))
	}
	return status
}

// demandListener serves one loopback port for one route.
type demandListener struct {
	manager  *demandManager
	owner    string
	route    string
	port     uint16
	listener net.Listener
	server   *http.Server
	handlers sync.Map // upstream port and isolation → proxy handler
}

func (l *demandListener) close() {
	_ = l.server.Close()
}

func (l *demandListener) serveHTTP(w http.ResponseWriter, request *http.Request) {
	route := l.manager.route(l.owner)
	if route == nil {
		http.Error(w, "route removed", http.StatusNotFound)
		return
	}
	if err := route.ready(request.Context()); err != nil {
		meshserve.WriteDemandFailure(w, err)
		return
	}
	service := route.definition()
	upstream := ""
	for _, listen := range service.Listens {
		if listen.Public == l.port {
			upstream = strconv.Itoa(int(listen.Upstream))
		}
	}
	if upstream == "" {
		http.Error(w, "listener removed", http.StatusNotFound)
		return
	}
	key := upstream
	if service.Isolate {
		key += "+isolate"
	}
	handler, ok := l.handlers.Load(key)
	if !ok {
		built, err := meshserve.Handler(meshserve.Service{
			Name: service.Name, Kind: meshserve.Proxy, Target: upstream, Isolate: service.Isolate,
		}, "/")
		if err != nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		handler, _ = l.handlers.LoadOrStore(key, built)
	}
	handler.(http.Handler).ServeHTTP(w, request)
}

// countingListener counts each accepted connection against its route until
// the connection closes. Keep-alives and upgraded WebSockets alike hold the
// route open; the server's idle timeout ends keep-alives nobody uses.
type countingListener struct {
	net.Listener
	hold func() func()
}

func (l *countingListener) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &countedConn{Conn: connection, release: l.hold()}, nil
}

type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
