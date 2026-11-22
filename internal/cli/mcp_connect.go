package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/mcpconnect"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/protocol/mcpframe"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transport"
	"github.com/spf13/cobra"
)

const mcpConnectResolveTimeout = 2 * time.Second

func (a *application) mcpConnectCommand() *cobra.Command {
	var identityName string
	var instanceHandle string
	cmd := &cobra.Command{
		Use:   "connect HOST/NAME",
		Short: "Connect over stdio to one hosted MCP server on any Mesh host",
		Long: "Run a stateless MCP 2026-07-28 stdio server on this machine that forwards\n" +
			"unqualified tool, prompt, and resource names directly to one hosted MCP server\n" +
			"on HOST over an authenticated Mesh H3 stream.\n\n" +
			"stdout carries only newline-delimited JSON-RPC MCP messages; diagnostics, help,\n" +
			"and startup errors go to stderr. Only Mesh is needed on this client.",
		Example: "mesh mcp connect pc/repo-tools --identity codex\n" +
			"mesh mcp connect local/browser --identity claude --instance mi1.HOST.SVC.INST",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return agentapi.Usage("mcp.connect", "Pass one HOST/NAME target, such as pc/repo-tools.")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if private, err := cmd.Flags().GetBool("privacy"); err == nil && private {
				return agentapi.Usage("mcp.connect", "Privacy mode cannot be combined with mesh mcp connect.")
			}
			return a.runMCPConnect(cmd, args[0], identityName, instanceHandle)
		},
	}
	// Keep stdout strictly protocol-only for mesh mcp connect.
	cmd.SetOut(a.dependencies.Stderr)
	cmd.SetErr(a.dependencies.Stderr)
	cmd.Flags().StringVar(&identityName, "identity", "", "local application identity created with mesh mcp identity create NAME (required)")
	cmd.Flags().StringVar(&instanceHandle, "instance", "", "optional instance handle (mi1.<hostId>.<serviceId>.<instanceId>) or instance ID")
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return agentapi.Usage("mcp.connect", err.Error())
	})
	return cmd
}

func (a *application) runMCPConnect(cmd *cobra.Command, ref, identityName, instanceArg string) error {
	hostArg, serviceName, err := parseQualifiedMCPServiceRef(ref, "mcp.connect")
	if err != nil {
		return err
	}
	if strings.TrimSpace(identityName) == "" {
		return agentapi.Usage("mcp.connect", "--identity NAME is required; create one with mesh mcp identity create NAME.")
	}
	id, err := loadApplicationIdentity("mcp.connect", identityName)
	if err != nil {
		return err
	}
	resolveCtx, cancelResolve := context.WithTimeout(cmd.Context(), mcpConnectResolveTimeout)
	host, err := resolveMCPTargetHost(resolveCtx, hostArg, a.dependencies.DialPassive)
	cancelResolve()
	if err != nil {
		return err
	}
	instanceSelector, err := resolveMCPInstanceArg("mcp.connect", instanceArg, host.ID)
	if err != nil {
		return err
	}
	// Keep standard library logs off protocol stdout.
	log.SetOutput(cmd.ErrOrStderr())

	pool := newMCPHostPool(a.dependencies.DialPassive, a.dependencies.Now)
	defer pool.Close()

	forwarder := &mcpconnect.H3Forwarder{
		Open: func(ctx context.Context, listen bool) (mcpconnect.Stream, error) {
			return pool.OpenMCPStream(ctx, host, listen)
		},
		Route: mcpframe.Route{
			Service:  serviceName,
			Instance: instanceSelector,
			Caller:   protocol.CallerCredential(id.Credential.Encode()),
		},
		HostID:   host.ID,
		Classify: classifyMCPTransportError,
	}
	server, err := mcpconnect.NewServer(mcpconnect.ServerOptions{
		Version:    release.Metadata().Version,
		HostID:     host.ID,
		Service:    serviceName,
		Forwarder:  forwarder,
		Stderr:     cmd.ErrOrStderr(),
		OnShutdown: pool.Close,
	})
	if err != nil {
		return fmt.Errorf("initialize MCP connect server: %w", err)
	}
	if err := server.Serve(cmd.Context(), a.dependencies.Stdin, a.dependencies.Stdout); err != nil {
		return fmt.Errorf("serve MCP connect stdio: %w", err)
	}
	return nil
}

func parseQualifiedMCPServiceRef(ref, operation string) (host, service string, err error) {
	host, service, ok := strings.Cut(strings.TrimSpace(ref), "/")
	if !ok || strings.TrimSpace(host) == "" || strings.TrimSpace(service) == "" || strings.Contains(service, "/") {
		return "", "", agentapi.Usage(operation, fmt.Sprintf("Expected HOST/NAME (got %q).", ref))
	}
	return strings.TrimSpace(host), strings.TrimSpace(service), nil
}

// resolveMCPInstanceArg validates mi1 handle syntax and host match locally and
// preserves the full handle verbatim so the owning host can validate the
// resolved service ID and caller ownership (PLAN §5.2).
func resolveMCPInstanceArg(operation, raw, expectedHostID string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "mi1.") {
		return raw, nil
	}
	parsed, err := mcpconnect.ParseInstanceHandle(raw)
	if err != nil {
		return "", agentapi.Usage(operation, err.Error())
	}
	if expectedHostID != "" && parsed.HostID != expectedHostID {
		return "", agentapi.Usage(operation, fmt.Sprintf("Instance handle belongs to host %s, not %s.", parsed.HostID, expectedHostID))
	}
	return raw, nil
}

// resolveMCPTargetHost resolves "local", an exact host ID, or a saved machine
// name without waking any host.
func resolveMCPTargetHost(ctx context.Context, hostArg string, dial HostDialer) (HostRecord, error) {
	state, err := paths.StateDirPath()
	if err != nil {
		return HostRecord{}, fmt.Errorf("locate Mesh state directory: %w", err)
	}
	local, err := agentLocalHost(state)
	if err != nil {
		return HostRecord{}, err
	}
	saved, err := LoadHosts()
	if err != nil {
		return HostRecord{}, err
	}
	return resolveMCPTargetHostWithSaved(ctx, hostArg, false, saved, local, dial)
}

func resolveMCPTargetHostWithSaved(ctx context.Context, hostArg string, cached bool, saved []HostRecord, local HostRecord, dial HostDialer) (HostRecord, error) {
	hostArg = strings.TrimSpace(hostArg)
	if hostArg == "" || hostArg == mcpLocalHost {
		if local.ID == "" {
			return HostRecord{}, agentapi.Fail(agentapi.HostUninitialized, "", "", "Set up Mesh here first with mesh install.")
		}
		return local, nil
	}
	return resolveAgentHost(ctx, hostArg, saved, cached, dial)
}

func classifyMCPTransportError(err error) (agentapi.Code, bool) {
	var failure *agentapi.Failure
	if errors.As(err, &failure) {
		return failure.Err.Code, failure.Err.Retryable
	}
	var mismatch *hostIdentityError
	var rejected *ControlError
	switch {
	case errors.As(err, &mismatch), errors.Is(err, transport.ErrDestinationIdentityMismatch):
		return agentapi.HostIdentityMismatch, false
	case transport.IsAuthenticationInterrupted(err):
		return agentapi.HostUnreachable, true
	case transport.IsAuthenticationRefused(err):
		return agentapi.AccessDenied, false
	case errors.As(err, &rejected):
		code := controlAgentCode(rejected.Code)
		return code, agentapi.Codes[code].Retryable
	default:
		return agentapi.HostUnreachable, true
	}
}
