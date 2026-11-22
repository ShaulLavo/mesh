package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/spf13/cobra"
)

const mcpControlTimeout = 2 * time.Minute

// mcpServiceCommands manage hosted MCP servers on this machine or across the fleet.
func (a *application) mcpServiceCommands() []*cobra.Command {
	commands := []*cobra.Command{
		a.mcpPublishCommand(),
		a.mcpListCommand(),
		a.mcpShowCommand(),
		a.mcpInstanceCommand("start", "mcp.start", "Start a hosted MCP server's instance now", protocol.TypeMCPServiceStart),
		a.mcpInstanceCommand("stop", "mcp.stop", "Stop a hosted MCP server's processes and keep its definition", protocol.TypeMCPServiceStop),
		a.mcpInstanceCommand("restart", "mcp.restart", "Replace a hosted MCP server instance's process", protocol.TypeMCPServiceRestart),
		a.mcpLogsCommand(),
		a.mcpRemoveCommand(),
	}
	// The mcp group sends its own output to stderr to keep the MCP server's
	// stdout protocol-only. These commands print their results to stdout.
	for _, command := range commands {
		command.SetOut(a.dependencies.Stdout)
	}
	return commands
}

type mcpPublishOptions struct {
	host, cwd, url, run, runCwd, accountMode, accountLabel string
	env, secretEnv, headerSecrets                          []string
	shared, kept, asJSON                                   bool
	idle, ready                                            time.Duration
}

func (a *application) mcpPublishCommand() *cobra.Command {
	var o mcpPublishOptions
	cmd := &cobra.Command{
		Use:   "publish NAME [flags] [-- COMMAND [ARG...]]",
		Short: "Publish an MCP server hosted on a Mesh machine",
		Long: "Publish stores a server definition on the target host (this machine by default).\n" +
			"A stdio server runs COMMAND; an HTTP server is reached at --url, optionally started\n" +
			"with --run. Each application caller gets its own isolated instance unless --shared is given.",
		Example: "mesh mcp publish repo-tools --cwd /work/app -- ./repo-mcp\n" +
			"mesh mcp publish search --host pc --url http://127.0.0.1:8080/mcp --shared",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := o.definition(args)
			if err != nil {
				return agentapi.Usage("mcp.publish", err.Error())
			}
			return a.publishMCP(cmd, o.host, d, o.asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.host, "host", "", "target host (machine name, exact host ID, or local; defaults to local)")
	f.StringVar(&o.cwd, "cwd", "", "working directory on the target host for a stdio server")
	f.StringArrayVar(&o.env, "env", nil, "NAME=VALUE for the server's environment; repeat as needed")
	f.StringArrayVar(&o.secretEnv, "secret-env", nil, "NAME=/absolute/file read into NAME at launch; the file must be mode 0600")
	f.StringVar(&o.url, "url", "", "loopback Streamable HTTP endpoint of an HTTP server")
	f.StringArrayVar(&o.headerSecrets, "header-secret", nil, "HEADER=/absolute/file sent to the HTTP server")
	f.StringVar(&o.run, "run", "", "command that starts the HTTP server at --url")
	f.StringVar(&o.runCwd, "run-cwd", "", "working directory for --run")
	f.BoolVar(&o.shared, "shared", false, "give every caller one shared instance")
	f.BoolVar(&o.kept, "kept", false, "keep the server running instead of starting it on demand")
	f.DurationVar(&o.idle, "idle", 0, "stop an on-demand instance after this long unused (default 15m)")
	f.DurationVar(&o.ready, "ready-timeout", 0, "how long the server has to answer server/discover (default 30s)")
	f.StringVar(&o.accountMode, "account-mode", "", "none, or host-account when the server uses the host's credentials")
	f.StringVar(&o.accountLabel, "account-label", "", "non-secret name of the host account, shown to callers")
	f.BoolVar(&o.asJSON, "json", false, "write one agent result")
	return cmd
}

func (o mcpPublishOptions) definition(args []string) (mcphost.Definition, error) {
	env, err := assignments(o.env, "--env")
	if err != nil {
		return mcphost.Definition{}, err
	}
	secrets, err := assignments(o.secretEnv, "--secret-env")
	if err != nil {
		return mcphost.Definition{}, err
	}
	headers, err := assignments(o.headerSecrets, "--header-secret")
	if err != nil {
		return mcphost.Definition{}, err
	}
	d := mcphost.Definition{Name: args[0], Argv: args[1:], Cwd: o.cwd, Env: env, SecretEnv: secrets, URL: o.url, HeaderSecrets: headers,
		RunCwd: o.runCwd, Idle: o.idle, ReadyTimeout: o.ready, AccountMode: o.accountMode, AccountLabel: o.accountLabel}
	if o.run != "" {
		d.RunArgv = []string{"/bin/sh", "-c", o.run}
	}
	if o.shared {
		d.InstancePolicy = mcphost.PolicyShared
	}
	if o.kept {
		d.Lifecycle = mcphost.LifecycleKept
	}
	return d, nil
}

func assignments(values []string, flag string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, value := range values {
		name, rest, ok := strings.Cut(value, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("%s needs NAME=VALUE, got %q", flag, value)
		}
		out[name] = rest
	}
	return out, nil
}

// publishMCP creates the service, or replaces the revision it finds now.
func (a *application) publishMCP(cmd *cobra.Command, hostArg string, d mcphost.Definition, asJSON bool) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
	defer cancel()
	host, err := resolveMCPTargetHost(ctx, hostArg, a.dependencies.DialPassive)
	if err != nil {
		return err
	}
	current, info, err := a.hostMCPServices(ctx, host, "")
	if err != nil {
		return err
	}
	var expected int64
	if index := slices.IndexFunc(current, func(s mcphost.CatalogService) bool { return s.Name == d.Name }); index >= 0 {
		expected = current[index].Revision
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode definition: %w", err)
	}
	var out struct {
		Service mcphost.CatalogService `json:"service"`
	}
	if _, err := a.hostMCPControl(ctx, host, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceUpsert, Definition: raw, ExpectedRevision: expected}, &out, false); err != nil {
		return err
	}
	if asJSON {
		now := a.dependencies.Now().UTC()
		svc := convertOneMCPService(host, hostDisplayName(host, info), out.Service, classifyMCPServiceState(out.Service), string(agentapi.FreshnessLive), &now)
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: svc})
	}
	return printLine(cmd.OutOrStdout(), "published %s revision %d (%s, %s)\n", out.Service.Name, out.Service.Revision, out.Service.Transport, out.Service.InstancePolicy)
}

func (a *application) mcpShowCommand() *cobra.Command {
	var asJSON bool
	var identityName string
	cmd := &cobra.Command{Use: "show [HOST/]NAME", Short: "Show one hosted MCP server and its instances", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		hostArg, name, err := splitOptionalHostServiceRef(args[0], "mcp.show")
		if err != nil {
			return err
		}
		caller, err := optionalMCPCaller("mcp.show", identityName)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
		defer cancel()
		host, err := resolveMCPTargetHost(ctx, hostArg, a.dependencies.DialPassive)
		if err != nil {
			return err
		}
		services, info, err := a.hostMCPServices(ctx, host, caller)
		if err != nil {
			return err
		}
		index := slices.IndexFunc(services, func(s mcphost.CatalogService) bool { return s.Name == name || s.ID == name })
		if index < 0 {
			return agentapi.Fail(agentapi.ServiceNotFound, "mcp.show", name, fmt.Sprintf("No hosted MCP server named %s on %s.", name, mcpHostLabel(host.ID, hostDisplayName(host, info), host.local)))
		}
		if asJSON {
			now := a.dependencies.Now().UTC()
			svc := convertOneMCPService(host, hostDisplayName(host, info), services[index], classifyMCPServiceState(services[index]), string(agentapi.FreshnessLive), &now)
			return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: svc})
		}
		return writeMCPService(cmd.OutOrStdout(), services[index])
	}}
	cmd.Flags().StringVar(&identityName, "identity", "", "act as this local application identity")
	cmd.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	return cmd
}

func writeMCPService(w io.Writer, s mcphost.CatalogService) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (revision %d, %s, %s, %s)\n", s.Name, s.Revision, s.Transport, s.InstancePolicy, s.Lifecycle)
	fmt.Fprintf(&b, "  account: %s\n", mcpAccountText(s.AccountMode, s.AccountLabel))
	if s.ProtocolVersion != "" {
		fmt.Fprintf(&b, "  protocol: %s\n", s.ProtocolVersion)
	}
	for _, inst := range s.Instances {
		fmt.Fprintf(&b, "  instance %s: %s generation %d", inst.ID, inst.State, inst.Generation)
		if inst.Reason != "" {
			fmt.Fprintf(&b, " (%s)", inst.Reason)
		}
		b.WriteString("\n")
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write MCP service: %w", err)
	}
	return nil
}

func (a *application) mcpInstanceCommand(use, operation, short, kind string) *cobra.Command {
	var instance, identityName string
	var asJSON bool
	cmd := &cobra.Command{Use: use + " [HOST/]NAME", Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.runMCPInstanceAction(cmd, operation, kind, args[0], instance, identityName, asJSON)
	}}
	cmd.Flags().StringVar(&instance, "instance", "", "instance handle or ID; without it, start and restart use your own instance and stop stops all of them")
	cmd.Flags().StringVar(&identityName, "identity", "", "act as this local application identity")
	cmd.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	return cmd
}

func (a *application) runMCPInstanceAction(cmd *cobra.Command, operation, kind, ref, instance, identityName string, asJSON bool) error {
	hostArg, name, err := splitOptionalHostServiceRef(ref, operation)
	if err != nil {
		return err
	}
	caller, err := optionalMCPCaller(operation, identityName)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
	defer cancel()
	host, err := resolveMCPTargetHost(ctx, hostArg, a.dependencies.DialPassive)
	if err != nil {
		return err
	}
	instanceSelector, err := resolveMCPInstanceArg(operation, instance, host.ID)
	if err != nil {
		return err
	}
	var out struct {
		Instance  *mcphost.InstanceView  `json:"instance"`
		Instances []mcphost.InstanceView `json:"instances"`
	}
	info, err := a.hostMCPControl(ctx, host, protocol.MCPServiceRequest{Type: kind, Service: name, Instance: instanceSelector, Caller: caller}, &out, false)
	if err != nil {
		return err
	}
	if out.Instance != nil {
		out.Instances = append(out.Instances, *out.Instance)
	}
	if asJSON {
		data := agentapi.MCPServiceInstancesData{
			HostID:      host.ID,
			MachineName: hostDisplayName(host, info),
			Service:     name,
			Instances:   convertMCPInstances(host.ID, out.Instances),
		}
		return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: data})
	}
	for _, inst := range out.Instances {
		if err := printLine(cmd.OutOrStdout(), "%s instance %s: %s\n", name, inst.ID, inst.State); err != nil {
			return err
		}
	}
	return nil
}

func convertMCPInstances(hostID string, views []mcphost.InstanceView) []agentapi.MCPCatalogInstance {
	items := make([]agentapi.MCPCatalogInstance, 0, len(views))
	for _, inst := range views {
		handle := ""
		if inst.ServiceID != "" {
			handle, _ = mcpconnect.FormatInstanceHandle(hostID, inst.ServiceID, inst.ID)
		}
		items = append(items, agentapi.MCPCatalogInstance{
			ID: inst.ID, Handle: handle, Shared: inst.Shared, Mine: inst.Mine, Default: inst.Default,
			State: inst.State, Reason: inst.Reason, ExitCode: inst.ExitCode, Revision: inst.Revision,
			Generation: inst.Generation, Active: inst.Active, Leases: inst.Leases,
			ServerInfo:   toAgentServerInfo(mcphost.ProjectServerInfo(inst.ServerInfo)),
			Capabilities: toAgentCapabilities(mcphost.ProjectCapabilities(inst.Capabilities)),
			LateReplies:  inst.LateReplies, UpdatedAt: inst.UpdatedAt,
		})
	}
	return items
}

func (a *application) mcpLogsCommand() *cobra.Command {
	var instance, identityName string
	var asJSON bool
	cmd := &cobra.Command{Use: "logs [HOST/]NAME", Short: "Print a hosted MCP server instance's recent stderr", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		hostArg, name, err := splitOptionalHostServiceRef(args[0], "mcp.logs")
		if err != nil {
			return err
		}
		caller, err := optionalMCPCaller("mcp.logs", identityName)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
		defer cancel()
		host, err := resolveMCPTargetHost(ctx, hostArg, a.dependencies.DialPassive)
		if err != nil {
			return err
		}
		instanceSelector, err := resolveMCPInstanceArg("mcp.logs", instance, host.ID)
		if err != nil {
			return err
		}
		var out struct {
			Stderr []byte `json:"stderr"`
		}
		info, err := a.hostMCPControl(ctx, host, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceLogs, Service: name, Instance: instanceSelector, Caller: caller}, &out, true)
		if err != nil {
			return err
		}
		if asJSON {
			data := agentapi.MCPServiceLogsData{
				HostID:      host.ID,
				MachineName: hostDisplayName(host, info),
				Service:     name,
				Instance:    instanceSelector,
				Stderr:      string(out.Stderr),
				Bytes:       len(out.Stderr),
			}
			return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: data})
		}
		if _, err := cmd.OutOrStdout().Write(out.Stderr); err != nil {
			return fmt.Errorf("write MCP diagnostics: %w", err)
		}
		return nil
	}}
	cmd.Flags().StringVar(&instance, "instance", "", "instance handle or ID, required when the server has several instances")
	cmd.Flags().StringVar(&identityName, "identity", "", "act as this local application identity")
	cmd.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	return cmd
}

func (a *application) mcpRemoveCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "rm [HOST/]NAME", Short: "Remove a hosted MCP server and stop its processes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		hostArg, name, err := splitOptionalHostServiceRef(args[0], "mcp.remove")
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
		defer cancel()
		host, err := resolveMCPTargetHost(ctx, hostArg, a.dependencies.DialPassive)
		if err != nil {
			return err
		}
		info, err := a.hostMCPControl(ctx, host, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceDelete, Service: name}, nil, false)
		if err != nil {
			return err
		}
		if asJSON {
			data := agentapi.MCPServiceRemoveData{HostID: host.ID, MachineName: hostDisplayName(host, info), Service: name, Removed: true}
			return a.agent.write(cmd.OutOrStdout(), agentapi.Envelope{Status: agentapi.StatusOK, Data: data})
		}
		return printLine(cmd.OutOrStdout(), "removed %s\n", name)
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	return cmd
}

// splitOptionalHostServiceRef accepts NAME (defaulting to local) or HOST/NAME.
func splitOptionalHostServiceRef(ref, operation string) (host, service string, err error) {
	ref = strings.TrimSpace(ref)
	if !strings.Contains(ref, "/") {
		if ref == "" {
			return "", "", agentapi.Usage(operation, "Service name must not be empty.")
		}
		return mcpLocalHost, ref, nil
	}
	return parseQualifiedMCPServiceRef(ref, operation)
}

func optionalMCPCaller(operation, identityName string) (protocol.CallerCredential, error) {
	if strings.TrimSpace(identityName) == "" {
		return "", nil
	}
	id, err := loadApplicationIdentity(operation, identityName)
	if err != nil {
		return "", err
	}
	return protocol.CallerCredential(id.Credential.Encode()), nil
}

const mcpLocalHost = "local"

func printLine(w io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func (a *application) hostMCPServices(ctx context.Context, host HostRecord, caller protocol.CallerCredential) ([]mcphost.CatalogService, protocol.HostInfo, error) {
	var out struct {
		Services []mcphost.CatalogService `json:"services"`
	}
	info, err := a.hostMCPControl(ctx, host, protocol.MCPServiceRequest{Type: protocol.TypeMCPCatalog, Caller: caller}, &out, true)
	if err != nil {
		return nil, info, err
	}
	return out.Services, info, nil
}

func (a *application) hostMCPControl(ctx context.Context, host HostRecord, req protocol.MCPServiceRequest, result any, retryRead bool) (protocol.HostInfo, error) {
	pool := newMCPHostPool(a.dependencies.DialPassive, a.dependencies.Now)
	defer pool.Close()
	response, info, err := pool.ExchangeMCP(ctx, host, req, retryRead)
	if err != nil {
		return info, err
	}
	if response.Type == protocol.TypeError {
		return info, typedControlError(response.ErrorCode, req.Type, response.Message, fmt.Sprintf("%s: %s", response.ErrorCode, response.Message))
	}
	if result == nil {
		return info, nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return info, fmt.Errorf("decode %s result: %w", req.Type, err)
	}
	return info, nil
}

func exchangeMCP(conn transport.Conn, req protocol.MCPServiceRequest) (protocol.MCPServiceResponse, error) {
	frame, err := protocol.EncodeMCP(req)
	if err != nil {
		return protocol.MCPServiceResponse{}, fmt.Errorf("encode %s: %w", req.Type, err)
	}
	if err := conn.WriteFrame(frame); err != nil {
		return protocol.MCPServiceResponse{}, fmt.Errorf("write %s: %w", req.Type, err)
	}
	reply, err := conn.ReadFrame()
	if err != nil {
		return protocol.MCPServiceResponse{}, fmt.Errorf("read %s reply: %w", req.Type, err)
	}
	var response protocol.MCPServiceResponse
	if err := json.Unmarshal(reply.Payload, &response); err != nil {
		return response, fmt.Errorf("decode %s reply: %w", req.Type, err)
	}
	if response.RequestID != req.RequestID {
		return response, errors.New("cli: MCP reply has a mismatched request ID")
	}
	return response, nil
}
