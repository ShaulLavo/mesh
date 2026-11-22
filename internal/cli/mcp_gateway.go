package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/appcaller"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcpgateway"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/mcpnative"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/protocol/mcpframe"
	"github.com/shaul/mesh/internal/release"
	"github.com/spf13/cobra"
)

const (
	mcpGatewayCacheDir = "mcp-gateway-cache"
	meshServerName     = "mesh"
)

// gatewayHosts resolves a profile's stable host IDs to saved host records on
// every use, so a renamed host keeps its routes and a re-pinned one is
// checked against its current pin.
type gatewayHosts struct{}

func (gatewayHosts) lookup(hostID string) (HostRecord, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return HostRecord{}, fmt.Errorf("locate Mesh state directory: %w", err)
	}
	local, err := agentLocalHost(state)
	if err != nil {
		return HostRecord{}, err
	}
	if local.ID == hostID {
		return local, nil
	}
	hosts, err := LoadHosts()
	if err != nil {
		return HostRecord{}, err
	}
	for _, h := range hosts {
		if h.ID == hostID {
			return h, nil
		}
	}
	return HostRecord{}, &mcpconnect.RouteFailure{Code: agentapi.RouteUnknown, Host: hostID,
		Message: fmt.Sprintf("host %s is not saved on this machine; add it again with mesh add", hostID)}
}

func (a *application) runMCPGateway(cmd *cobra.Command, profileName string, hasIdentity bool, identityName string) error {
	dir, err := meshConfigDir()
	if err != nil {
		return err
	}
	store := mcpgateway.NewStore(dir)
	file, err := store.Load()
	if err != nil {
		return agentapi.Usage("mcp.serve", err.Error())
	}
	profile, ok := file.Profiles[profileName]
	if !ok {
		return agentapi.Usage("mcp.serve", fmt.Sprintf("No MCP profile named %q. Create it with mesh mcp profile add %s.", profileName, profileName))
	}
	var identity appcaller.Identity
	if hasIdentity {
		if identity, err = loadApplicationIdentity("mcp.serve", identityName); err != nil {
			return err
		}
	} else if len(profile.Services) > 0 {
		return agentapi.Usage("mcp.serve", "--profile with hosted services needs --identity NAME; create one with mesh mcp identity create NAME.")
	}
	log.SetOutput(cmd.ErrOrStderr())
	pool := newMCPHostPool(a.dependencies.DialPassive, a.dependencies.Now)
	defer pool.Close()
	caller := protocol.CallerCredential("")
	if hasIdentity {
		caller = protocol.CallerCredential(identity.Credential.Encode())
	}
	gateway, err := newMCPGateway(a, pool, store, profileName, caller)
	if err != nil {
		return err
	}
	server, err := mcpconnect.NewServer(mcpconnect.ServerOptions{
		Version:     release.Metadata().Version,
		Service:     profileName,
		Forwarder:   gateway,
		Stderr:      cmd.ErrOrStderr(),
		OnShutdown:  pool.Close,
		PassResults: true,
	})
	if err != nil {
		return fmt.Errorf("initialize MCP gateway server: %w", err)
	}
	if err := server.Serve(cmd.Context(), a.dependencies.Stdin, a.dependencies.Stdout); err != nil {
		return fmt.Errorf("serve MCP gateway stdio: %w", err)
	}
	return nil
}

func newMCPGateway(a *application, pool *mcpHostPool, store *mcpgateway.Store, profileName string, caller protocol.CallerCredential) (*mcpgateway.Gateway, error) {
	version := release.Metadata().Version
	native, err := mcpnative.NewGatewayNative(mcpnative.ServerOptions{Version: version, Controls: newMCPControls(a, pool, caller)})
	if err != nil {
		return nil, fmt.Errorf("initialize native gateway tools: %w", err)
	}
	state, err := paths.StateDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate Mesh state directory: %w", err)
	}
	tag := mcpCallerTag(caller)
	hosts := gatewayHosts{}
	scope := func(hostID string) string {
		host, err := hosts.lookup(hostID)
		if err != nil || host.MeshIdentity == "" {
			return ""
		}
		return host.MeshIdentity + "\x00" + tag
	}
	serverInfo, err := json.Marshal(struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}{meshServerName, version})
	if err != nil {
		return nil, fmt.Errorf("encode gateway server info: %w", err)
	}
	var secret []byte
	if caller != "" {
		secret = []byte(caller)
	}
	gw, err := mcpgateway.New(mcpgateway.Options{
		Profile:      profileName,
		Store:        store,
		Native:       native,
		Upstream:     gatewayUpstream(pool, hosts, caller),
		Catalog:      &gatewayCatalog{pool: pool, hosts: hosts, caller: caller},
		Cache:        mcpgateway.NewFileCache(filepath.Join(state, mcpGatewayCacheDir), scope, a.dependencies.Now),
		CursorSecret: secret,
		ServerInfo:   serverInfo,
		Now:          a.dependencies.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize MCP gateway: %w", err)
	}
	return gw, nil
}

func mcpCallerTag(caller protocol.CallerCredential) string {
	if caller == "" {
		return "admin"
	}
	credential, err := appcaller.ParseCredential(string(caller))
	if err != nil {
		return "unreadable-credential"
	}
	return "app:" + credential.Fingerprint()
}

func gatewayUpstream(pool *mcpHostPool, hosts gatewayHosts, caller protocol.CallerCredential) func(mcpgateway.Selection) mcpgateway.Upstream {
	return func(sel mcpgateway.Selection) mcpgateway.Upstream {
		instance, err := resolveMCPInstanceArg("mcp.serve", sel.Instance, sel.HostID, sel.ServiceID)
		if err != nil {
			instance = sel.Instance
		}
		return &mcpconnect.H3Forwarder{
			Open: func(ctx context.Context, listen bool) (mcpconnect.Stream, error) {
				host, err := hosts.lookup(sel.HostID)
				if err != nil {
					return nil, err
				}
				return pool.OpenMCPStream(ctx, host, listen)
			},
			Route:    mcpframe.Route{Service: sel.ServiceID, Instance: instance, Caller: caller},
			HostID:   sel.HostID,
			Classify: classifyGatewayTransportError,
		}
	}
}

func classifyGatewayTransportError(err error) (agentapi.Code, bool) {
	var rf *mcpconnect.RouteFailure
	if errors.As(err, &rf) {
		return rf.Code, rf.Retryable
	}
	return classifyMCPTransportError(err)
}

// gatewayCatalog reads one host's passive hosted catalog for the gateway.
type gatewayCatalog struct {
	pool   *mcpHostPool
	hosts  gatewayHosts
	caller protocol.CallerCredential
}

func (c *gatewayCatalog) Services(ctx context.Context, hostID string) ([]mcpgateway.CatalogEntry, error) {
	host, err := c.hosts.lookup(hostID)
	if err != nil {
		return nil, err
	}
	views, err := fetchMCPCatalogViews(ctx, c.pool, host, c.caller)
	if err != nil {
		code, retryable := classifyGatewayTransportError(err)
		return nil, &mcpconnect.RouteFailure{Code: code, Retryable: retryable, Host: hostID, Message: agentapi.CleanMessage(err.Error()), Err: err}
	}
	entries := make([]mcpgateway.CatalogEntry, 0, len(views))
	for _, v := range views {
		entries = append(entries, mcpgateway.CatalogEntry{ServiceID: v.ID, Name: v.Name, Revision: v.Revision, State: classifyMCPServiceState(v)})
	}
	return entries, nil
}

// fetchMCPCatalogViews reads mcp.catalog from one host, retrying once after
// a transport failure as a passive read may.
func fetchMCPCatalogViews(ctx context.Context, pool *mcpHostPool, host HostRecord, caller protocol.CallerCredential) ([]mcphost.ServiceView, error) {
	resp, _, err := pool.ExchangeMCP(ctx, host, protocol.MCPServiceRequest{Type: protocol.TypeMCPCatalog, Caller: caller}, true)
	if err != nil {
		return nil, err
	}
	if resp.Type == protocol.TypeError {
		return nil, typedControlError(resp.ErrorCode, protocol.TypeMCPCatalog, resp.Message, resp.Message)
	}
	var payload struct {
		Services []mcphost.ServiceView `json:"services"`
	}
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		return nil, fmt.Errorf("decode mcp.catalog reply from %s: %w", host.ID, err)
	}
	return payload.Services, nil
}
