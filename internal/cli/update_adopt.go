package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
)

type adoptOptions struct {
	version, fleet, last string
	ssh, canaries        []string
	local, notice        bool
	lead                 time.Duration
}

const adoptionCommandName = "adopt"

func updateAdoptCommand(options *updateOptions) *cobra.Command {
	command := &cobra.Command{Use: adoptionCommandName, Short: "Bring an existing installation into automatic updates", Args: cobra.NoArgs}
	var start adoptOptions
	begin := &cobra.Command{Use: "start [--notice -- COMMAND ARG...]", Short: "Start one durable adoption from a verified release executable",
		RunE: func(cmd *cobra.Command, args []string) error { return runAdoptionStart(cmd, start, args, options.json) },
	}
	begin.Flags().StringVar(&start.version, "release", "", "exact signed activation release")
	begin.Flags().StringVar(&start.fleet, "fleet", "", "fleet manifest to adopt")
	begin.Flags().StringArrayVar(&start.ssh, "ssh", nil, "system SSH alias as ID=ALIAS; may repeat")
	begin.Flags().StringArrayVar(&start.canaries, "canary", nil, "host identity that adopts first; may repeat")
	begin.Flags().StringVar(&start.last, "last", "LOCAL", "local host identity that adopts last")
	begin.Flags().BoolVar(&start.local, "local", false, "receive this host's pinned adoption input on stdin")
	begin.Flags().BoolVar(&start.notice, "notice", false, "run the argv after -- before the last host activates")
	begin.Flags().DurationVar(&start.lead, "notice-lead", 30*time.Minute, "time between successful notice and local activation")
	status := &cobra.Command{Use: adoptionStatusAction, Short: "Read this host's durable adoption progress", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeAdoptionStatus(cmd.Context(), cmd.OutOrStdout(), options.json)
		},
	}
	command.AddCommand(begin, status)
	return command
}

func runAdoptionStart(cmd *cobra.Command, options adoptOptions, args []string, structured bool) error {
	if options.version == "" || options.lead < 0 {
		return errors.New("adoption requires an exact --release and nonnegative --notice-lead")
	}
	stateDir, err := paths.StateDirPath()
	if err != nil {
		return adoptionError(err)
	}
	start, err := adoptionStartInput(cmd, stateDir, options, args)
	if err != nil {
		return err
	}
	files, err := adoptionPreservedFiles(stateDir)
	if err != nil {
		return err
	}
	start.Files = files
	supervisor, err := localAdoptionSupervisor(cmd.Context(), stateDir)
	if err != nil {
		return err
	}
	if err := supervisor.Start(cmd.Context(), start); err != nil {
		return adoptionError(err)
	}
	return writeAdoptionStatus(cmd.Context(), cmd.OutOrStdout(), structured)
}

func adoptionStartInput(cmd *cobra.Command, stateDir string, options adoptOptions, args []string) (update.AdoptionStart, error) {
	if options.local {
		return localAdoptionInput(cmd.InOrStdin(), options, args)
	}
	if !options.notice || len(args) == 0 || cmd.ArgsLenAtDash() != 0 {
		return update.AdoptionStart{}, errors.New("fleet adoption requires --notice -- COMMAND ARG...")
	}
	if _, err := exec.LookPath(args[0]); err != nil {
		return update.AdoptionStart{}, fmt.Errorf("locate adoption notice command: %w", err)
	}
	return fleetAdoptionInput(stateDir, options, args)
}

func localAdoptionInput(reader io.Reader, options adoptOptions, args []string) (update.AdoptionStart, error) {
	if options.fleet != "" || len(options.ssh) != 0 || len(options.canaries) != 0 || options.notice || len(args) != 0 {
		return update.AdoptionStart{}, errors.New("--local accepts only the pinned record on stdin and --release")
	}
	data, err := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if err != nil {
		return update.AdoptionStart{}, fmt.Errorf("read local adoption input: %w", err)
	}
	input, err := update.DecodeAdoptionLocalInput(data)
	if err != nil {
		return update.AdoptionStart{}, adoptionError(err)
	}
	if input.Version != options.version {
		return update.AdoptionStart{}, errors.New("local adoption release differs from input pin")
	}
	return update.AdoptionStart{ID: input.ID, Version: input.Version, Expected: input.Expected, Pin: input.Pin, Policy: input.Policy, Require: input.Require}, nil
}

func fleetAdoptionInput(stateDir string, options adoptOptions, args []string) (update.AdoptionStart, error) {
	host, err := identity.LoadOwned(stateDir)
	if err != nil {
		return update.AdoptionStart{}, adoptionError(err)
	}
	last := options.last
	if last == "LOCAL" {
		last = host.ID
	}
	if last != host.ID {
		return update.AdoptionStart{}, errors.New("fleet adoption must start in the last daemon's own account")
	}
	path := options.fleet
	if path == "" {
		config, err := ConfigPath()
		if err != nil {
			return update.AdoptionStart{}, err
		}
		path = filepath.Join(filepath.Dir(config), "fleet.json")
	}
	fleet, err := update.ReadFleet(path)
	if err != nil {
		return update.AdoptionStart{}, adoptionError(err)
	}
	order, err := update.AdoptionOrder(fleet, options.canaries, last)
	if err != nil {
		return update.AdoptionStart{}, adoptionError(err)
	}
	aliases, err := adoptionSSHAliases(options.ssh)
	if err != nil {
		return update.AdoptionStart{}, err
	}
	remotes, err := adoptionRemotes(fleet, order, aliases, last)
	if err != nil {
		return update.AdoptionStart{}, err
	}
	return update.AdoptionStart{Version: options.version, Expected: last, Policy: update.HostPolicy{Mode: update.HostAutomatic, After: options.canaries}, Require: order[last], Remotes: remotes, Notice: &update.AdoptionNotice{Command: args, Lead: options.lead}}, nil
}

func adoptionSSHAliases(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		id, alias, ok := strings.Cut(value, "=")
		if !ok || alias == "" || result[id] != "" {
			return nil, errors.New("each --ssh requires a distinct ID=ALIAS")
		}
		if _, err := update.PublicKey(id); err != nil {
			return nil, adoptionError(err)
		}
		result[id] = alias
	}
	return result, nil
}

func adoptionRemotes(fleet update.Fleet, order map[string][]update.Host, aliases map[string]string, local string) ([]update.AdoptionRemote, error) {
	var remotes []update.AdoptionRemote
	for _, host := range fleet.Members {
		if host.ID == local {
			continue
		}
		if aliases[host.ID] == "" {
			return nil, fmt.Errorf("host %s has no configured system --ssh alias", host.ID)
		}
		policy := update.HostPolicy{Schema: 1, Revision: 1, Mode: update.HostAutomatic}
		for _, peer := range order[host.ID] {
			policy.After = append(policy.After, peer.ID)
		}
		remotes = append(remotes, update.AdoptionRemote{Host: host, SSH: aliases[host.ID], Policy: policy, Require: order[host.ID], State: "pending"})
	}
	if len(aliases) != len(remotes) {
		return nil, errors.New("--ssh aliases must name exactly the remote fleet members")
	}
	return remotes, nil
}

func localAdoptionSupervisor(ctx context.Context, stateDir string) (*update.AdoptionSupervisor, error) {
	trust, err := release.RefreshTrust(ctx, filepath.Join(stateDir, "updates", "host-trust"), release.TrustRefreshBudget{Documents: 8, Duration: 30 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("authenticate adoption release trust: %w", err)
	}
	side, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate adoption side executable: %w", err)
	}
	supervisor, err := update.NewLocalAdoptionSupervisor(stateDir, side, update.HostDocuments{Trust: trust})
	if err != nil {
		return nil, adoptionError(err)
	}
	supervisor.Deploy = update.DeployAdoptionRemote
	return supervisor, nil
}

func adoptionPreservedFiles(stateDir string) ([]string, error) {
	config, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(config)
	return []string{config, filepath.Join(directory, "fleet.json"), filepath.Join(directory, "domains.json"), filepath.Join(stateDir, "authorized_keys"), filepath.Join(stateDir, "access", "scopes.json"), filepath.Join(stateDir, "updates", "administrators.json")}, nil
}

func writeAdoptionStatus(ctx context.Context, output io.Writer, structured bool) error {
	stateDir, err := paths.StateDirPath()
	if err != nil {
		return adoptionError(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "updates", "host-adoption.json")); err != nil {
		return fmt.Errorf("read adoption status: %w", err)
	}
	store, err := update.OpenStore(stateDir)
	if err != nil {
		return adoptionError(err)
	}
	envelope, operation, err := store.AdoptionStatus(ctx)
	if err != nil {
		return adoptionError(err)
	}
	if structured {
		if err := json.NewEncoder(output).Encode(envelope); err != nil {
			return fmt.Errorf("write adoption status: %w", err)
		}
		return nil
	}
	_, err = fmt.Fprintf(output, "Adoption %s: %s\n%s\n", SafeTerminalText(operation.ID), SafeTerminalText(string(operation.Phase)), SafeTerminalText(operation.Problem))
	if err != nil {
		return fmt.Errorf("write adoption status: %w", err)
	}
	return nil
}

func resumeAdoption(ctx context.Context, stateDir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(stateDir, "updates", "host-adoption.json")); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return true, fmt.Errorf("locate helper adoption: %w", err)
	}
	store, err := update.OpenStore(stateDir)
	if err != nil {
		return true, adoptionError(err)
	}
	envelope, operation, err := store.AdoptionStatus(ctx)
	if err != nil || envelope.Confirmed {
		return false, adoptionError(err)
	}
	if operation.Phase == update.AdoptionAttention || operation.Phase == update.AdoptionRolledBack || time.Now().Before(operation.NextAttempt) {
		return true, nil
	}
	supervisor, err := localAdoptionSupervisor(ctx, stateDir)
	if err != nil {
		return true, err
	}
	return true, adoptionError(supervisor.Resume(ctx))
}

const adoptionStatusAction = "status"

func adoptionError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("adoption: %w", err)
}
