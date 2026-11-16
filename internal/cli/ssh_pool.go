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

type sshPoolEntry struct {
	ready     chan struct{}
	client    *ssh.Client
	err       error
	retry     time.Time
	users     int
	abandoned int
	idle      time.Time
	dead      bool
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
	key, private, err := sshCommandKey(host, state, port)
	if err != nil {
		return nil, nil, err
	}
	entry, fresh, err := p.reserve(key)
	if err != nil {
		return nil, nil, err
	}
	release := func() { p.release(key, entry) }
	if fresh {
		p.connecting.Go(func() { p.connect(key, entry, host, private, port) }) //nolint:contextcheck // The dial belongs to the pool, independently of its first waiter.
	}
	select {
	case <-ctx.Done():
		release()
		return nil, nil, fmt.Errorf("wait for shared SSH transport: %w", ctx.Err())
	case <-entry.ready:
	}
	if entry.err != nil {
		release()
		return nil, nil, entry.err
	}
	return &sshCommandLease{Client: entry.client, pool: p, entry: entry}, release, nil
}

func (p *sshCommandPool) reserve(key sshPoolKey) (*sshPoolEntry, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneFailed(time.Now())
	if entry := p.entries[key]; entry != nil && !entry.dead {
		if entry.users+entry.abandoned >= sshPoolChannels {
			return nil, false, errors.New("SSH host has 16 active commands; try again after one finishes")
		}
		entry.users++
		return entry, false, nil
	}
	if len(p.entries) >= sshPoolHosts || p.live >= sshPoolHosts {
		return nil, false, errors.New("SSH connection owner has 8 destinations in use; wait for an idle connection to expire")
	}
	entry := &sshPoolEntry{ready: make(chan struct{}), users: 1}
	p.entries[key] = entry
	p.live++
	return entry, true, nil
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
	p.live--
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
		return errors.New("SSH connection owner has 128 pending setup operations")
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
}
