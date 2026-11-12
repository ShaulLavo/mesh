package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/machinename"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

func dialAgentHost(ctx context.Context, host HostRecord) (transport.Conn, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate passive authentication state: %w", err)
	}
	_, key, err := identity.LoadPrivate(state)
	if errors.Is(err, os.ErrNotExist) {
		return nil, agentapi.Fail(agentapi.HostUninitialized, "", host.ID, "Set up Mesh here first with mesh install.")
	}
	if err != nil {
		return nil, agentapi.Fail(agentapi.StateUnavailable, "", host.ID, err.Error())
	}
	if host.local {
		return dialAgentLocal(ctx, filepath.Join(state, "daemon.sock"))
	}
	conn, err := transport.DialOnce(ctx, host.Endpoint, transport.DialOptions{Auth: &transport.Authentication{Key: key, ExpectedIdentity: host.MeshIdentity}})
	if err != nil {
		return nil, fmt.Errorf("passive control connection: %w", err)
	}
	return conn, nil
}

func dialAgentLocal(ctx context.Context, socket string) (transport.Conn, error) {
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, daemonDialError(socket, err)
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("adapt passive local connection: %w", err)
	}
	return conn, nil
}

func openAgentHostInfo(ctx context.Context, host HostRecord, dial HostDialer) (transport.Conn, protocol.HostInfo, error) {
	conn, err := dial(ctx, host)
	if err != nil {
		return nil, protocol.HostInfo{}, err
	}
	id, err := newDaemonRequestID()
	if err != nil {
		_ = conn.Close()
		return nil, protocol.HostInfo{}, err
	}
	response, err := controlRequest(ctx, conn, protocol.Control{Type: protocol.TypeHostInfo, RequestID: id})
	if err != nil {
		_ = conn.Close()
		return nil, protocol.HostInfo{}, err
	}
	if response.Type == protocol.TypeError {
		_ = conn.Close()
		return nil, protocol.HostInfo{}, daemonResponseError("host identity", response.Message, response.ErrorCode)
	}
	if err := validateAgentHostInfo(host, response); err != nil {
		_ = conn.Close()
		return nil, protocol.HostInfo{}, err
	}
	return conn, *response.Host, nil
}

func validateAgentHostInfo(host HostRecord, response protocol.Control) error {
	if response.Type != protocol.TypeHostInfoResult || response.Host == nil {
		return errors.New("host did not return host info")
	}
	if err := validateNameEnvelope(response); err != nil {
		return err
	}
	if err := validateHostInfo(host, *response.Host); err != nil {
		return err
	}
	if host.targetName != "" && host.targetName != response.Host.MachineName {
		return &hostIdentityError{label: HostLabel(host)}
	}
	return nil
}

// resolveAgentHost never interprets session IDs, creates identity or synchronizes a connection book.
func resolveAgentHost(ctx context.Context, value string, hosts []HostRecord, cached bool, dial HostDialer) (HostRecord, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return HostRecord{}, fmt.Errorf("locate passive target state: %w", err)
	}
	local, err := agentLocalHost(state)
	if err != nil {
		return HostRecord{}, err
	}
	known := append([]HostRecord(nil), hosts...)
	if local.ID != "" {
		known = agentHostsWithLocal(known, local)
	}
	host, err := matchAgentHost(value, known)
	var failure *agentapi.Failure
	if err == nil || cached || local.ID == "" || !errors.As(err, &failure) || failure.Err.Code != agentapi.HostUnknown {
		return host, err
	}
	if _, finite := ctx.Deadline(); !finite {
		return HostRecord{}, errors.New("passive name refresh requires a finite deadline")
	}
	if dial == nil {
		return HostRecord{}, errors.New("passive name refresh requires a dialer")
	}
	refreshed := refreshAgentNames(ctx, known, dial)
	return matchAgentHost(value, refreshed)
}

func agentLocalHost(state string) (HostRecord, error) {
	owner, _, err := identity.LoadPrivate(state)
	if errors.Is(err, os.ErrNotExist) {
		return HostRecord{local: true}, nil
	}
	if err != nil {
		return HostRecord{}, agentapi.Fail(agentapi.StateUnavailable, "", "", err.Error())
	}
	claim, err := machinename.ReadClaim(state, owner.ID)
	if err != nil {
		return HostRecord{}, fmt.Errorf("read passive local declaration: %w", err)
	}
	return HostRecord{ID: owner.ID, MeshIdentity: owner.ID, MachineName: claim.MachineName, NameRevision: claim.Revision, NameVerified: claim.ID != "", local: true}, nil
}

func agentHostsWithLocal(hosts []HostRecord, local HostRecord) []HostRecord {
	for i := range hosts {
		if hosts[i].ID == local.ID {
			hosts[i] = local
			return hosts
		}
	}
	return append(hosts, local)
}

func matchAgentHost(value string, hosts []HostRecord) (HostRecord, error) {
	for _, host := range hosts {
		if host.ID != "" && host.ID == value {
			return host, nil
		}
	}
	name, err := machinename.Normalize(value)
	if err != nil {
		return HostRecord{}, agentapi.Fail(agentapi.HostUnknown, "", value, "Pass a machine name or exact host ID.")
	}
	matches := make([]HostRecord, 0)
	for _, host := range hosts {
		if host.MachineName == name && machinename.ValidateClaim(host.ID, machinename.Claim{ID: host.ID, MachineName: name, Revision: host.NameRevision}) == nil {
			matches = append(matches, host)
		}
	}
	if len(matches) == 1 {
		matches[0].targetName = name
		return matches[0], nil
	}
	if len(matches) == 0 {
		return HostRecord{}, agentapi.Fail(agentapi.HostUnknown, "", value, "No machine is named "+name+". Pass an exact host ID.")
	}
	ids := make([]string, len(matches))
	for i, host := range matches {
		ids[i] = host.ID
	}
	sort.Strings(ids)
	return HostRecord{}, agentapi.Fail(agentapi.HostAmbiguous, "", value, "Machines share this name: "+strings.Join(ids, ", ")+". Pass an exact host ID.")
}

func refreshAgentNames(ctx context.Context, hosts []HostRecord, dial HostDialer) []HostRecord {
	results := make(chan HostRecord, len(hosts))
	for _, host := range hosts {
		go refreshAgentName(ctx, host, dial, results)
	}
	refreshed := make([]HostRecord, 0, len(hosts))
	for range hosts {
		select {
		case host := <-results:
			refreshed = append(refreshed, host)
		case <-ctx.Done():
			return refreshed
		}
	}
	return refreshed
}

func refreshAgentName(ctx context.Context, host HostRecord, dial HostDialer, results chan<- HostRecord) {
	if host.local {
		results <- host
		return
	}
	host.targetName = ""
	conn, info, err := openAgentHostInfo(ctx, host, dial)
	if err == nil {
		_ = conn.Close()
		host.MachineName, host.NameRevision, host.NameVerified = info.MachineName, info.NameRevision, true
	}
	results <- host
}

// openAgentCatalogCache never creates a missing state directory or database.
func openAgentCatalogCache(ctx context.Context) (*SQLiteCatalogCache, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate passive catalog state: %w", err)
	}
	if _, _, err := identity.LoadPrivate(state); errors.Is(err, os.ErrNotExist) {
		return &SQLiteCatalogCache{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("read passive catalog identity: %w", err)
	}
	path := filepath.Join(state, catalogDatabaseName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return &SQLiteCatalogCache{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat passive catalog: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("catalog cache is not a regular file")
	}
	store, err := storage.OpenAdvisory(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open existing catalog cache: %w", err)
	}
	return &SQLiteCatalogCache{store: store, now: time.Now, stateDir: state}, nil
}
