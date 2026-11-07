package daemon

import (
	"fmt"
	"sync/atomic"

	"github.com/shaul/mesh/internal/access"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/shaul/mesh/internal/update"
)

func publishedOtherRoles(stateDir string, subject access.Subject) (access.OtherRoles, error) {
	full, err := identity.DeviceMembership(stateDir, string(subject))
	if err != nil {
		return access.OtherRoles{}, fmt.Errorf("daemon: observe full device role: %w", err)
	}
	administrator, err := update.AdministratorMembership(stateDir, string(subject))
	if err != nil {
		return access.OtherRoles{}, fmt.Errorf("daemon: observe update role: %w", err)
	}
	return access.OtherRoles{FullDevice: full, UpdateAdministrator: administrator}, nil
}
func controlAuthentication(stateDir string) (*transport.Authentication, error) {
	host, key, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return nil, fmt.Errorf("daemon: load control identity: %w", err)
	}
	scopes, err := access.Open(stateDir, host.ID, nil)
	if err != nil {
		return nil, fmt.Errorf("daemon: open scoped policy: %w", err)
	}
	updates := &update.Authority{StateDir: stateDir, ID: host.ID}
	bind := func(id string) transport.Authorization { return bindControlPeer(stateDir, id, scopes, updates) }
	return &transport.Authentication{Key: key, Authorize: func(id string) bool { grant := bind(id); return grant.Current != nil && grant.Current() }, Bind: bind}, nil
}
func bindControlPeer(stateDir, id string, scopes *access.Store, updates *update.Authority) transport.Authorization {
	scoped, err := scopes.Membership(access.Subject(id))
	if err != nil {
		return transport.Authorization{}
	}
	if scoped {
		return bindScopedPeer(stateDir, id, scopes)
	}
	unambiguous := func() bool {
		registered, err := scopes.Membership(access.Subject(id))
		return err == nil && !registered
	}
	if current, ok := identity.BindIdentity(stateDir, id); ok {
		return transport.Authorization{Class: access.FullDevice, Current: latchCurrent(func() bool { return current() && unambiguous() })}
	}
	return transport.Authorization{Class: access.UpdateCarrier, Current: latchCurrent(func() bool { return updates.IsAdministrator(id) && unambiguous() }), Admit: func(frame protocol.Frame) bool {
		if frame.Kind != protocol.KindControl {
			return false
		}
		request, err := protocol.DecodeControl(frame.Payload)
		return err == nil && request.Type == update.ControlType
	}}
}
func bindScopedPeer(stateDir, id string, scopes *access.Store) transport.Authorization {
	binding, err := scopes.Bind(access.Subject(id))
	if err != nil {
		return transport.Authorization{}
	}
	binding = binding.Constrain(func() bool {
		roles, err := publishedOtherRoles(stateDir, access.Subject(id))
		return err == nil && !roles.FullDevice && !roles.UpdateAdministrator
	})

	return transport.Authorization{Class: access.Scoped, Scoped: binding, Current: binding.Current, Admit: func(frame protocol.Frame) bool {
		if frame.Kind != protocol.KindControl {
			return false
		}
		_, err := protocol.DecodeAccessRead(frame.Payload)
		return err == nil
	}}
}
func latchCurrent(current func() bool) func() bool {
	var invalid atomic.Bool
	return func() bool {
		if invalid.Load() {
			return false
		}
		if !current() {
			invalid.Store(true)
			return false
		}
		return !invalid.Load()
	}
}
