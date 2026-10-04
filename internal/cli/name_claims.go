package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
)

func declaredName(info protocol.HostInfo) machinename.Claim {
	return machinename.Claim{ID: info.ID, MachineName: info.MachineName, Revision: info.NameRevision}
}

func rememberHostName(ctx context.Context, host HostRecord, info protocol.HostInfo) error {
	if err := validateHostInfo(host, info); err != nil {
		return err
	}
	if info.MachineName == "" && info.NameRevision == 0 {
		return nil
	}
	if host.local {
		return nil
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if _, err := machinename.RememberClaim(ctx, filepath.Dir(path), host.ID, declaredName(info)); err != nil {
		return fmt.Errorf("cache destination machine name: %w", err)
	}
	return nil
}

func stateDeclaredHost(response protocol.Control) *protocol.HostInfo {
	switch response.Type {
	case protocol.TypeStateSnapshot:
		if response.StateSnapshot != nil {
			return response.StateSnapshot.Host
		}
	case protocol.TypeStateEvent:
		if response.StateEvent != nil {
			return response.StateEvent.Payload.Host
		}
	}
	return nil
}

func verifyNamedTarget(ctx context.Context, host HostRecord, info protocol.HostInfo) error {
	if host.targetName == "" {
		return nil
	}
	if info.MachineName != host.targetName {
		return fmt.Errorf("machine name %q changed on the destination; use its exact host ID %s", host.targetName, host.ID)
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	owner, err := targetOwnerClaim(ctx, host, info)
	if err != nil {
		return err
	}
	hosts = withOwnerClaim(hosts, owner)
	target, err := ResolveArgument(host.targetName, hosts)
	if err != nil {
		return err
	}
	if target.Host == nil || target.Host.ID != host.ID {
		return fmt.Errorf("machine name %q changed targets; use the exact host ID %s", host.targetName, host.ID)
	}
	return nil
}

func applyVerifiedState(ctx context.Context, host HostRecord, view *StateView, response protocol.Control, received time.Time, transit time.Duration) error {
	if err := validateStateHost(host, response); err != nil {
		return err
	}
	info := stateDeclaredHost(response)
	if info == nil {
		return view.Apply(response, received, transit)
	}
	next := view.Clone()
	if err := next.Apply(response, received, transit); err != nil {
		return err
	}
	if err := rememberHostName(ctx, host, *info); err != nil {
		return err
	}
	*view = next
	return nil
}

func resolveDeclaredArgument(value string, hosts []HostRecord) (ArgumentTarget, bool, error) {
	claims := make([]machinename.Claim, 0, len(hosts))
	for _, host := range hosts {
		if host.MachineName != "" {
			claims = append(claims, machinename.Claim{ID: host.ID, MachineName: host.MachineName, Revision: host.NameRevision})
		}
	}
	var ids []string
	for _, row := range machinename.Project(claims) {
		if strings.EqualFold(row.MachineName, value) {
			ids = append(ids, row.ID)
		}
	}
	if len(ids) > 1 {
		return ArgumentTarget{}, true, fmt.Errorf("machine name %q is shared by hosts %s; use an exact host ID", value, strings.Join(ids, ", "))
	}
	if len(ids) == 1 {
		for _, host := range hosts {
			if host.ID == ids[0] {
				host.targetName = host.MachineName
				return ArgumentTarget{Host: &host}, true, nil
			}
		}
	}
	return ArgumentTarget{}, false, nil
}

func targetOwnerClaim(ctx context.Context, host HostRecord, info protocol.HostInfo) (HostRecord, error) {
	if host.local {
		host.MachineName, host.NameRevision, host.NameVerified = info.MachineName, info.NameRevision, true
		return host, nil
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return HostRecord{}, fmt.Errorf("locate own naming state: %w", err)
	}
	return localNameRecord(ctx, stateDir)
}
