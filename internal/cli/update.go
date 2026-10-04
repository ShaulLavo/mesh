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
	"github.com/shaul/mesh/internal/privacy"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

type updateOptions struct {
	all, local, yes, json, check, details bool
	hosts                                 []string
	fleet, version, coordinator           string
}

type updateOutput struct {
	out, diagnostic io.Writer
	privacy         *privacy.Mask
	details         bool
}

type updateEnvironment struct {
	stateDir    string
	client      update.Caller
	local       update.Host
	coordinator update.Host
}

type updatePreview struct {
	CoordinatorSetup     bool                          `json:"coordinatorSetup"`
	CoordinatorBootstrap bool                          `json:"coordinatorBootstrap"`
	CoordinatorAdded     bool                          `json:"coordinatorAdded"`
	Fleet                update.Fleet                  `json:"fleet"`
	Release              release.Manifest              `json:"release"`
	ReleaseDigest        string                        `json:"releaseDigest"`
	Targets              []update.Target               `json:"targets"`
	Reviews              map[string]updateTargetReview `json:"reviews,omitempty"`
	ApprovalProblem      string                        `json:"approvalProblem,omitempty"`
	ClientOnly           bool                          `json:"clientOnly"`
	OutsideFleet         []string                      `json:"outsideFleet,omitempty"`
}

func (a *application) updateCommand() *cobra.Command {
	options := updateOptions{version: "latest"}
	command := &cobra.Command{
		Use: "update", Short: "Review and update Mesh on this machine or your saved fleet", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runUpdate(cmd.Context(), options, updateOutput{cmd.OutOrStdout(), cmd.ErrOrStderr(), a.privacy, options.details})
		},
	}
	command.Flags().BoolVar(&options.all, "all", false, "update the machines in your saved fleet")
	command.Flags().BoolVar(&options.local, "local", false, "update this machine only")
	command.Flags().StringArrayVar(&options.hosts, "host", nil, "update a named machine; may repeat")
	command.Flags().StringVar(&options.fleet, "fleet", "", "use an explicit fleet manifest")
	command.Flags().StringVar(&options.version, "version", "latest", "exact published release tag, or latest")
	command.Flags().BoolVar(&options.yes, "yes", false, "approve the selected scope without prompting")
	command.PersistentFlags().BoolVar(&options.details, "details", false, "show release identifiers, selected machines, and update checks")
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
	return a.runUpdate(ctx, updateOptions{version: "latest"}, updateOutput{a.dependencies.Stdout, a.dependencies.Stderr, a.privacy, false})
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
	options, intents, err := a.resolveUpdateNameIntents(ctx, options)
	if err != nil {
		return err
	}
	environment, err := openUpdateEnvironment(ctx, options.coordinator)
	if err != nil {
		return err
	}
	if a.dependencies.UpdateCaller != nil {
		environment.client = a.dependencies.UpdateCaller
	}
	fleet, outside, err := a.updateFleet(options, environment, interactive)
	if err != nil {
		return err
	}
	manifest, err := a.dependencies.UpdateRelease.Manifest(ctx, options.version)
	if err != nil {
		return err
	}
	preview := updatePreview{Fleet: fleet, Release: manifest, ReleaseDigest: manifest.Digest(), OutsideFleet: outside}
	if options.version == "latest" {
		_ = localUpdateNoticeStore(environment.stateDir).Record(ctx, manifest)
	}
	preview.Targets, preview.Reviews = inspectUpdateTargets(ctx, environment.client, fleet.Members)
	localScope := options.local || (!options.all && options.fleet == "" && len(options.hosts) == 0 && fleet.Name == "local" && len(fleet.Members) == 1 && fleet.Members[0].ID == environment.local.ID)
	preview.ClientOnly = localScope && localCoordinatorMissing(preview.Targets, environment.local.ID) && localDaemonAbsent(ctx, environment.stateDir)
	preview = a.reviewLocalUpdateBuild(environment, preview)
	preview, err = previewFirstCoordinator(ctx, a, environment, preview)
	if err != nil {
		return err
	}
	preview = reviewLocalUpdateJournal(preview, environment.stateDir, environment.local)
	preview = prepareUpdateApproval(preview)
	if !options.json {
		preview.Targets, err = declaredUpdateTargets(ctx, preview.Targets)
		if err != nil {
			return err
		}
	}
	if options.check {
		return printUpdatePreview(output.out, preview, options.json, options.details, output.privacy)
	}
	if !options.json {
		if err := printUpdatePreview(output.diagnostic, preview, false, options.details, output.privacy); err != nil {
			return err
		}
	}
	if preview.ApprovalProblem != "" {
		if options.json {
			if err := printUpdatePreview(output.out, preview, true, options.details, output.privacy); err != nil {
				return err
			}
		}
		return errors.New(preview.ApprovalProblem)
	}
	if updatePreviewCurrent(preview) {
		if options.json {
			return printUpdatePreview(output.out, preview, true, options.details, output.privacy)
		}
		return nil
	}
	if !options.yes && !a.confirmUpdate(preview, output.diagnostic) {
		return errors.New("update cancelled")
	}
	if err := verifyUpdateNameIntents(ctx, intents, a.dependencies.DialControl); err != nil {
		return err
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

func openUpdateEnvironment(ctx context.Context, coordinator string) (updateEnvironment, error) {
	stateDir, err := paths.StateDir()
	if err != nil {
		return updateEnvironment{}, err
	}
	host, key, err := identity.LoadOrCreate(stateDir)
	if err != nil {
		return updateEnvironment{}, err
	}
	environment := updateEnvironment{stateDir: stateDir, client: update.Client{ID: host.ID, Key: key}, local: update.LocalHost(stateDir, host.ID)}
	if declared, readErr := localNameRecord(ctx, stateDir); readErr == nil {
		environment.local.MachineName = declared.MachineName
	}
	environment.coordinator = environment.local
	if coordinator == "" || coordinator == localHostID() {
		return environment, nil
	}
	hosts, err := LoadHosts()
	if err != nil {
		return environment, err
	}
	remote, err := resolveHostTarget(hosts, coordinator)
	if err != nil {
		return environment, err
	}
	environment.coordinator = adoptedUpdateHost(remote)
	return environment, environment.coordinator.Validate()
}

func adoptedUpdateHost(host HostRecord) update.Host {
	return update.Host{ID: host.MeshIdentity, MachineName: host.MachineName, Endpoint: host.Endpoint}
}

type updateInspection struct {
	target update.Target
	review updateTargetReview
}

func inspectUpdateTargets(ctx context.Context, client update.Caller, hosts []update.Host) ([]update.Target, map[string]updateTargetReview) {
	results := make(chan updateInspection, len(hosts))
	limit := make(chan struct{}, 8)
	for _, host := range hosts {
		go inspectUpdateTarget(ctx, client, host, limit, results)
	}
	byID := make(map[string]update.Target, len(hosts))
	reviews := make(map[string]updateTargetReview)
	for range hosts {
		result := <-results
		byID[result.target.Host.ID] = result.target
		if result.review.Kind != "" {
			reviews[result.target.Host.ID] = result.review
		}
	}
	targets := make([]update.Target, 0, len(hosts))
	for _, host := range hosts {
		targets = append(targets, byID[host.ID])
	}
	return targets, reviews
}

func inspectUpdateTarget(ctx context.Context, client update.Caller, host update.Host, limit chan struct{}, results chan<- updateInspection) {
	select {
	case limit <- struct{}{}:
		defer func() { <-limit }()
	case <-ctx.Done():
		results <- updateInspection{target: update.Target{Host: host, State: update.Offline, Problem: ctx.Err().Error()}}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var info update.Info
	err := client.Call(ctx, host, "info", nil, &info)
	target := update.Target{Host: host, State: update.Pending}
	if err != nil {
		target.State, target.Problem = update.Failed, err.Error()
		review := classifyUpdateInspection(err)
		review.Cause = err.Error()
		if review.Kind == "offline" {
			target.State = update.Offline
		}
		if legacyUpdateUnavailable(err) {
			target.State = update.Bootstrap
		}
		results <- updateInspection{target: target, review: review}
		return
	}

	if info.Health.HostID != host.ID {
		target.State, target.Problem = update.Failed, "host health identity differs from pinned identity"
		results <- updateInspection{target: target}
		return
	}
	target.Build, target.Workers = &info.Health.Build, info.Health.Workers
	if info.Installation != nil && info.Installation.Phase == updateinstall.RollbackFailed {
		target.State = update.Failed
		review := updateTargetReview{Kind: updateReviewRecovery, Message: updateRecoveryProblem, Cause: string(info.Installation.Phase) + ": " + info.Installation.Error}
		results <- updateInspection{target: target, review: review}
		return
	}
	results <- updateInspection{target: target}
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
	if len(preview.Fleet.Members) == 1 && update.IsLocal(preview.Fleet.Members[0]) {
		prompt = "Update Mesh on this machine? [y/N] "
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

func (a *application) updateFleet(options updateOptions, environment updateEnvironment, interactive bool) (update.Fleet, []string, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return update.Fleet{}, nil, err
	}
	if options.local {
		return scopedUpdateFleet("local", []update.Host{environment.local}), nil, nil
	}
	if len(options.hosts) > 0 {
		fleet, err := selectedUpdateFleet(options.hosts, hosts, environment.local)
		return fleet, nil, err
	}
	path := options.fleet
	if path == "" {
		configPath, err := ConfigPath()
		if err != nil {
			return update.Fleet{}, nil, err
		}
		path = filepath.Join(filepath.Dir(configPath), "fleet.json")
	}
	fleet, err := update.ReadFleet(path)
	if err == nil {
		return fleet, outsideUpdateFleet(fleet, hosts), nil
	}
	if !errors.Is(err, os.ErrNotExist) || options.fleet != "" {
		return update.Fleet{}, nil, err
	}
	if options.all || options.yes || (!interactive && !options.check) {
		return update.Fleet{}, nil, errors.New("no saved fleet: use --fleet FILE to choose machines, or --local to update this machine")
	}
	return scopedUpdateFleet("local", []update.Host{environment.local}), nil, nil
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
	if alias == local.ID {
		return local, nil
	}
	candidates := append([]HostRecord(nil), hosts...)
	found := false
	for _, host := range candidates {
		if host.ID == local.ID {
			found = true
		}
	}
	if !found {
		candidates = append(candidates, HostRecord{ID: local.ID, MeshIdentity: local.ID, MachineName: local.MachineName, local: true})
	}
	host, err := resolveHostTarget(candidates, alias)
	if err != nil {
		return update.Host{}, err
	}
	if host.ID == local.ID {
		return local, nil
	}
	return adoptedUpdateHost(host), nil
}

func outsideUpdateFleet(fleet update.Fleet, hosts []HostRecord) []string {
	known := make(map[string]bool)
	for _, host := range fleet.Members {
		known[host.ID] = true
	}
	var outside []string
	for _, host := range hosts {
		if !known[host.MeshIdentity] {
			outside = append(outside, HostLabel(host))
		}
	}
	return outside
}

func (a *application) resolveUpdateNameIntents(ctx context.Context, options updateOptions) (updateOptions, []HostRecord, error) {
	if options.coordinator == "" && len(options.hosts) == 0 {
		return options, nil, nil
	}
	hosts, err := machineTargetHosts(ctx)
	if err != nil {
		return options, nil, err
	}
	var intents []HostRecord
	resolve := func(value string) (string, error) {
		host, err := resolveHostTarget(hosts, value)
		if err != nil {
			return "", err
		}
		if host.targetName != "" {
			intents = append(intents, host)
		}
		return host.ID, nil
	}
	if options.coordinator != "" {
		options.coordinator, err = resolve(options.coordinator)
		if err != nil {
			return options, nil, err
		}
	}
	selected := make([]string, len(options.hosts))
	for index, value := range options.hosts {
		selected[index], err = resolve(value)
		if err != nil {
			return options, nil, err
		}
	}
	options.hosts = selected
	if err := verifyUpdateNameIntents(ctx, intents, a.dependencies.DialControl); err != nil {
		return options, nil, err
	}
	return options, intents, nil
}

func verifyUpdateNameIntents(ctx context.Context, hosts []HostRecord, dial HostDialer) error {
	for _, host := range hosts {
		if host.local {
			stateDir, err := paths.StateDir()
			if err != nil {
				return fmt.Errorf("locate updater target state: %w", err)
			}
			owner, err := localNameRecord(ctx, stateDir)
			if err != nil {
				return err
			}
			if owner.ID != host.ID || !owner.NameVerified || owner.MachineName != host.targetName {
				return fmt.Errorf("local machine name changed; use exact host ID %s", host.ID)
			}
			continue
		}
		conn, _, err := openVerifiedHostInfo(ctx, host, dial)
		if err != nil {
			return err
		}
		_ = conn.Close()
	}
	return nil
}
