package daemon

import (
	"fmt"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

func controlAuthentication(stateDir string) (*transport.Authentication, error) {
	host, key, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return nil, fmt.Errorf("daemon: load control identity: %w", err)
	}
	updates := &update.Authority{StateDir: stateDir, ID: host.ID}
	granted := func(id string) bool { return identity.GrantedIdentity(stateDir, id) }
	return &transport.Authentication{
		Key:       key,
		Authorize: func(id string) bool { return granted(id) || updates.IsAdministrator(id) },
		Retain: func(id string) func() bool {
			if granted(id) {
				return func() bool { return granted(id) }
			}
			return func() bool { return updates.IsAdministrator(id) }
		},
		Admit: func(id string, frame protocol.Frame) bool {
			if granted(id) {
				return true
			}
			if frame.Kind != protocol.KindControl || !updates.IsAdministrator(id) {
				return false
			}
			control, err := protocol.DecodeControl(frame.Payload)
			return err == nil && control.Type == update.ControlType
		},
	}, nil
}
