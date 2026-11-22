package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	gateway, err := newMCPGateway(a, pool, store, profileName, caller, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	server, err := mcpgateway.NewFrontend(gateway, mcpgateway.FrontendOptions{Stderr: cmd.ErrOrStderr(), OnShutdown: pool.Close})
	if err != nil {
		return fmt.Errorf("initialize MCP gateway server: %w", err)
	}
	if err := server.Serve(cmd.Context(), a.dependencies.Stdin, a.dependencies.Stdout); err != nil {
		return fmt.Errorf("serve MCP gateway stdio: %w", err)
	}
	return nil
}

func newMCPGateway(a *application, pool *mcpHostPool, store *mcpgateway.Store, profileName string, caller protocol.CallerCredential, stderr io.Writer) (*mcpgateway.Gateway, error) {
	version := release.Metadata().Version
	native, err := mcpnative.NewGatewayNative(mcpnative.ServerOptions{Version: version, Controls: newMCPControls(a, pool, caller)})
	if err != nil {
		return nil, fmt.Errorf("initialize native gateway tools: %w", err)
	}
	state, err := paths.StateDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate Mesh state directory: %w", err)
	}
	local, err := agentLocalHost(state)
	if err != nil {
		return nil, err
	}
	tag := mcpCallerScopeFor(caller, local.ID)
	hosts := gatewayHosts{}
	scope := func(hostID string) string {
		host, err := hosts.lookup(hostID)
		if err != nil || host.MeshIdentity == "" || tag == "" {
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
		Hosted:       caller != "",
		Catalog:      &gatewayCatalog{pool: pool, hosts: hosts, caller: caller},
		Cache:        mcpgateway.NewFileCache(filepath.Join(state, mcpGatewayCacheDir), scope),
		CursorSecret: secret,
		ServerInfo:   serverInfo,
		Now:          a.dependencies.Now,
		Log:          func(line string) { _, _ = io.WriteString(stderr, line) },
	})
	if err != nil {
		return nil, fmt.Errorf("initialize MCP gateway: %w", err)
	}
	return gw, nil
}

// mcpCallerScopeFor names the private cache scope of a caller credential the
// same way mesh mcp ls does: the full derived application ID on this device,
// or whole-account administration.
func mcpCallerScopeFor(caller protocol.CallerCredential, localHostID string) string {
	principal := localHostID
	if principal == "" {
		principal = mcpUninitializedPrincipal
	}
	if caller == "" {
		return "device:" + principal + ":admin"
	}
	credential, err := appcaller.ParseCredential(string(caller))
	if err != nil {
		return ""
	}
	derived, err := appcaller.Derive(appcaller.Scope{Class: appcaller.ScopeDevice, Principal: principal}, credential)
	if err != nil {
		return ""
	}
	return "app:" + string(derived)
}

// gatewayUpstream builds the H3 forwarder for one call: by exact service ID,
// bound to the revision and account the gateway resolved, and always with
// the frontend's verified application credential.
func gatewayUpstream(pool *mcpHostPool, hosts gatewayHosts, caller protocol.CallerCredential) func(mcpgateway.Selection, *mcpgateway.Expect) mcpgateway.Upstream {
	return func(sel mcpgateway.Selection, expect *mcpgateway.Expect) mcpgateway.Upstream {
		instance, err := resolveMCPInstanceArg("mcp.serve", sel.Instance, sel.HostID)
		if err != nil {
			instance = sel.Instance
		}
		route := mcpframe.Route{Service: sel.ServiceID, Instance: instance, Caller: caller, ExactID: true}
		if expect != nil {
			route.Expect = &mcpframe.RouteExpect{Revision: expect.Revision, Account: expect.Account}
		}
		return &mcpconnect.H3Forwarder{
			Open:     gatewayOpener(pool, hosts, caller, sel.HostID),
			Route:    route,
			HostID:   sel.HostID,
			Classify: classifyGatewayTransportError,
		}
	}
}

// gatewayOpener opens H3 streams to one host, refusing to without the
// frontend's verified application credential.
func gatewayOpener(pool *mcpHostPool, hosts gatewayHosts, caller protocol.CallerCredential, hostID string) mcpconnect.Opener {
	return func(ctx context.Context, listen bool) (mcpconnect.Stream, error) {
		if caller == "" {
			return nil, &mcpconnect.RouteFailure{Code: agentapi.AccessDenied, Host: hostID, Message: "hosted services need a verified application identity"}
		}
		host, err := hosts.lookup(hostID)
		if err != nil {
			return nil, err
		}
		return pool.OpenMCPStream(ctx, host, listen)
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

func (c *gatewayCatalog) Services(ctx context.Context, hostID string) (mcpgateway.HostCatalog, error) {
	host, err := c.hosts.lookup(hostID)
	if err != nil {
		return mcpgateway.HostCatalog{}, err
	}
	reply, err := fetchMCPCatalog(ctx, c.pool, host, c.caller)
	if err != nil {
		return mcpgateway.HostCatalog{}, gatewayFailure(hostID, err)
	}
	catalog := mcpgateway.HostCatalog{Scope: reply.Scope, Entries: make([]mcpgateway.CatalogEntry, 0, len(reply.Services))}
	for _, v := range reply.Services {
		catalog.Entries = append(catalog.Entries, mcpgateway.CatalogEntry{ServiceID: v.ID, Name: v.Name, Revision: v.Revision, Account: v.AccountLabel,
			ListingRevision: v.ListingRevision, State: classifyMCPServiceState(v)})
	}
	return catalog, nil
}

func gatewayFailure(hostID string, err error) error {
	code, retryable := classifyGatewayTransportError(err)
	return &mcpconnect.RouteFailure{Code: code, Retryable: retryable, Host: hostID, Message: agentapi.CleanMessage(err.Error()), Err: err}
}

type mcpCatalogReply struct {
	Scope    string                   `json:"scope"`
	Services []mcphost.CatalogService `json:"services"`
}

func fetchMCPCatalogViews(ctx context.Context, pool *mcpHostPool, host HostRecord, caller protocol.CallerCredential) ([]mcphost.CatalogService, error) {
	reply, err := fetchMCPCatalog(ctx, pool, host, caller)
	return reply.Services, err
}

// fetchMCPCatalog reads mcp.catalog from one host, retrying once after a
// transport failure as a passive read may.
func fetchMCPCatalog(ctx context.Context, pool *mcpHostPool, host HostRecord, caller protocol.CallerCredential) (mcpCatalogReply, error) {
	resp, _, err := pool.ExchangeMCP(ctx, host, protocol.MCPServiceRequest{Type: protocol.TypeMCPCatalog, Caller: caller}, true)
	if err != nil {
		return mcpCatalogReply{}, err
	}
	if resp.Type == protocol.TypeError {
		return mcpCatalogReply{}, typedControlError(resp.ErrorCode, protocol.TypeMCPCatalog, resp.Message, resp.Message)
	}
	var reply mcpCatalogReply
	if err := json.Unmarshal(resp.Result, &reply); err != nil {
		return mcpCatalogReply{}, fmt.Errorf("decode mcp.catalog reply from %s: %w", host.ID, err)
	}
	return reply, nil
}
