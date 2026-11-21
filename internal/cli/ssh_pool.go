package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"golang.org/x/crypto/ssh"
)

const sshPoolIdle = time.Minute
const sshPoolRetry = time.Second
const sshPoolHosts = 8
const sshPoolChannels = 16
const sshPoolSetups = sshPoolHosts * sshPoolChannels

type sshPoolKey struct {
	Identity   string
	Endpoint   string
	Port       uint16
	Credential string
}

// errSSHPoolFull means the shared owner has no room for a command. Nothing was
// sent to the destination, so the caller runs it on its own connection instead.
var errSSHPoolFull = errors.New("shared SSH capacity is in use")

type sshPoolEntry struct {
	ready     chan struct{}
	client    *ssh.Client
	err       error
	retry     time.Time
	users     int
	abandoned int
	idle      time.Time
	dead      bool
	// evicted entries stopped counting as live when they were closed, so their
	// later disconnect must not count them out a second time.
	evicted bool
}

type sshCommandPool struct {
	mu            sync.Mutex
	ctx           context.Context
	connecting    sync.WaitGroup
	setups        sync.WaitGroup
	lifetimes     sync.WaitGroup
	live          int
	pendingSetups int
	entries       map[sshPoolKey]*sshPoolEntry
	dial          func(context.Context, HostRecord, ed25519.PrivateKey, uint16) (*ssh.Client, error)
}

type sshCommandLease struct {
	*ssh.Client
	pool  *sshCommandPool
	key   sshPoolKey
	entry *sshPoolEntry
}

// sshPoolTicket is a reserved place for one command whose transport may still
// be connecting.
type sshPoolTicket struct {
	pool  *sshCommandPool
	key   sshPoolKey
	entry *sshPoolEntry
}

func sshCommandKey(host HostRecord, state string, port uint16) (sshPoolKey, ed25519.PrivateKey, error) {
	_, private, err := identity.LoadPrivate(state)
	if err != nil {
		return sshPoolKey{}, nil, fmt.Errorf("load SSH caller identity: %w", err)
	}
	fingerprint := sha256.Sum256(private)
	return sshPoolKey{Identity: host.MeshIdentity, Endpoint: host.Endpoint, Port: port,
		Credential: hex.EncodeToString(fingerprint[:])}, private, nil
}

func (p *sshCommandPool) acquire(ctx context.Context, host HostRecord, state string, port uint16) (*sshCommandLease, func(), error) {
	ticket, err := p.admit(host, state, port) //nolint:contextcheck // The dial belongs to the pool, independently of this caller.
	if err != nil {
		return nil, nil, err
	}
	return ticket.wait(ctx)
}

// admit reserves room for one command without waiting for its transport.
func (p *sshCommandPool) admit(host HostRecord, state string, port uint16) (*sshPoolTicket, error) {
	key, private, err := sshCommandKey(host, state, port)
	if err != nil {
		return nil, err
	}
	entry, fresh, err := p.reserve(key)
	if err != nil {
		return nil, err
	}
	if fresh {
		p.connecting.Go(func() { p.connect(key, entry, host, private, port) }) //nolint:contextcheck // The dial belongs to the pool, independently of its first waiter.
	}
	return &sshPoolTicket{pool: p, key: key, entry: entry}, nil
}

func (t *sshPoolTicket) wait(ctx context.Context) (*sshCommandLease, func(), error) {
	release := func() { t.pool.release(t.key, t.entry) }
	select {
	case <-ctx.Done():
		release()
		return nil, nil, fmt.Errorf("wait for shared SSH transport: %w", ctx.Err())
	case <-t.entry.ready:
	}
	if t.entry.err != nil {
		release()
		return nil, nil, t.entry.err
	}
	return &sshCommandLease{Client: t.entry.client, pool: t.pool, key: t.key, entry: t.entry}, release, nil
}

func (p *sshCommandPool) reserve(key sshPoolKey) (*sshPoolEntry, bool, error) {
	p.mu.Lock()
	entry, fresh, evicted, err := p.reserveLocked(key)
	p.mu.Unlock()
	if evicted != nil {
		_ = evicted.Close()
	}
	return entry, fresh, err
}

// Called under mu. Returns an evicted idle transport for the caller to close.
func (p *sshCommandPool) reserveLocked(key sshPoolKey) (*sshPoolEntry, bool, *ssh.Client, error) {
	p.pruneFailed(time.Now())
	if entry := p.entries[key]; entry != nil && !entry.dead {
		if entry.users+entry.abandoned >= sshPoolChannels {
			return nil, false, nil, fmt.Errorf("%w: SSH host has %d active commands", errSSHPoolFull, sshPoolChannels)
		}
		entry.users++
		return entry, false, nil, nil
	}
	var evicted *ssh.Client
	if len(p.entries) >= sshPoolHosts || p.live >= sshPoolHosts {
		evicted = p.evictIdleLocked()
	}
	if len(p.entries) >= sshPoolHosts || p.live >= sshPoolHosts {
		return nil, false, evicted, fmt.Errorf("%w: SSH connection owner has %d busy destinations", errSSHPoolFull, sshPoolHosts)
	}
	entry := &sshPoolEntry{ready: make(chan struct{}), users: 1}
	p.entries[key] = entry
	p.live++
	return entry, true, evicted, nil
}

// Called under mu. Retires the least recently used transport that no command
// is using; nothing runs on it, so closing it cannot affect a command.
func (p *sshCommandPool) evictIdleLocked() *ssh.Client {
	var oldestKey sshPoolKey
	var oldest *sshPoolEntry
	for key, entry := range p.entries {
		if entry.users != 0 || entry.abandoned != 0 || entry.client == nil || entry.dead {
			continue
		}
		if oldest == nil || entry.idle.Before(oldest.idle) {
			oldestKey, oldest = key, entry
		}
	}
	if oldest == nil {
		return nil
	}
	oldest.dead, oldest.evicted = true, true
	p.live--
	delete(p.entries, oldestKey)
	return oldest.client
}

// forget retires a transport that failed to open a channel, so no command
// waits on it again. Closing it runs discard, which settles the live count.
func (p *sshCommandPool) forget(key sshPoolKey, entry *sshPoolEntry) {
	p.mu.Lock()
	entry.dead = true
	if p.entries[key] == entry {
		delete(p.entries, key)
	}
	p.mu.Unlock()
	_ = entry.client.Close()
}

// Called under mu. A failed dial has no command whose execution could be replayed.
func (p *sshCommandPool) pruneFailed(now time.Time) {
	for key, entry := range p.entries {
		if entry.err != nil && entry.users == 0 && !now.Before(entry.retry) {
			delete(p.entries, key)
		}
	}
}

func (p *sshCommandPool) connect(key sshPoolKey, entry *sshPoolEntry, host HostRecord, private ed25519.PrivateKey, port uint16) {
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()
	client, err := p.dial(ctx, host, private, port)
	p.mu.Lock()
	entry.client, entry.err = client, err
	if err != nil {
		entry.retry = time.Now().Add(sshPoolRetry)
		p.live--
	}
	close(entry.ready)
	p.mu.Unlock()
	if err != nil {
		return
	}
	p.lifetimes.Go(func() { _ = entry.client.Wait(); p.discard(key, entry) })
	p.lifetimes.Go(func() { keepSSHTransportAlive(entry.client) })
}

func keepSSHTransportAlive(client *ssh.Client) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	done := make(chan struct{})
	go func() { _ = client.Wait(); close(done) }()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !probeSSHTransport(client) {
				return
			}
		}
	}
}

func probeSSHTransport(client *ssh.Client) bool {
	reply := make(chan error, 1)
	go func() { _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); reply <- err }()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case err := <-reply:
		return err == nil
	case <-timer.C:
		_ = client.Close()
		<-reply
		return false
	}
}

func (p *sshCommandPool) discard(key sshPoolKey, entry *sshPoolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry.dead = true
	if !entry.evicted {
		p.live--
	}
	if p.entries[key] == entry {
		delete(p.entries, key)
	}
}

func (p *sshCommandPool) release(key sshPoolKey, entry *sshPoolEntry) {
	p.mu.Lock()
	entry.users--
	entry.idle = time.Now()
	retire := entry.users == 0 && entry.abandoned != 0
	if retire {
		entry.dead = true
		if p.entries[key] == entry {
			delete(p.entries, key)
		}
	}
	p.mu.Unlock()
	if retire {
		_ = entry.client.Close()
	}
}

type sshSetupResult struct {
	session *ssh.Session
	err     error
}

// SSH cannot cancel an unconfirmed channel open. Account for its late reply
// until it arrives, or close the idle transport after the last sibling leaves.
func (l *sshCommandLease) setup(ctx context.Context, operation func() (*ssh.Session, error)) (*ssh.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("SSH setup canceled: %w", err)
	}
	if err := l.pool.beginSetup(); err != nil {
		return nil, err
	}
	result := make(chan sshSetupResult, 1)
	l.pool.setups.Go(func() { session, err := operation(); result <- sshSetupResult{session: session, err: err} })
	select {
	case reply := <-result:
		l.pool.endSetup()
		return reply.session, reply.err
	case <-ctx.Done():
		l.pool.mu.Lock()
		l.entry.abandoned++
		l.pool.mu.Unlock()
		l.pool.setups.Go(func() { l.finishSetup(<-result) })
		return nil, fmt.Errorf("SSH setup canceled: %w", ctx.Err())
	}
}

func (l *sshCommandLease) finishSetup(reply sshSetupResult) {
	if reply.session != nil {
		_ = reply.session.Close()
	}
	l.pool.mu.Lock()
	l.entry.abandoned--
	l.pool.pendingSetups--
	l.pool.mu.Unlock()
}

func (p *sshCommandPool) beginSetup() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pendingSetups >= sshPoolSetups {
		return fmt.Errorf("%w: SSH connection owner has %d pending setup operations", errSSHPoolFull, sshPoolSetups)
	}
	p.pendingSetups++
	return nil
}

func (p *sshCommandPool) endSetup() {
	p.mu.Lock()
	p.pendingSetups--
	p.mu.Unlock()
}

func (p *sshCommandPool) expire(now time.Time, all bool) {
	p.mu.Lock()
	closing := make([]*ssh.Client, 0, len(p.entries))
	for key, entry := range p.entries {
		if !all && (entry.users != 0 || now.Sub(entry.idle) < sshPoolIdle) {
			continue
		}
		entry.dead = true
		delete(p.entries, key)
		if entry.client != nil {
			closing = append(closing, entry.client)
		}
	}
	p.mu.Unlock()
	for _, client := range closing {
		_ = client.Close()
	}
}

type sshHelperRequest struct {
	Host HostRecord      `json:"host"`
	Port uint16          `json:"port"`
	Run  json.RawMessage `json:"run"`
}

type sshHelperResult struct {
	Code  int    `json:"code"`
	Error string `json:"error,omitempty"`
	// fallback asks the client to run the command on its own connection.
	// It is decided before acceptance and never sent as a result.
	fallback bool
}
