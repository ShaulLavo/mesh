package fleetshare

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// Sync replaces complete snapshots from each enrolled controller. Failed pulls
// keep the last durable snapshot available through restarts and outages.
func Sync(ctx context.Context, configDir, stateDir string) error {
	peers, err := Peers(configDir)
	if err != nil || len(peers) == 0 {
		return err
	}
	dir, err := openDirectory(configDir, false)
	if err != nil || dir == nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	lock, err := openFile(dir, ".connection-sharing-sync.lock", unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := acquireLockFor(ctx, lock, 10*time.Second); err != nil {
		return fmt.Errorf("fleet: wait for connection sync: %w", err)
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
	// The lock covers observation and publication. A slower earlier pull cannot
	// overwrite a newer snapshot written by another daemon or CLI process.
	return syncLocked(ctx, configDir, stateDir)
}

func syncLocked(ctx context.Context, configDir, stateDir string) error {
	state, err := readState(configDir)
	if err != nil || len(state.Peers) == 0 {
		return err
	}
	_, key, err := identity.LoadPrivate(stateDir)
	if err != nil {
		return fmt.Errorf("fleet: load controller identity: %w", err)
	}
	return syncSources(ctx, configDir, key, state.Peers)
}

func syncSources(ctx context.Context, configDir string, key ed25519.PrivateKey, sources []enrollment) error {
	var workers sync.WaitGroup
	slots := make(chan struct{}, 8)
	results := make(chan error, len(sources))
	for _, source := range sources {
		select {
		case <-ctx.Done():
			results <- ctx.Err()
		case slots <- struct{}{}:
			startSource(ctx, &workers, slots, results, configDir, key, source)
		}
	}
	workers.Wait()
	close(results)
	var failures []error
	for result := range results {
		failures = append(failures, result)
	}
	return errors.Join(failures...)
}

func startSource(ctx context.Context, workers *sync.WaitGroup, slots chan struct{}, results chan<- error, configDir string, key ed25519.PrivateKey, source enrollment) {
	workers.Go(func() {
		defer func() { <-slots }()
		results <- syncSource(ctx, configDir, key, source)
	})
}

func syncSource(ctx context.Context, configDir string, key ed25519.PrivateKey, source enrollment) error {
	pull, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	hosts, err := fetch(pull, key, source.Peer)
	if err != nil {
		return fmt.Errorf("fleet: pull controller %s: %w", source.Identity, err)
	}
	return replaceSnapshot(ctx, configDir, source, hosts)
}

func fetch(ctx context.Context, key ed25519.PrivateKey, peer Peer) ([]protocol.FleetHost, error) {
	conn, err := transport.DialOnce(ctx, peer.Endpoint, transport.DialOptions{Auth: &transport.Authentication{Key: key, ExpectedIdentity: peer.Identity}})
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() { stop(); _ = conn.Close() }()
	request, err := fleetRequest()
	if err != nil {
		return nil, err
	}
	if err := conn.WriteFrame(request); err != nil {
		return nil, fmt.Errorf("request connections: %w", err)
	}
	frame, err := conn.ReadFrame()
	if err != nil {
		return nil, fmt.Errorf("read connections: %w", err)
	}
	return fleetResponse(frame, request)
}

func fleetRequest() (protocol.Frame, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return protocol.Frame{}, fmt.Errorf("fleet: generate request ID: %w", err)
	}
	request := protocol.Control{Type: protocol.TypeFleetHosts, RequestID: hex.EncodeToString(nonce[:])}
	payload, err := request.Encode()
	if err != nil {
		return protocol.Frame{}, fmt.Errorf("fleet: encode request: %w", err)
	}
	return protocol.Frame{Kind: protocol.KindControl, Payload: payload}, nil
}

func fleetResponse(frame, request protocol.Frame) ([]protocol.FleetHost, error) {
	if frame.Kind != protocol.KindControl {
		return nil, errors.New("fleet: response is not a control frame")
	}
	response, err := protocol.DecodeControl(frame.Payload)
	if err != nil {
		return nil, fmt.Errorf("fleet: decode response: %w", err)
	}
	original, _ := protocol.DecodeControl(request.Payload)
	if response.RequestID != original.RequestID {
		return nil, errors.New("fleet: response request ID does not match")
	}
	if response.Type != protocol.TypeFleetHostsResult {
		return nil, errors.New("fleet: controller did not return its connections")
	}
	if err := validateHosts(response.FleetHosts); err != nil {
		return nil, err
	}
	return response.FleetHosts, nil
}
