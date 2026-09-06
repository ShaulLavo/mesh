package daemon

import (
	"context"
	"errors"

	"github.com/shaul/mesh/internal/protocol"
)

func (s *clientServer) recoverTunnel(ctx context.Context, request protocol.Control) (protocol.Control, error) {
	if local, _ := ctx.Value(localClientKey{}).(bool); !local {
		return protocol.Control{}, errors.New("tunnel: local recovery requires the edge's Unix daemon socket")
	}
	controller, ok := s.edge.(interface {
		RecoverTunnelClaim(context.Context, string) error
	})
	if !ok {
		return protocol.Control{}, errors.New("tunnel: this daemon is not a public edge")
	}
	if err := controller.RecoverTunnelClaim(ctx, request.TunnelName); err != nil {
		return protocol.Control{}, err
	}
	return protocol.Control{Type: protocol.TypeTunnelRecovered, RequestID: request.RequestID, TunnelName: request.TunnelName}, nil
}
