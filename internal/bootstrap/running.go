package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/tailnet"
	"github.com/shaul/mesh/internal/transport"
)

// probeTCPTimeout keeps a machine that is not serving Mesh cheap to rule out:
// the usual answer is a refused connection, which arrives at once.
const probeTCPTimeout = 700 * time.Millisecond

// probeVerifyTimeout bounds the identity exchange with a host that did answer.
const probeVerifyTimeout = 5 * time.Second

// Discovery supplies addresses only. Adoption without SSH requires an existing
// owner-controlled destination pin and a device grant at that destination.
func probeRunningDaemon(ctx context.Context, normalized normalizedOptions, deps dependencies) (Result, bool) {
	if normalized.expectedIdentity == "" {
		return Result{}, false
	}
	_, key, err := identity.LoadOrCreate(normalized.stateDir)
	if err != nil {
		return Result{}, false
	}
	peer, ok := deps.servingPeer(ctx, normalized.target, normalized.daemonPort)
	if !ok {
		return Result{}, false
	}
	verifyCtx, cancel := context.WithTimeout(ctx, probeVerifyTimeout)
	defer cancel()
	host, endpoint, err := deps.verify(verifyCtx, peer.Addrs, normalized.daemonPort, normalized.webSocketPath, &transport.Authentication{Key: key, ExpectedIdentity: normalized.expectedIdentity})
	if err != nil || validateVerifiedHost(host, peer.Name) != nil {
		return Result{}, false
	}
	if host.ID != normalized.expectedIdentity || host.MeshIdentity != normalized.expectedIdentity {
		return Result{}, false
	}
	return Result{
		AuthenticatedIdentity: normalized.expectedIdentity,
		Wake:                  host.Wake,
		MachineName:           host.MachineName, NameRevision: host.NameRevision,
		ID:                 host.ID,
		MeshIdentity:       host.MeshIdentity,
		TailscaleName:      peer.Name,
		TailscaleAddresses: append([]string(nil), peer.Addrs...),
		Endpoint:           endpoint,
		AlreadyConfigured:  true,
	}, true
}

// findServingPeer finds the tailnet peer the target names and reports whether
// anything answers on its Mesh port.
func findServingPeer(ctx context.Context, wanted target, port uint16) (tailnet.Peer, bool) {
	peer, err := tailnetPeerFor(ctx, wanted)
	if err != nil || !reachesMeshPort(ctx, peer.Addrs, port) {
		return tailnet.Peer{}, false
	}
	return peer, true
}

// reachesMeshPort keeps the probe from costing anything on a bare machine.
// Verification retries until its deadline, so without this a refused port would
// burn the whole budget before every ordinary SSH adoption.
func reachesMeshPort(ctx context.Context, addresses []string, port uint16) bool {
	for _, address := range addresses {
		dialCtx, cancel := context.WithTimeout(ctx, probeTCPTimeout)
		var dialer net.Dialer
		conn, err := dialer.DialContext(dialCtx, "tcp", net.JoinHostPort(address, strconv.Itoa(int(port))))
		cancel()
		if err != nil {
			continue
		}
		_ = conn.Close()
		return true
	}
	return false
}

// tailnetPeerFor finds the machine the target names. Without SSH there is no
// remote to ask, so the peer list this machine can already see is the only
// source of the addresses to verify against.
func tailnetPeerFor(ctx context.Context, wanted target) (tailnet.Peer, error) {
	peers, err := tailnet.Peers(ctx)
	if err != nil {
		return tailnet.Peer{}, err
	}
	names := []string{wanted.host, wanted.alias}
	for _, peer := range peers {
		if !peerAnswersTo(peer, names) {
			continue
		}
		if len(peer.Addrs) == 0 {
			return tailnet.Peer{}, diagnostic(DiagnosticPortBlocked, fmt.Errorf("tailnet peer %s has no address", peer.Name))
		}
		return peer, nil
	}
	return tailnet.Peer{}, diagnostic(DiagnosticPortBlocked, errors.New("no tailnet peer answers to that name"))
}

func peerAnswersTo(peer tailnet.Peer, names []string) bool {
	full := strings.TrimSuffix(peer.Name, ".")
	short, _, _ := strings.Cut(full, ".")
	for _, name := range names {
		name = strings.TrimSuffix(strings.TrimSpace(name), ".")
		if name == "" {
			continue
		}
		if strings.EqualFold(name, full) || strings.EqualFold(name, short) {
			return true
		}
	}
	return false
}
