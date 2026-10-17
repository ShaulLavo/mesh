package fleetshare

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

// Service publishes only the local connection book supplied by LocalHosts.
// The caller dispatches it behind the normal full-control transport gate.
type Service struct {
	Directory   string
	StateDir    string
	LocalHosts  func(context.Context) ([]protocol.FleetHost, error)
	ReportError func(error)
}

// HandleControl serves authenticated, currently approved controllers.
func (s *Service) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if request.Type != protocol.TypeFleetHosts {
		return protocol.Control{}, false, nil
	}
	peer, ok := transport.Peer(ctx)
	if !ok || !identity.GrantedIdentity(s.StateDir, peer.Identity) {
		return protocol.Control{}, true, errors.New("fleet: approved controller authentication required")
	}
	if err := s.authorize(peer.Identity); err != nil {
		return protocol.Control{}, true, err
	}
	if s.LocalHosts == nil {
		return protocol.Control{}, true, errors.New("fleet: local connection book is unavailable")
	}
	hosts, err := s.LocalHosts(ctx)
	if err != nil {
		return protocol.Control{}, true, fmt.Errorf("fleet: read local connections: %w", err)
	}
	if err := validateHosts(hosts); err != nil {
		return protocol.Control{}, true, err
	}
	if err := s.authorize(peer.Identity); err != nil {
		return protocol.Control{}, true, err
	}
	if !identity.GrantedIdentity(s.StateDir, peer.Identity) {
		return protocol.Control{}, true, errors.New("fleet: controller device approval was removed")
	}
	return protocol.Control{Type: protocol.TypeFleetHostsResult, RequestID: request.RequestID, FleetHosts: hosts}, true, nil
}

func (s *Service) authorize(id string) error {
	peers, err := Peers(s.Directory)
	if err != nil {
		return err
	}
	for _, peer := range peers {
		if peer.Identity == id {
			return nil
		}
	}
	return errors.New("fleet: device is not a trusted fleet controller")
}

// Run retries snapshots after startup and while controllers are offline.
func (s *Service) Run(ctx context.Context) {
	s.run(ctx, 30*time.Second)
}

func (s *Service) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		err := Sync(ctx, s.Directory, s.StateDir)
		if err != nil && ctx.Err() == nil && s.ReportError != nil {
			s.ReportError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
