package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/shaul/mesh/internal/agentapi"
	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/update"
	"github.com/spf13/cobra"
)

func (a *application) hostsCommand() *cobra.Command {
	var asJSON, cached bool
	var timeout time.Duration
	command := &cobra.Command{Use: "hosts", Short: "Read machine context without waking machines", Args: profileArgs("hosts.list"), RunE: func(cmd *cobra.Command, _ []string) error {
		if err := validateAgentHostFlags("hosts.list", asJSON, timeout, 30*time.Second); err != nil {
			return err
		}
		return a.runAgentHosts(cmd, cached, timeout)
	}}
	command.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	command.Flags().BoolVar(&cached, "cached", false, "read local files and cached observations without dialing")
	command.Flags().DurationVar(&timeout, "timeout", 2*time.Second, "hard deadline for host queries (100ms to 30s)")
	return command
}

func (a *application) inspectHostCommand() *cobra.Command {
	var asJSON, cached bool
	var timeout time.Duration
	command := &cobra.Command{Use: "inspect HOST", Short: "Read full host and fleet notes and current access", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return agentapi.Usage("hosts.inspect", "Pass one machine name or exact host ID.")
		}
		return nil
	}, RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAgentHostFlags("hosts.inspect", asJSON, timeout, 60*time.Second); err != nil {
			return err
		}
		return a.runAgentInspect(cmd, args[0], cached, timeout)
	}}
	command.Flags().BoolVar(&asJSON, "json", false, "write one agent result")
	command.Flags().BoolVar(&cached, "cached", false, "read local files and cached observations without dialing")
	command.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "hard deadline for host queries (100ms to 60s)")
	return command
}

func validateAgentHostFlags(operation string, asJSON bool, timeout, maximum time.Duration) error {
	if !asJSON {
		return agentapi.Usage(operation, "Use --json for machine context reads.")
	}
	if timeout < 100*time.Millisecond || timeout > maximum {
		return agentapi.Usage(operation, fmt.Sprintf("Use --timeout between 100ms and %s.", maximum))
	}
	return nil
}

func (a *application) runAgentHosts(cmd *cobra.Command, cached bool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	reader, err := newAgentHostReader("hosts.list", false, cached, a)
	if err != nil {
		return err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	local, err := agentLocalHost(reader.state)
	if err != nil {
		return err
	}
	hosts = agentHostsWithLocal(hosts, local)
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	results := collectAgentHosts(ctx, hosts, reader)
	authority := &update.Authority{StateDir: reader.state, ID: local.ID}
	fleet, _ := hostprofile.OpenFleetStore(reader.state).ReadReplica(authority.IsAdministrator)
	data := agentapi.HostsData{ReceivedAt: a.dependencies.Now().UTC(), Fleet: contextDeclared(hostprofile.ContextReply{Fleet: hostprofile.FleetSection(fleet, true)}, agentapi.FreshnessLive, a.dependencies.Now().UTC()).Fleet, Hosts: make([]agentapi.HostSummary, len(results))}
	diagnostics := []agentapi.Error{}
	for i, result := range results {
		data.Hosts[i] = result.host
		diagnostics = append(diagnostics, result.errors...)
	}
	if fleet.State == hostprofile.StateUnavailable {
		diagnostics = append(diagnostics, reader.diagnostic(errors.New(fleet.Problem), local.ID, agentapi.StateUnavailable))
	}
	if !cached && local.ID != "" {
		if err := pruneAgentContext(ctx, reader, hosts); err != nil {
			diagnostics = append(diagnostics, reader.diagnostic(err, "", agentapi.StateUnavailable))
		}
	}
	envelope := agentapi.Envelope{Status: agentapi.StatusOK, Data: data, Errors: diagnostics}
	if len(diagnostics) > 0 {
		envelope.Status = agentapi.StatusPartial
	}
	return a.agent.write(cmd.OutOrStdout(), envelope)
}

type indexedAgentHost struct {
	index  int
	result agentHostRead
}

func collectAgentHosts(ctx context.Context, hosts []HostRecord, reader agentHostReader) []agentHostRead {
	channel := make(chan indexedAgentHost, len(hosts))
	for i, host := range hosts {
		go func() { channel <- indexedAgentHost{index: i, result: reader.read(ctx, host)} }()
	}
	results := make([]agentHostRead, len(hosts))
	// Every adapter is context-bound; join callers before returning or pruning their cache.
	for range hosts {
		result := <-channel
		results[result.index] = result.result
	}
	return results
}

func pruneAgentContext(ctx context.Context, reader agentHostReader, hosts []HostRecord) error {
	pins := make(map[string]string, len(hosts))
	for _, host := range hosts {
		pins[host.ID] = host.MeshIdentity
	}
	if err := hostprofile.OpenContextCache(reader.state).Prune(ctx, pins); err != nil {
		return fmt.Errorf("prune passive context cache: %w", err)
	}
	return nil
}

func (a *application) runAgentInspect(cmd *cobra.Command, target string, cached bool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	reader, err := newAgentHostReader("hosts.inspect", true, cached, a)
	if err != nil {
		return err
	}
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	host, err := resolveAgentHost(ctx, target, hosts, cached, a.dependencies.DialPassive)
	if err != nil {
		return err
	}
	result := reader.read(ctx, host)
	if !result.useful && len(result.errors) == 0 {
		result.errors = append(result.errors, reader.diagnostic(errors.New("No context observation is cached for this machine."), host.ID, agentapi.StateUnavailable))
	}
	envelope := agentapi.Envelope{Status: agentapi.StatusOK, Data: agentapi.HostDetail{Host: result.host, Access: reader.access(result, host)}, Errors: result.errors}
	if len(result.errors) > 0 {
		envelope.Status = agentapi.StatusPartial
	}
	if !result.useful {
		envelope.Status, envelope.Data = agentapi.StatusError, nil
	}
	return a.agent.write(cmd.OutOrStdout(), envelope)
}
