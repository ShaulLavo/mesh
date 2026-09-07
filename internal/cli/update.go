package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

type updateOptions struct {
	all, local, yes, json, check bool
	hosts                        []string
	fleet, version, coordinator  string
}

type updateOutput struct{ out, diagnostic io.Writer }

type updateEnvironment struct {
	stateDir    string
	client      update.Caller
	local       update.Host
	coordinator update.Host
}

type updatePreview struct {
	CoordinatorSetup     bool             `json:"coordinatorSetup"`
	CoordinatorBootstrap bool             `json:"coordinatorBootstrap"`
	CoordinatorAdded     bool             `json:"coordinatorAdded"`
	Fleet                update.Fleet     `json:"fleet"`
	Release              release.Manifest `json:"release"`
	ReleaseDigest        string           `json:"releaseDigest"`
	Targets              []update.Target  `json:"targets"`
	FirstFleet           bool             `json:"firstFleet"`
	ClientOnly           bool             `json:"clientOnly"`
	OutsideFleet         []string         `json:"outsideFleet,omitempty"`
}

func (a *application) updateCommand() *cobra.Command {
	options := updateOptions{version: "latest"}
	command := &cobra.Command{
		Use: "update", Short: "Review and update every machine in the configured fleet", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runUpdate(cmd.Context(), options, updateOutput{cmd.OutOrStdout(), cmd.ErrOrStderr()})
		},
	}
	command.Flags().BoolVar(&options.all, "all", false, "update the configured fleet, the default scope")
	command.Flags().BoolVar(&options.local, "local", false, "update this machine only")
	command.Flags().StringArrayVar(&options.hosts, "host", nil, "update an adopted host; may repeat")
	command.Flags().StringVar(&options.fleet, "fleet", "", "use an explicit fleet manifest")
	command.Flags().StringVar(&options.version, "version", "latest", "exact published release tag, or latest")
	command.Flags().BoolVar(&options.yes, "yes", false, "approve the selected scope without prompting")
	command.Flags().BoolVar(&options.check, "check", false, "check published and installed versions without installing")
	command.PersistentFlags().BoolVar(&options.json, "json", false, "write structured results")
	command.PersistentFlags().StringVar(&options.coordinator, "coordinator", "", "adopted host holding the operation; defaults to this machine")
	for _, action := range []string{"status", "retry", "cancel"} {
		command.AddCommand(a.updateOperationCommand(action, &options))
	}
	command.AddCommand(updateTrustCommand())
	return command
}

func (a *application) runUpdatePreview(ctx context.Context) error {
	return a.runUpdate(ctx, updateOptions{version: "latest"}, updateOutput{a.dependencies.Stdout, a.dependencies.Stderr})
}

func validateUpdateOptions(options updateOptions) error {
	if options.local && (options.all || len(options.hosts) > 0 || options.fleet != "") {
		return errors.New("--local cannot be combined with --all, --host, or --fleet")
	}
	if len(options.hosts) > 0 && (options.all || options.fleet != "") {
		return errors.New("--host cannot be combined with --all or --fleet")
	}
	if options.version != "latest" {
		_, err := release.CompareVersions(options.version, options.version)
		return err
	}
	return nil
}

func (a *application) runUpdate(ctx context.Context, options updateOptions, output updateOutput) error {
	if err := validateUpdateOptions(options); err != nil {
		return err
	}
	interactive := a.updateInteractive() && !options.json
	if !options.check && !options.yes && !interactive {
		return errors.New("noninteractive updates require --yes; use --check to inspect availability")
	}
	environment, err := openUpdateEnvironment(options.coordinator)
	if err != nil {
		return err
	}
	if a.dependencies.UpdateCaller != nil {
		environment.client = a.dependencies.UpdateCaller
	}
	fleet, savePath, outside, err := a.updateFleet(options, environment, interactive)
	if err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	manifest, err := a.dependencies.UpdateRelease.Manifest(checkCtx, options.version)
	cancel()
	if err != nil {
		return err
	}
	preview := updatePreview{Fleet: fleet, Release: manifest, ReleaseDigest: manifest.Digest(), FirstFleet: savePath != "", OutsideFleet: outside}
	if options.version == "latest" {
		_ = localUpdateNoticeStore(environment.stateDir).Record(ctx, manifest)
	}
	preview.Targets = inspectUpdateTargets(ctx, environment.client, fleet.Members)
	preview.ClientOnly = options.local && localCoordinatorMissing(preview.Targets, environment.local.ID) && localDaemonAbsent(ctx, environment.stateDir)
	preview, err = previewFirstCoordinator(ctx, environment, preview)
	if err != nil {
		return err
	}
	if options.check {
		return printUpdatePreview(output.out, preview, options.json)
	}
	if !options.json {
		if err := printUpdatePreview(output.diagnostic, preview, false); err != nil {
			return err
		}
	}
	if !options.yes && !a.confirmUpdate(preview, output.diagnostic) {
		return errors.New("update cancelled")
	}
	if savePath != "" {
		if err := update.SaveFleet(savePath, preview.Fleet); err != nil {
			return err
		}
	}
	if preview.ClientOnly {
		return a.runClientOnlyUpdate(ctx, environment, preview, options, output)
	}
	if preview.CoordinatorBootstrap {
		return a.runFirstCoordinator(ctx, environment, preview, options, output)
	}
	var run update.Run
	if err := environment.client.Call(ctx, environment.coordinator, "plan", update.Plan{Fleet: fleet, Manifest: manifest}, &run); err != nil {
		return fmt.Errorf("submit update to coordinator: %w; if this machine has no current daemon, run mesh daemon install or use --local", err)
	}
	return observeUpdate(ctx, environment, run, options.json, output)
}

func openUpdateEnvironment(coordinator string) (updateEnvironment, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return updateEnvironment{}, err
	}
	host, key, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return updateEnvironment{}, err
	}
	environment := updateEnvironment{stateDir: stateDir, client: update.Client{ID: host.ID, Key: key}, local: update.LocalHost(stateDir, host.ID)}
	environment.coordinator = environment.local
	if coordinator == "" || coordinator == localHostAlias {
		return environment, nil
	}
	hosts, err := LoadHosts()
	if err != nil {
		return environment, err
	}
	remote, err := hostWithAlias(hosts, coordinator)
	if err != nil {
		return environment, err
	}
	environment.coordinator = adoptedUpdateHost(remote)
	return environment, environment.coordinator.Validate()
}

func adoptedUpdateHost(host HostRecord) update.Host {
	return update.Host{ID: host.MeshIdentity, Alias: host.Alias, Endpoint: host.Endpoint}
}

func inspectUpdateTargets(ctx context.Context, client update.Caller, hosts []update.Host) []update.Target {
	results := make(chan update.Target, len(hosts))
	limit := make(chan struct{}, 8)
	for _, host := range hosts {
		go inspectUpdateTarget(ctx, client, host, limit, results)
	}
	byID := make(map[string]update.Target, len(hosts))
	for range hosts {
		target := <-results
		byID[target.Host.ID] = target
	}
	targets := make([]update.Target, 0, len(hosts))
	for _, host := range hosts {
		targets = append(targets, byID[host.ID])
	}
	return targets
}

func inspectUpdateTarget(ctx context.Context, client update.Caller, host update.Host, limit chan struct{}, results chan<- update.Target) {
	select {
	case limit <- struct{}{}:
		defer func() { <-limit }()
	case <-ctx.Done():
		results <- update.Target{Host: host, State: update.Offline, Problem: ctx.Err().Error()}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var info update.Info
	err := client.Call(ctx, host, "info", nil, &info)
	target := update.Target{Host: host, State: update.Pending}
	if err != nil {
		target.State, target.Problem = update.Offline, err.Error()
		var remote *update.RemoteError
		if errors.As(err, &remote) {
			target.State = update.Failed
		}
		if legacyUpdateUnavailable(err) {
			target.State, target.Problem = update.Bootstrap, "Legacy bootstrap required. Approval permits the existing same-user session management path to install the updater and enroll this coordinator."
		}
		results <- target
		return
	}
	if info.Health.HostID != host.ID {
		target.State, target.Problem = update.Failed, "host health identity differs from pinned identity"
		results <- target
		return
	}
	target.Build, target.Workers = &info.Health.Build, info.Health.Workers
	results <- target
}

func legacyUpdateUnavailable(err error) bool {
	var remote *update.RemoteError
	return errors.As(err, &remote) && strings.Contains(remote.Problem, "unknown control") && strings.Contains(remote.Problem, "update.control")
}

func localCoordinatorMissing(targets []update.Target, id string) bool {
	for _, target := range targets {
		if target.Host.ID == id {
			return target.State == update.Offline
		}
	}
	return false
}

func localDaemonAbsent(ctx context.Context, stateDir string) bool {
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", filepath.Join(stateDir, "daemon.sock"))
	if err == nil {
		_ = connection.Close()
		return false
	}
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func (a *application) confirmUpdate(preview updatePreview, output io.Writer) bool {
	prompt := "Update these machines? [y/N] "
	if preview.FirstFleet {
		prompt = "Is this your complete intended fleet, and should Mesh save it and update these machines? [y/N] "
	}
	if preview.ClientOnly {
		prompt = "Install the supervised update helper and update this local CLI? [y/N] "
	}
	_, _ = fmt.Fprint(output, prompt)
	answer, err := bufio.NewReader(io.LimitReader(a.dependencies.Stdin, 128)).ReadString('\n')
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes")
}

func (a *application) updateFleet(options updateOptions, environment updateEnvironment, interactive bool) (update.Fleet, string, []string, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return update.Fleet{}, "", nil, err
	}
	if options.local {
		return scopedUpdateFleet("local", []update.Host{environment.local}), "", nil, nil
	}
	if len(options.hosts) > 0 {
		fleet, err := selectedUpdateFleet(options.hosts, hosts, environment.local)
		return fleet, "", nil, err
	}
	path := options.fleet
	if path == "" {
		configPath, err := ConfigPath()
		if err != nil {
			return update.Fleet{}, "", nil, err
		}
		path = filepath.Join(filepath.Dir(configPath), "fleet.json")
	}
	fleet, err := update.ReadFleet(path)
	if err == nil {
		return fleet, "", outsideUpdateFleet(fleet, hosts), nil
	}
	if !errors.Is(err, os.ErrNotExist) || options.fleet != "" {
		return update.Fleet{}, "", nil, err
	}
	if !interactive || options.yes {
		return update.Fleet{}, "", nil, errors.New("no saved fleet: provide --fleet FILE, --local, or --host; --yes cannot assume adopted hosts are the complete fleet")
	}
	members := []update.Host{environment.local}
	seen := map[string]bool{environment.local.ID: true}
	for _, host := range hosts {
		member := adoptedUpdateHost(host)
		if seen[member.ID] {
			continue
		}
		members = append(members, member)
		seen[member.ID] = true
	}
	fleet = scopedUpdateFleet("default", members)
	return fleet, path, nil, fleet.Validate()
}

func scopedUpdateFleet(name string, members []update.Host) update.Fleet {
	return update.Fleet{Version: 1, Name: name, Revision: 1, Members: members}
}

func selectedUpdateFleet(aliases []string, hosts []HostRecord, local update.Host) (update.Fleet, error) {
	members := make([]update.Host, 0, len(aliases))
	seen := make(map[string]bool)
	for _, alias := range aliases {
		member, err := selectedUpdateHost(alias, hosts, local)
		if err != nil {
			return update.Fleet{}, err
		}
		if seen[member.ID] {
			continue
		}
		seen[member.ID] = true
		members = append(members, member)
	}
	fleet := scopedUpdateFleet("selected", members)
	return fleet, fleet.Validate()
}

func selectedUpdateHost(alias string, hosts []HostRecord, local update.Host) (update.Host, error) {
	if alias == local.Alias || alias == "local" {
		return local, nil
	}
	host, err := hostWithAlias(hosts, alias)
	return adoptedUpdateHost(host), err
}

func outsideUpdateFleet(fleet update.Fleet, hosts []HostRecord) []string {
	known := make(map[string]bool)
	for _, host := range fleet.Members {
		known[host.ID] = true
	}
	var outside []string
	for _, host := range hosts {
		if !known[host.MeshIdentity] {
			outside = append(outside, host.Alias)
		}
	}
	return outside
}
