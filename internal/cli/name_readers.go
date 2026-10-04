package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"

	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
)

const retainedNameSuffix = " · cached name"

// HostLabel never substitutes OS or viewer labels for an owner's declaration.
func HostLabel(host HostRecord) string {
	if host.MachineName != "" {
		label := host.MachineName
		if host.NameConflict {
			label += " [" + host.NameSuffix + "] conflict"
			if host.NamePriority {
				label += " priority"
			}
		}
		return label
	}
	if host.ID != "" {
		return host.ID
	}
	return "Local machine"
}

func cacheBootstrapName(ctx context.Context, host HostRecord, authenticatedIdentity string) error {
	if authenticatedIdentity == "" || host.ID != authenticatedIdentity || host.MeshIdentity != authenticatedIdentity {
		return fmt.Errorf("bootstrap declaration differs from the authenticated destination; add this machine again")
	}
	if host.MachineName == "" && host.NameRevision == 0 {
		return fmt.Errorf("destination %s has no naming declaration; update that destination", host.ID)
	}
	owner := host
	owner.ID, owner.MeshIdentity = authenticatedIdentity, authenticatedIdentity
	return rememberHostName(ctx, owner, protocol.HostInfo{ID: host.ID, MeshIdentity: host.MeshIdentity, MachineName: host.MachineName, NameRevision: host.NameRevision})
}

func localDeclaredHost(ctx context.Context, stateDir string) (HostRecord, error) {
	return declaredSocketHost(ctx, filepath.Join(stateDir, "daemon.sock"))
}

func declaredSocketHost(ctx context.Context, socket string) (HostRecord, error) {
	requestID, err := newDaemonRequestID()
	if err != nil {
		return HostRecord{}, err
	}
	response, err := daemonControlRequest(ctx, socket, protocol.Control{Type: protocol.TypeHostInfo, RequestID: requestID})
	if err != nil {
		return HostRecord{}, err
	}
	if response.Type != protocol.TypeHostInfoResult || response.Host == nil {
		return HostRecord{}, fmt.Errorf("local daemon did not report a machine declaration")
	}
	if err := validateNameEnvelope(response); err != nil {
		return HostRecord{}, err
	}
	info := *response.Host
	if err := machinename.ValidateClaim(info.ID, declaredName(info)); err != nil {
		return HostRecord{}, fmt.Errorf("validate local declaration: %w", err)
	}
	if info.ID != info.MeshIdentity {
		return HostRecord{}, fmt.Errorf("local declaration identity differs from its owner")
	}
	return HostRecord{ID: info.ID, MeshIdentity: info.MeshIdentity, MachineName: info.MachineName, NameRevision: info.NameRevision, NameVerified: true, local: true}, nil
}

func localHostID() string {
	stateDir, err := paths.StateDir()
	if err != nil {
		return ""
	}
	host, err := existingLocalIdentity(stateDir)
	if err != nil {
		return ""
	}
	return host.ID
}

func localHostLabel(parent context.Context) string {
	stateDir, err := paths.StateDir()
	if err != nil {
		return localHostID()
	}
	ctx, cancel := context.WithTimeout(parent, localQueryTimeout)
	defer cancel()
	host, err := localDeclaredHost(ctx, stateDir)
	if err != nil {
		return localHostID()
	}
	if host.ID != localHostID() {
		return localHostID()
	}
	return HostLabel(host)
}

func localNameRecord(ctx context.Context, stateDir string) (HostRecord, error) {
	owner, err := existingLocalIdentity(stateDir)
	if err != nil {
		return HostRecord{}, err
	}
	host := HostRecord{ID: owner.ID, MeshIdentity: owner.ID, local: true}
	if owner.ID == "" {
		return host, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, localQueryTimeout)
	defer cancel()
	declared, err := localDeclaredHost(queryCtx, stateDir)
	if err != nil {
		var network *net.OpError
		if errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) {
			return host, nil
		}
		return HostRecord{}, err
	}
	if declared.ID != owner.ID {
		return HostRecord{}, fmt.Errorf("local daemon declaration differs from this machine's identity")
	}
	return declared, nil
}

// ProjectHostNames decorates known owner claims without changing their names or IDs.
func ProjectHostNames(hosts []HostRecord) {
	claims := make([]machinename.Claim, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		if seen[host.ID] {
			continue
		}
		seen[host.ID] = true
		if host.MachineName != "" {
			claims = append(claims, machinename.Claim{ID: host.ID, MachineName: host.MachineName, Revision: host.NameRevision})
		}
	}
	rows := machinename.Project(claims)
	byID := make(map[string]machinename.Projection, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	for index := range hosts {
		row := byID[hosts[index].ID]
		hosts[index].NameConflict, hosts[index].NamePriority, hosts[index].NameSuffix = row.Conflict, row.Priority, row.Suffix
	}
}

// VerifyNamedHost checks a bare-name intent against its authenticated owner before bootstrap effects.
func VerifyNamedHost(ctx context.Context, host HostRecord) error {
	return verifyNamedHost(ctx, host, dialControlHost)
}

func verifyNamedHost(ctx context.Context, host HostRecord, dial HostDialer) error {
	if host.targetName == "" {
		return nil
	}
	query, cancel := context.WithTimeout(ctx, remoteConnectTimeout)
	defer cancel()
	conn, _, err := openVerifiedHostInfo(query, host, dial)
	if err != nil {
		return err
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("close naming verification: %w", err)
	}
	return nil
}

func machineTargetHosts(ctx context.Context) ([]HostRecord, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return nil, err
	}
	stateDir, err := paths.StateDir()
	if err != nil {
		return nil, fmt.Errorf("locate target naming state: %w", err)
	}
	owner, err := localNameRecord(ctx, stateDir)
	if err != nil {
		return nil, err
	}
	return withOwnerClaim(hosts, owner), nil
}

func withOwnerClaim(hosts []HostRecord, owner HostRecord) []HostRecord {
	if owner.ID == "" {
		return hosts
	}
	known := make([]HostRecord, 0, len(hosts)+1)
	for _, host := range hosts {
		if host.ID != owner.ID {
			known = append(known, host)
		}
	}
	known = append(known, owner)
	ProjectHostNames(known)
	return known
}
