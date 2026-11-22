package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	meshdaemon "github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/mcphost"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
	"github.com/spf13/cobra"
)

const mcpControlTimeout = 2 * time.Minute

// mcpServiceCommands manage MCP servers hosted by this machine's daemon.
func (a *application) mcpServiceCommands() []*cobra.Command {
	commands := []*cobra.Command{
		a.mcpPublishCommand(),
		a.mcpListCommand(),
		a.mcpShowCommand(),
		a.mcpInstanceCommand("start", "Start a hosted MCP server's instance now", protocol.TypeMCPServiceStart),
		a.mcpInstanceCommand("stop", "Stop a hosted MCP server's processes and keep its definition", protocol.TypeMCPServiceStop),
		a.mcpInstanceCommand("restart", "Replace a hosted MCP server instance's process", protocol.TypeMCPServiceRestart),
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
	cwd, url, run, runCwd, accountMode, accountLabel string
	env, secretEnv, headerSecrets                    []string
	shared, kept, asJSON                             bool
	idle, ready                                      time.Duration
}

func (a *application) mcpPublishCommand() *cobra.Command {
	var o mcpPublishOptions
	cmd := &cobra.Command{
		Use:   "publish NAME [flags] [-- COMMAND [ARG...]]",
		Short: "Publish an MCP server hosted on this machine",
		Long: "Publish stores a server definition on this machine. A stdio server runs COMMAND; " +
			"an HTTP server is reached at --url, optionally started with --run. Each application caller " +
			"gets its own isolated instance unless --shared is given.",
		Example: "mesh mcp publish repo-tools --cwd /work/app -- ./repo-mcp\n" +
			"mesh mcp publish search --url http://127.0.0.1:8080/mcp --shared",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := o.definition(args)
			if err != nil {
				return err
			}
			return a.publishMCP(cmd, d, o.asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.cwd, "cwd", "", "working directory on this machine for a stdio server")
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
	f.StringVar(&o.accountMode, "account-mode", "", "none, or host-account when the server uses this machine's credentials")
	f.StringVar(&o.accountLabel, "account-label", "", "non-secret name of the host account, shown to callers")
	f.BoolVar(&o.asJSON, "json", false, "write the stored definition as JSON")
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
func (a *application) publishMCP(cmd *cobra.Command, d mcphost.Definition, asJSON bool) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
	defer cancel()
	current, err := localMCPServices(ctx)
	if err != nil {
		return err
	}
	var expected int64
	if index := slices.IndexFunc(current, func(s mcphost.ServiceView) bool { return s.Name == d.Name }); index >= 0 {
		expected = current[index].Revision
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode definition: %w", err)
	}
	var out struct {
		Service mcphost.ServiceView `json:"service"`
	}
	if err := localMCPControl(ctx, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceUpsert, Definition: raw, ExpectedRevision: expected}, &out); err != nil {
		return err
	}
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), out.Service)
	}
	return printLine(cmd.OutOrStdout(), "published %s revision %d (%s, %s)\n", out.Service.Name, out.Service.Revision, out.Service.Transport, out.Service.InstancePolicy)
}

func (a *application) mcpListCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "ls", Short: "List MCP servers hosted on this machine", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
		defer cancel()
		services, err := localMCPServices(ctx)
		if err != nil {
			return err
		}
		if asJSON {
			return writeJSON(cmd.OutOrStdout(), map[string]any{"services": services})
		}
		return writeMCPTable(cmd.OutOrStdout(), services)
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "write the services as JSON")
	return cmd
}

func writeMCPTable(w io.Writer, services []mcphost.ServiceView) error {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "NAME\tTRANSPORT\tPOLICY\tLIFECYCLE\tACCOUNT\tINSTANCES")
	for _, s := range services {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, s.Transport, s.InstancePolicy, s.Lifecycle, accountText(s), instanceSummary(s.Instances))
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write MCP services: %w", err)
	}
	return nil
}

func accountText(s mcphost.ServiceView) string {
	if s.AccountMode == mcphost.AccountHostAccount {
		return s.AccountLabel
	}
	return s.AccountMode
}

func instanceSummary(instances []mcphost.InstanceView) string {
	if len(instances) == 0 {
		return "none"
	}
	states := map[string]int{}
	for _, inst := range instances {
		states[inst.State]++
	}
	parts := make([]string, 0, len(states))
	for _, state := range slices.Sorted(maps.Keys(states)) {
		parts = append(parts, fmt.Sprintf("%d %s", states[state], state))
	}
	return strings.Join(parts, ", ")
}

func (a *application) mcpShowCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "show NAME", Short: "Show one hosted MCP server and its instances", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name, err := localMCPName(args[0])
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
		defer cancel()
		services, err := localMCPServices(ctx)
		if err != nil {
			return err
		}
		index := slices.IndexFunc(services, func(s mcphost.ServiceView) bool { return s.Name == name || s.ID == name })
		if index < 0 {
			return fmt.Errorf("no hosted MCP server named %s on this machine", name)
		}
		if asJSON {
			return writeJSON(cmd.OutOrStdout(), services[index])
		}
		return writeMCPService(cmd.OutOrStdout(), services[index])
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "write the service as JSON")
	return cmd
}

func writeMCPService(w io.Writer, s mcphost.ServiceView) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (revision %d, %s, %s, %s)\n", s.Name, s.Revision, s.Transport, s.InstancePolicy, s.Lifecycle)
	if len(s.Command) > 0 {
		fmt.Fprintf(&b, "  command: %s\n", strings.Join(s.Command, " "))
	}
	if s.URL != "" {
		fmt.Fprintf(&b, "  url: %s\n", s.URL)
	}
	fmt.Fprintf(&b, "  account: %s\n", accountText(s))
	if len(s.SecretNames) > 0 {
		fmt.Fprintf(&b, "  secret variables: %s\n", strings.Join(s.SecretNames, ", "))
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

func (a *application) mcpInstanceCommand(use, short, kind string) *cobra.Command {
	var instance string
	cmd := &cobra.Command{Use: use + " NAME", Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name, err := localMCPName(args[0])
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
		defer cancel()
		var out struct {
			Instance  *mcphost.InstanceView  `json:"instance"`
			Instances []mcphost.InstanceView `json:"instances"`
		}
		if err := localMCPControl(ctx, protocol.MCPServiceRequest{Type: kind, Service: name, Instance: instance}, &out); err != nil {
			return err
		}
		if out.Instance != nil {
			out.Instances = append(out.Instances, *out.Instance)
		}
		for _, inst := range out.Instances {
			if err := printLine(cmd.OutOrStdout(), "%s instance %s: %s\n", name, inst.ID, inst.State); err != nil {
				return err
			}
		}
		return nil
	}}
	cmd.Flags().StringVar(&instance, "instance", "", "instance ID; without it, start and restart use your own instance and stop stops all of them")
	return cmd
}

func (a *application) mcpLogsCommand() *cobra.Command {
	var instance string
	cmd := &cobra.Command{Use: "logs NAME", Short: "Print a hosted MCP server instance's recent stderr", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name, err := localMCPName(args[0])
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
		defer cancel()
		var out struct {
			Stderr []byte `json:"stderr"`
		}
		if err := localMCPControl(ctx, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceLogs, Service: name, Instance: instance}, &out); err != nil {
			return err
		}
		if _, err := cmd.OutOrStdout().Write(out.Stderr); err != nil {
			return fmt.Errorf("write MCP diagnostics: %w", err)
		}
		return nil
	}}
	cmd.Flags().StringVar(&instance, "instance", "", "instance ID, required when the server has several instances")
	return cmd
}

func (a *application) mcpRemoveCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "rm NAME", Short: "Remove a hosted MCP server and stop its processes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name, err := localMCPName(args[0])
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), mcpControlTimeout)
		defer cancel()
		if err := localMCPControl(ctx, protocol.MCPServiceRequest{Type: protocol.TypeMCPServiceDelete, Service: name}, nil); err != nil {
			return err
		}
		return printLine(cmd.OutOrStdout(), "removed %s\n", name)
	}}
	return cmd
}

// localMCPName accepts NAME or local/NAME. Other hosts' servers are reached
// through mesh mcp connect.
func localMCPName(ref string) (string, error) {
	host, name, qualified := strings.Cut(ref, "/")
	if !qualified {
		return ref, nil
	}
	if host != mcpLocalHost {
		return "", fmt.Errorf("mesh mcp %s manages this machine's servers; use NAME or local/NAME", ref)
	}
	return name, nil
}

const mcpLocalHost = "local"

func printLine(w io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func localMCPServices(ctx context.Context) ([]mcphost.ServiceView, error) {
	var out struct {
		Services []mcphost.ServiceView `json:"services"`
	}
	if err := localMCPControl(ctx, protocol.MCPServiceRequest{Type: protocol.TypeMCPCatalog}, &out); err != nil {
		return nil, err
	}
	return out.Services, nil
}

// localMCPControl sends one hosted MCP control to this machine's daemon as
// whole-account administration.
func localMCPControl(ctx context.Context, req protocol.MCPServiceRequest, result any) error {
	stateDir, err := paths.StateDir()
	if err != nil {
		return fmt.Errorf("locate the local daemon: %w", err)
	}
	if req.RequestID, err = newDaemonRequestID(); err != nil {
		return err
	}
	stream, err := (&net.Dialer{}).DialContext(ctx, "unix", meshdaemon.SocketPath(stateDir))
	if err != nil {
		return daemonDialError(meshdaemon.SocketPath(stateDir), err)
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return fmt.Errorf("cli: adapt daemon connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	response, err := exchangeMCP(conn, req)
	if err != nil {
		return daemonOperationError(ctx, req.Type, err)
	}
	if response.Type == protocol.TypeError {
		return fmt.Errorf("%s: %s", response.ErrorCode, response.Message)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("decode %s result: %w", req.Type, err)
	}
	return nil
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

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}
