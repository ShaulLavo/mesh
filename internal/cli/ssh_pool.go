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

type sshPoolKey struct {
	Identity   string
	Endpoint   string
	Port       uint16
	Credential string
}

type sshPoolEntry struct {
	ready  chan struct{}
	client *ssh.Client
	err    error
	retry  time.Time
	users  int
	idle   time.Time
	dead   bool
}

type sshCommandPool struct {
	mu         sync.Mutex
	ctx        context.Context
	connecting sync.WaitGroup
	entries    map[sshPoolKey]*sshPoolEntry
	dial       func(context.Context, HostRecord, ed25519.PrivateKey, uint16) (*ssh.Client, error)
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

func (p *sshCommandPool) acquire(ctx context.Context, host HostRecord, state string, port uint16) (*ssh.Client, func(), error) {
	key, private, err := sshCommandKey(host, state, port)
	if err != nil {
		return nil, nil, err
	}
	entry, fresh, err := p.reserve(key)
	if err != nil {
		return nil, nil, err
	}
	release := func() { p.release(entry) }
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
	return entry.client, release, nil
}

func (p *sshCommandPool) reserve(key sshPoolKey) (*sshPoolEntry, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneFailed(time.Now())
	if entry := p.entries[key]; entry != nil && !entry.dead {
		if entry.users >= sshPoolChannels {
			return nil, false, errors.New("SSH host has 16 active commands; try again after one finishes")
		}
		entry.users++
		return entry, false, nil
	}
	if len(p.entries) >= sshPoolHosts {
		return nil, false, errors.New("SSH connection owner has 8 destinations in use; wait for an idle connection to expire")
	}
	entry := &sshPoolEntry{ready: make(chan struct{}), users: 1}
	p.entries[key] = entry
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
	}
	close(entry.ready)
	p.mu.Unlock()
	if err != nil {
		return
	}
	go func() { _ = entry.client.Wait(); p.discard(key, entry) }()
	go keepSSHTransportAlive(entry.client)
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
	if p.entries[key] == entry {
		delete(p.entries, key)
	}
}

func (p *sshCommandPool) release(entry *sshPoolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry.users--
	entry.idle = time.Now()
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
