// Package wakeclient finds a LAN sender and waits for a pinned Mesh target.
package wakeclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/wake"
)

const (
	Timeout        = 90 * time.Second
	requestTimeout = 5 * time.Second
	maximumPeers   = 256
)

var ErrObservationUnavailable = errors.New("no independent witness confirms that the target is down")

type Target struct{ ID, Name, Endpoint string }
type Result struct {
	Sender        string
	AlreadyOnline bool
}
type Observation struct {
	State  wake.State
	Sender string
}
type Options struct {
	Endpoints     func(context.Context) ([]string, error)
	DiscoverPeers func(context.Context) ([]tailnet.Peer, error)
}

type localSender interface {
	Probe(context.Context, wake.Grant) (wake.State, error)
	Send(context.Context, wake.Grant) (bool, error)
}

type flight struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	result  Result
	err     error
}

type candidate struct {
	endpoint, identity, name string
	state                    wake.State
}

type Client struct {
	selfID    string
	cache     *wake.Cache
	sender    localSender
	endpoints func(context.Context) ([]string, error)
	peers     func(context.Context) ([]tailnet.Peer, error)
	exchange  func(context.Context, string, string, protocol.Control) (protocol.HostInfo, protocol.Control, error)
	mu        sync.Mutex
	flights   map[string]*flight
}

func New(stateDir string, opts Options) (*Client, error) {
	cache, err := wake.NewCache(stateDir)
	if err != nil {
		return nil, err
	}
	sender, err := wake.NewSender(stateDir)
	if err != nil {
		return nil, err
	}
	if opts.DiscoverPeers == nil {
		opts.DiscoverPeers = tailnet.Peers
	}
	self, err := identity.Load(stateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &Client{selfID: self.ID, cache: cache, sender: sender, endpoints: opts.Endpoints, peers: opts.DiscoverPeers,
		exchange: exchange, flights: make(map[string]*flight)}, nil
}

func (c *Client) Remember(grant wake.Grant) error { return c.cache.Put(grant) }

// Refresh learns current permission over an identity-pinned connection.
func (c *Client) Refresh(ctx context.Context, target Target) error {
	host, _, err := c.exchange(ctx, target.Endpoint, target.ID, protocol.Control{})
	if err != nil {
		return err
	}
	if host.Wake == nil {
		return nil
	}
	if host.Wake.TargetID != target.ID {
		return fmt.Errorf("%w: wake permission belongs to another host", ErrIdentityChanged)
	}
	if err := wake.ValidateGrant(*host.Wake, time.Now()); err != nil {
		if errors.Is(err, wake.ErrExpired) {
			return nil
		}
		return fmt.Errorf("%w: invalid wake permission: %v", ErrIdentityChanged, err)
	}
	// Optional wake metadata must not make a pinned, usable origin unavailable.
	cacheCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_ = c.cache.PutContext(cacheCtx, *host.Wake)
	return nil
}

// Publish distributes a signed permission change. It never probes the LAN or
// sends a wake packet; disconnected peers learn it on a later exchange.
func (c *Client) Publish(ctx context.Context, grant wake.Grant) error {
	if err := c.cache.PutContext(ctx, grant); err != nil {
		return err
	}
	endpoints, err := c.discover(ctx)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	limit := make(chan struct{}, 16)
	for _, endpoint := range endpoints {
		wg.Go(func() { c.publishPeer(ctx, endpoint, grant, limit) })
	}
	wg.Wait()
	return ctx.Err()
}

func (c *Client) publishPeer(ctx context.Context, endpoint string, grant wake.Grant, limit chan struct{}) {
	select {
	case <-ctx.Done():
		return
	case limit <- struct{}{}:
	}
	defer func() { <-limit }()
	_, _, _ = c.request(ctx, endpoint, "", protocol.Control{Type: protocol.TypeWakeRemember, WakeGrant: &grant})
}

// Wake shares a bounded operation among callers. Cancelling the last waiter
// cancels its network work, without allowing one request to cancel another.
func (c *Client) Wake(ctx context.Context, target Target) (Result, error) {
	if target.ID == "" || target.Endpoint == "" {
		return Result{}, errors.New("wake target has no identity or endpoint")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	c.mu.Lock()
	f := c.flights[target.ID]
	if f == nil {
		if len(c.flights) >= maximumPeers {
			c.mu.Unlock()
			return Result{}, errors.New("too many concurrent wake targets")
		}
		operation, cancel := context.WithTimeout(context.Background(), Timeout)
		f = &flight{done: make(chan struct{}), cancel: cancel}
		c.flights[target.ID] = f
		go c.runFlight(operation, target, f)
	}
	f.waiters++
	c.mu.Unlock()
	defer c.release(target.ID, f)
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-f.done:
		return f.result, f.err
	}
}

func (c *Client) runFlight(ctx context.Context, target Target, f *flight) {
	f.result, f.err = c.wake(ctx, target)
	f.cancel()
	close(f.done)
}

func (c *Client) release(id string, f *flight) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f.waiters--
	if f.waiters != 0 {
		return
	}
	f.cancel()
	if c.flights[id] == f {
		delete(c.flights, id)
	}
}

func (c *Client) wake(ctx context.Context, target Target) (Result, error) {
	err := c.ready(ctx, target)
	if err == nil {
		return Result{AlreadyOnline: true}, nil
	}
	if errors.Is(err, ErrIdentityChanged) {
		return Result{}, err
	}
	grant, err := c.grant(target)
	if err != nil {
		return Result{}, err
	}
	sender, err := c.selectSender(ctx, target, grant, true)
	if err != nil {
		return Result{}, err
	}
	result := Result{Sender: sender.name}
	if sender.endpoint == "" {
		if _, err := c.sender.Send(ctx, grant); err != nil {
			return result, fmt.Errorf("wake %s locally: %w", target.Name, err)
		}
	} else {
		// A lost reply may follow a successful broadcast. Wait for the target
		// after an ambiguous result rather than selecting a second sender.
		_, response, sendErr := c.request(ctx, sender.endpoint, sender.identity, protocol.Control{Type: protocol.TypeWakeSend, WakeGrant: &grant})
		if errors.Is(sendErr, ErrIdentityChanged) {
			return result, sendErr
		}
		if sendErr == nil && response.Type == protocol.TypeError {
			return result, fmt.Errorf("wake through %s: %s", sender.name, response.Message)
		}
		if sendErr == nil && response.Type != protocol.TypeWakeSent {
			return result, errors.New("wake sender returned an unexpected response")
		}
	}
	if err := c.waitReady(ctx, target); err != nil {
		return result, fmt.Errorf("wake %s through %s: %w", target.Name, sender.name, err)
	}
	return result, nil
}

func (c *Client) grant(target Target) (wake.Grant, error) {
	grant, err := c.cache.Get(target.ID)
	if errors.Is(err, os.ErrNotExist) {
		return wake.Grant{}, fmt.Errorf("host %s has no known wake permission; run mesh wake allow on it, then connect once while it is online", target.Name)
	}
	if err != nil {
		return wake.Grant{}, fmt.Errorf("read wake permission for %s: %w", target.Name, err)
	}
	if err := wake.ValidateGrant(grant, time.Now()); err != nil {
		return wake.Grant{}, err
	}
	if !grant.Enabled {
		return wake.Grant{}, fmt.Errorf("host %s does not allow waking: %w", target.Name, wake.ErrDisabled)
	}
	return grant, nil
}

// Observe never transmits a wake packet. Only a remote sender can supply the
// independent network observation used for recovering an existing connection.
func (c *Client) Observe(ctx context.Context, target Target) (Observation, error) {
	grant, err := c.grant(target)
	if err != nil {
		return Observation{State: wake.Unknown}, err
	}
	sender, err := c.selectSender(ctx, target, grant, false)
	if err != nil {
		return Observation{State: wake.Unknown}, err
	}
	return Observation{State: sender.state, Sender: sender.name}, nil
}

func (c *Client) Recover(ctx context.Context, target Target) error {
	observation, err := c.Observe(ctx, target)
	if err != nil {
		return err
	}
	if observation.State != wake.Down {
		return ErrObservationUnavailable
	}
	_, err = c.Wake(ctx, target)
	return err
}

func (c *Client) ready(ctx context.Context, target Target) error {
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return c.Refresh(probe, target)
}

func (c *Client) waitReady(ctx context.Context, target Target) error {
	for {
		err := c.ready(ctx, target)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrIdentityChanged) {
			return err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) selectSender(ctx context.Context, target Target, grant wake.Grant, local bool) (candidate, error) {
	if local {
		probe, cancel := context.WithTimeout(ctx, requestTimeout)
		state, err := c.sender.Probe(probe, grant)
		cancel()
		if err == nil {
			return candidate{name: "this machine", state: state}, nil
		}
	}
	endpoints, err := c.discover(ctx)
	if err != nil {
		return candidate{}, fmt.Errorf("find wake sender: %w", err)
	}
	return c.probePeers(ctx, target, grant, endpoints)
}

func (c *Client) discover(ctx context.Context) ([]string, error) {
	discovery, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var endpoints []string
	var configErr error
	if c.endpoints != nil {
		endpoints, configErr = c.endpoints(discovery)
	}
	peers, peerErr := c.peers(discovery)
	for _, peer := range peers {
		if !peer.Online {
			continue
		}
		for _, address := range peer.Addrs {
			endpoints = append(endpoints, (&url.URL{Scheme: "ws", Host: net.JoinHostPort(address, "7337"), Path: "/mesh"}).String())
		}
	}
	if len(endpoints) == 0 {
		return nil, errors.Join(configErr, peerErr, errors.New("no awake Mesh peers found"))
	}
	sort.Strings(endpoints)
	unique := make([]string, 0, min(maximumPeers, len(endpoints)))
	for _, endpoint := range endpoints {
		if len(unique) == maximumPeers {
			break
		}
		if len(unique) != 0 && unique[len(unique)-1] == endpoint {
			continue
		}
		unique = append(unique, endpoint)
	}
	return unique, nil
}

func (c *Client) probePeers(ctx context.Context, target Target, grant wake.Grant, endpoints []string) (candidate, error) {
	probes, cancel := context.WithTimeout(ctx, 2*requestTimeout)
	defer cancel()
	results := make([]candidate, len(endpoints))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 16)
	for i, endpoint := range endpoints {
		if endpoint == target.Endpoint {
			continue
		}
		wg.Go(func() { c.probePeer(probes, target.ID, endpoint, grant, limit, &results[i]) })
	}
	wg.Wait()
	for _, result := range results {
		if result.identity != "" && result.state == wake.Down {
			return result, nil
		}
	}
	for _, result := range results {
		if result.identity != "" {
			return result, nil
		}
	}
	return candidate{}, fmt.Errorf("no awake Mesh machine can send on %s's LAN", target.Name)
}

func (c *Client) probePeer(ctx context.Context, targetID, endpoint string, grant wake.Grant, limit chan struct{}, result *candidate) {
	select {
	case <-ctx.Done():
		return
	case limit <- struct{}{}:
	}
	defer func() { <-limit }()
	host, response, err := c.request(ctx, endpoint, "", protocol.Control{Type: protocol.TypeWakeProbe, WakeGrant: &grant})
	if err != nil || host.ID == targetID || host.ID == c.selfID || response.Type != protocol.TypeWakeProbed || !response.WakeCanSend {
		return
	}
	name := host.TailscaleName
	if name == "" {
		name = endpoint
	}
	*result = candidate{endpoint: endpoint, identity: host.ID, name: name, state: response.WakeState}
}

func (c *Client) request(ctx context.Context, endpoint, id string, request protocol.Control) (protocol.HostInfo, protocol.Control, error) {
	bounded, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return c.exchange(bounded, endpoint, id, request)
}
