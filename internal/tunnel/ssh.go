package tunnel

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"sync"
	"time"

	charmssh "github.com/charmbracelet/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Endpoint opens HTTP connections to the machine holding this SSH connection.
type Endpoint interface {
	Dial(context.Context) (net.Conn, error)
	Close() error
}

type Activator interface {
	ActivateTunnel(context.Context, string, string, Endpoint) (func(), error)
}

type forwardRequest struct {
	BindAddr string
	BindPort uint32
}

type forwardedChannel struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

type sshForwardKey struct{}

type forwardHandler struct {
	activator Activator
	interval  time.Duration
	mu        sync.Mutex
	perKey    map[string]int
	streams   chan struct{}
}

type forwardConnection struct {
	conn     *gossh.ServerConn
	mu       sync.Mutex
	closed   bool
	forwards map[string]func()
	stopped  chan struct{}
}

// SSHOption installs constrained reverse forwarding. It does not add a listener
// or permit direct-tcpip channels.
func SSHOption(activator Activator) charmssh.Option {
	return sshOption(activator, 15*time.Second)
}

func sshOption(activator Activator, interval time.Duration) charmssh.Option {
	h := &forwardHandler{activator: activator, interval: interval, perKey: make(map[string]int), streams: make(chan struct{}, MaximumForwardsPerKey*maximumStreamsPerForward)}
	return func(server *charmssh.Server) error {
		if activator == nil || interval <= 0 {
			return errors.New("tunnel: invalid SSH forwarding configuration")
		}
		if server.RequestHandlers == nil {
			server.RequestHandlers = make(map[string]charmssh.RequestHandler)
		}
		server.RequestHandlers["tcpip-forward"] = h.handle
		server.RequestHandlers["cancel-tcpip-forward"] = h.handle
		return nil
	}
}

func (h *forwardHandler) handle(ctx charmssh.Context, _ *charmssh.Server, request *gossh.Request) (bool, []byte) {
	var payload forwardRequest
	if len(request.Payload) > MaximumFrameBytes || gossh.Unmarshal(request.Payload, &payload) != nil {
		return false, nil
	}
	if payload.BindPort != 80 || ValidateHostname(payload.BindAddr) != nil {
		return false, nil
	}
	connection, ok := ctx.Value(charmssh.ContextKeyConn).(*gossh.ServerConn)
	if !ok {
		return false, nil
	}
	state, _ := ctx.Value(sshForwardKey{}).(*forwardConnection)
	if state == nil {
		state = &forwardConnection{conn: connection, forwards: make(map[string]func()), stopped: make(chan struct{})}
		ctx.SetValue(sshForwardKey{}, state)
		go state.watch(ctx, h.interval)
	}
	if request.Type == "cancel-tcpip-forward" {
		return state.cancel(payload.BindAddr), nil
	}
	return h.activate(ctx, state, payload.BindAddr), nil
}

func (h *forwardHandler) activate(ctx charmssh.Context, state *forwardConnection, hostname string) bool {
	key, ok := ctx.Value(charmssh.ContextKeyPublicKey).(gossh.CryptoPublicKey)
	if !ok {
		return false
	}
	publicKey, ok := key.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return false
	}
	claimant := KeyID(publicKey)
	if !h.reserve(claimant) {
		return false
	}
	endpoint := newSSHEndpoint(state, hostname, h.streams)
	release, err := h.activator.ActivateTunnel(ctx, claimant, hostname, endpoint)
	if err != nil {
		h.unreserve(claimant)
		_ = endpoint.Close()
		return false
	}
	cleanup := sync.OnceFunc(func() {
		release()
		_ = endpoint.Close()
		h.unreserve(claimant)
	})
	if !state.add(hostname, cleanup) {
		cleanup()
		return false
	}
	return true
}

func (h *forwardHandler) reserve(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.perKey[key] >= MaximumForwardsPerKey {
		return false
	}
	h.perKey[key]++
	return true
}

func (h *forwardHandler) unreserve(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.perKey[key]--
	if h.perKey[key] == 0 {
		delete(h.perKey, key)
	}
}

func (c *forwardConnection) add(hostname string, release func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.forwards[hostname] != nil {
		return false
	}
	c.forwards[hostname] = release
	return true
}

func (c *forwardConnection) cancel(hostname string) bool {
	c.mu.Lock()
	release := c.forwards[hostname]
	delete(c.forwards, hostname)
	c.mu.Unlock()
	if release == nil {
		return false
	}
	release()
	return true
}

func (c *forwardConnection) stop() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.stopped
		return
	}
	c.closed = true
	defer close(c.stopped)
	releases := c.forwards
	c.forwards = nil
	c.mu.Unlock()
	var pending sync.WaitGroup
	for _, release := range releases {
		pending.Go(release)
	}
	pending.Wait()
	_ = c.conn.Close()
}

func (c *forwardConnection) watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer c.stop()
	state := keepaliveState{replies: make(chan error, 1)}
	for c.keepaliveEvent(ctx, ticker.C, &state) {
	}
}

type keepaliveState struct {
	replies chan error
	misses  int
	pending bool
}

func (c *forwardConnection) keepaliveEvent(ctx context.Context, ticks <-chan time.Time, state *keepaliveState) bool {
	select {
	case <-ctx.Done():
		return false
	case err := <-state.replies:
		state.misses, state.pending = 0, false
		return err == nil
	case <-ticks:
		return c.keepaliveTick(state)
	}
}

func (c *forwardConnection) keepaliveTick(state *keepaliveState) bool {
	state.misses++
	// Start teardown at 45 seconds, leaving time for the edge mutation gate.
	if state.misses >= 3 {
		return false
	}
	if state.pending {
		return true
	}
	state.pending = true
	go c.keepalive(state.replies)
	return true
}

func (c *forwardConnection) keepalive(replies chan<- error) {
	_, _, err := c.conn.SendRequest("keepalive@openssh.com", true, nil)
	replies <- err
}
