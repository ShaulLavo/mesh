package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

const (
	helperLaunchd = "launchd"
	helperSystemd = "systemd"
)

func versionCommand() *cobra.Command {
	var structured bool
	command := &cobra.Command{Use: "version", Short: "Show the executing Mesh build", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			build := release.Metadata()
			if structured {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(release.Current())
			}
			version := build.Version
			if version == "" {
				version = "development"
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "mesh %s %s %s\n", version, build.Commit, build.Platform.String())
			return err
		},
	}
	command.Flags().BoolVar(&structured, "json", false, "write build metadata as JSON")
	return command
}

func updateTrustCommand() *cobra.Command {
	var ifEmpty bool
	command := &cobra.Command{Use: "trust ID", Short: "Authorize an update administrator on this machine", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stateDir, err := paths.StateDir()
			if err != nil {
				return err
			}
			return update.Trust(stateDir, args[0], ifEmpty)
		},
	}
	command.Flags().BoolVar(&ifEmpty, "if-empty", false, "enroll only when no administrator policy exists")
	return command
}

func updateHelperCommand() *cobra.Command {
	var stateDir string
	var checkJournal bool
	command := &cobra.Command{Use: "update-helper", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !filepath.IsAbs(stateDir) {
				return errors.New("update helper requires an absolute --state-dir")
			}
			if checkJournal {
				return checkUpdateJournal(stateDir)
			}
			return runUpdateHelper(cmd.Context(), stateDir, cmd.ErrOrStderr())
		},
	}
	command.Flags().StringVar(&stateDir, "state-dir", "", "Mesh state directory")
	command.Flags().BoolVar(&checkJournal, "check-journal", false, "validate journal compatibility without performing work")
	command.AddCommand(helperRecoveryCommand())
	return command
}

func checkUpdateJournal(stateDir string) error {
	settings, err := updateinstall.ReadSettings(stateDir)
	if err != nil {
		return err
	}
	if settings.StateDir != stateDir {
		return errors.New("installation journal belongs to another state directory")
	}
	config := settings.Config()
	config.Probe = updatebootstrap.Probe(stateDir)
	_, err = updateinstall.New(config)
	return err
}

func runUpdateHelper(ctx context.Context, stateDir string, diagnostic io.Writer) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastProblem := ""
	for ctx.Err() == nil {
		err := updateHelperStep(ctx, stateDir)
		if err != nil && err.Error() != lastProblem && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, updateinstall.ErrNoGrant) {
			lastProblem = err.Error()
			_, _ = fmt.Fprintf(diagnostic, "mesh update helper: %s\n", SafeTerminalText(lastProblem))
		}
		if err == nil {
			lastProblem = ""
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	return nil
}

func updateHelperStep(ctx context.Context, stateDir string) error {
	if handled, err := resumePendingCoordinatorSetup(ctx, stateDir); handled || err != nil {
		return err
	}
	previous, err := updateinstall.Read(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return resumeMissingCoordinatorInstallation(ctx, stateDir)
	}
	if err != nil {
		return err
	}
	settings := previous.Settings
	if settings.StateDir != stateDir {
		return errors.New("installation journal belongs to another state directory")
	}
	if request, ok := pendingCoordinatorRetry(previous); ok {
		return resumeApprovedCoordinatorInstallation(ctx, stateDir, request)
	}
	config := settings.Config()
	config.Probe = updatebootstrap.Probe(stateDir)
	engine, err := updateinstall.New(config)
	if err != nil {
		return err
	}
	status, err := engine.Run(ctx)
	if errors.Is(err, updateinstall.ErrNoGrant) && status.Settings.ClientOnly {
		status, err = grantApprovedClientOnlyUpdate(ctx, engine, status)
		if err == nil {
			status, err = engine.Run(ctx)
		}
	}
	if errors.Is(err, updateinstall.ErrNoGrant) && !status.Settings.ClientOnly {
		status, err = grantFirstCoordinator(ctx, engine, status)
		if err == nil {
			status, err = engine.Run(ctx)
		}
	}
	if err != nil || status.Phase != updateinstall.Committed {
		return err
	}
	kind, domain := helperUpgradeService(status.Settings)
	_, err = updateinstall.UpgradeHelper(ctx, updateinstall.HelperConfig{StateDir: stateDir, Executable: status.Settings.Executable, Kind: kind, Domain: domain})
	return err
}

func helperUpgradeService(settings updateinstall.Settings) (string, string) {
	if !settings.ClientOnly {
		return settings.Service.Kind, settings.Service.Domain
	}
	if runtime.GOOS == pickerDarwinOS {
		return helperLaunchd, fmt.Sprintf("gui/%d", os.Getuid())
	}
	return helperSystemd, ""
}

func (a *application) runClientOnlyUpdate(ctx context.Context, environment updateEnvironment, preview updatePreview, options updateOptions, output updateOutput) error {
	current := release.Current()
	comparison, err := release.CompareVersions(current.Version, preview.Release.Version)
	if err != nil {
		return err
	}
	if comparison >= 0 {
		if err := verifyCurrentLocalRelease(current, preview.Release, comparison); err != nil {
			return err
		}
		state := update.Updated
		if comparison > 0 {
			state = update.Newer
		}
		run := update.Run{Fleet: preview.Fleet, Release: preview.Release, Targets: []update.Target{{Host: environment.local, State: state, Build: &current}}}
		return printDeclaredUpdateRun(ctx, output.out, run, options.json, options.details, output.privacy)
	}
	if err := preview.Release.Allows(current); err != nil {
		return err
	}
	if _, err := update.EnsureHelper(ctx, environment.stateDir); err != nil {
		return fmt.Errorf("install supervised update helper: %w", err)
	}
	engine, err := update.NewInstallation(environment.stateDir, true, nil)
	if err != nil {
		return err
	}
	request, err := clientOnlyUpdateRequest(environment, preview.Release)
	if err != nil {
		return err
	}
	if err := saveClientOnlyApproval(environment.stateDir, request); err != nil {
		return err
	}
	status, err := engine.Stage(ctx, request)
	if err != nil {
		return err
	}
	if status.Phase == updateinstall.Staged {
		status, err = grantClientOnlyUpdate(ctx, engine, status)
		if err != nil {
			return err
		}
	}
	return observeClientOnlyUpdate(ctx, environment.stateDir, status, options.json, output)
}

func verifyCurrentLocalRelease(current release.Build, manifest release.Manifest, comparison int) error {
	artifact, err := manifest.Artifact(current.Platform)
	if err != nil {
		return err
	}
	if current.Modified {
		return errors.New("modified local build requires intervention")
	}
	if comparison == 0 && current.Digest != artifact.BinarySHA256 {
		return errors.New("local executable differs from the published release at the same version")
	}
	compatibility := manifest.Compatibility
	if current.UpdateProtocol != release.CurrentUpdateProtocol || current.StateVersion < compatibility.StateReadMin || current.StateVersion > compatibility.StateReadMax || current.WorkerProtocol < compatibility.WorkerMin || current.WorkerProtocol > compatibility.WorkerMax {
		return errors.New("newer local build has incompatible or unknown update, state, or worker capabilities")
	}
	return nil
}

func clientOnlyUpdateRequest(environment updateEnvironment, manifest release.Manifest) (updateinstall.Request, error) {
	generation := uint64(1)
	previous, err := updateinstall.Read(environment.stateDir)
	if err == nil {
		if !previous.Settings.ClientOnly && !installationFinished(previous.Phase) {
			return updateinstall.Request{}, fmt.Errorf("fleet update %s already manages this installation; use mesh update status or retry", previous.Request.ID)
		}
		if previous.Request.Manifest.Digest() == manifest.Digest() && !installationFinished(previous.Phase) {
			return previous.Request, nil
		}
		generation = previous.Request.Generation + 1
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return updateinstall.Request{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return updateinstall.Request{}, err
	}
	return updateinstall.Request{ID: hex.EncodeToString(id[:]), TargetID: environment.local.ID, Generation: generation, Manifest: manifest, Current: release.Current()}, nil
}

func grantClientOnlyUpdate(ctx context.Context, engine *updateinstall.Engine, status updateinstall.Status) (updateinstall.Status, error) {
	// Grant is durable. The supervised helper alone owns executable activation.
	return engine.Grant(ctx, status.Request.ID, status.Request.Generation)
}

func observeClientOnlyUpdate(ctx context.Context, stateDir string, status updateinstall.Status, structured bool, output updateOutput) error {
	if !structured {
		_, _ = fmt.Fprintf(output.diagnostic, "Local update %s saved; the supervised helper will finish it.\n", output.privacy.Value("update", status.Request.ID))
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for !installationFinished(status.Phase) {
		select {
		case <-ctx.Done():
			if err := printDeclaredUpdateRun(ctx, output.out, runFromInstallation(status), structured, output.details, output.privacy); err != nil {
				return err
			}
			return statusError{code: 2}
		case <-ticker.C:
		}
		current, err := updateinstall.Read(stateDir)
		if err == nil {
			status = current
		}
	}
	run := runFromInstallation(status)
	if err := printDeclaredUpdateRun(ctx, output.out, run, structured, output.details, output.privacy); err != nil {
		return err
	}
	return updateExit(run.ExitCode())
}

func installationFinished(phase updateinstall.Phase) bool {
	return phase == updateinstall.Committed || phase == updateinstall.RolledBack || phase == updateinstall.RollbackFailed || phase == updateinstall.Failed || phase == updateinstall.Cancelled
}

func runFromInstallation(status updateinstall.Status) update.Run {
	target := update.Target{Host: update.LocalHost(status.Settings.StateDir, status.Request.TargetID), Generation: status.Request.Generation, State: update.Pending, Build: &status.Original.Build, Workers: status.Original.Workers, Problem: status.Error}
	switch status.Phase {
	case updateinstall.Staged:
		target.State = update.Staged
	case updateinstall.Granted, updateinstall.Activating, updateinstall.Validating, updateinstall.RollingBack:
		target.State, target.Grant = update.Granted, true
	case updateinstall.Committed:
		target.State = update.Updated
	case updateinstall.Cancelled:
		target.State = update.Cancelled
	case updateinstall.RolledBack, updateinstall.RollbackFailed, updateinstall.Failed:
		target.State = update.Failed
	}
	if status.Verified != nil {
		target.Build, target.Workers = &status.Verified.Build, status.Verified.Workers
	}
	return update.Run{ID: status.Request.ID, Coordinator: status.Request.TargetID, Fleet: scopedUpdateFleet("local", []update.Host{target.Host}), Release: status.Request.Manifest, ReleaseDigest: status.Request.Manifest.Digest(), Targets: []update.Target{target}, UpdatedAt: status.UpdatedAt}
}

func helperRecoveryCommand() *cobra.Command {
	var request updateinstall.HelperRecovery
	var version, phase string
	var structured bool
	command := &cobra.Command{Use: "recover", Short: "Recover the local update helper for a reviewed failed update", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request.Expected.Phase = updateinstall.Phase(phase)
			result, err := runLocalHelperRecovery(cmd.Context(), request, version)
			if err != nil {
				return err
			}
			if structured {
				if err = json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
					return fmt.Errorf("write helper recovery diagnostics: %w", err)
				}
				return nil
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Update helper recovered. Review the next compatible update before approving it.")
			if err != nil {
				return fmt.Errorf("write helper recovery result: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&request.Helper.StateDir, "state-dir", "", "Mesh state directory")
	command.Flags().StringVar(&request.Helper.Executable, "replacement", "", "verified local replacement executable")
	command.Flags().StringVar(&request.Helper.ServiceDir, "service-dir", "", "existing helper service directory")
	command.Flags().StringVar(&version, "version", "", "exact published replacement release")
	command.Flags().StringVar(&request.Digest, "sha256", "", "exact published replacement executable digest")
	command.Flags().StringVar(&request.Expected.Operation, "operation", "", "reviewed installation operation")
	command.Flags().Uint64Var(&request.Expected.Generation, "generation", 0, "reviewed installation generation")
	command.Flags().StringVar(&phase, "phase", "", "reviewed terminal installation phase")
	command.Flags().StringVar(&request.Expected.OriginalDigest, "original-sha256", "", "reviewed original daemon executable digest")
	command.Flags().BoolVar(&structured, "json", false, "write helper process and installation diagnostics")
	return command
}

func runLocalHelperRecovery(ctx context.Context, request updateinstall.HelperRecovery, version string) (updateinstall.HelperRecoveryResult, error) {
	if err := request.Expected.Validate(); err != nil {
		return updateinstall.HelperRecoveryResult{}, fmt.Errorf("review helper recovery journal: %w", err)
	}
	if !filepath.IsAbs(request.Helper.StateDir) || !filepath.IsAbs(request.Helper.Executable) {
		return updateinstall.HelperRecoveryResult{}, errors.New("helper recovery requires absolute state and replacement executable paths")
	}
	if _, err := release.CompareVersions(version, version); err != nil {
		return updateinstall.HelperRecoveryResult{}, fmt.Errorf("review replacement helper release: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	settings, err := updateinstall.ReadSettingsContext(ctx, request.Helper.StateDir)
	if err != nil {
		return updateinstall.HelperRecoveryResult{}, fmt.Errorf("read helper recovery settings: %w", err)
	}
	if settings.StateDir != request.Helper.StateDir {
		return updateinstall.HelperRecoveryResult{}, errors.New("installation journal belongs to another state directory")
	}
	request.Helper.Kind, request.Helper.Domain = settings.Service.Kind, settings.Service.Domain
	request.Helper.ServiceDir, err = helperRecoveryServiceDir(request.Helper)
	if err != nil {
		return updateinstall.HelperRecoveryResult{}, err
	}
	client := release.Client{}
	request.Manifest, err = client.Manifest(ctx, version)
	if err != nil {
		return updateinstall.HelperRecoveryResult{}, fmt.Errorf("read replacement helper release: %w", err)
	}
	config := settings.Config()
	config.Client, config.Probe = client, updatebootstrap.Probe(settings.StateDir)
	engine, err := updateinstall.New(config)
	if err != nil {
		return updateinstall.HelperRecoveryResult{}, fmt.Errorf("open helper recovery installation: %w", err)
	}
	request.Probe, request.CheckIdle = updatebootstrap.HelperProcessProbe(request.Helper), checkHelperRecoveryApprovals
	result, err := engine.RecoverHelper(ctx, request)
	if err != nil {
		return updateinstall.HelperRecoveryResult{}, fmt.Errorf("recover local update helper: %w", err)
	}
	return result, nil
}

func helperRecoveryServiceDir(helper updateinstall.HelperConfig) (string, error) {
	if helper.ServiceDir != "" {
		return helper.ServiceDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate helper service directory: %w", err)
	}
	if helper.Kind == helperLaunchd {
		return filepath.Join(home, "Library", "LaunchAgents"), nil
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func checkHelperRecoveryApprovals(ctx context.Context, status updateinstall.Status) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("check helper recovery approvals: %w", err)
	}
	_, err := os.Stat(filepath.Join(status.Settings.StateDir, "updates", "runs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("locate helper recovery approvals: %w", err)
	}
	store, err := update.OpenStore(status.Settings.StateDir)
	if err != nil {
		return fmt.Errorf("open helper recovery approvals: %w", err)
	}
	runs, err := store.List()
	if err != nil {
		return fmt.Errorf("read helper recovery approvals: %w", err)
	}
	for _, run := range runs {
		for _, target := range run.Targets {
			if target.Host.ID != status.Request.TargetID {
				continue
			}
			if err := helperRecoveryTargetIdle(run, target); err != nil {
				return err
			}
		}
	}
	return nil
}

func helperRecoveryTargetIdle(run update.Run, target update.Target) error {
	if target.Grant || target.BootstrapRetry || run.CoordinatorSetup {
		return errors.New("an update approval is pending; stop helper recovery")
	}
	// The coordinator skips verified targets; their saved retry metadata is inert.
	if target.State == update.Updated || target.State == update.Newer {
		return nil
	}
	if target.RetryPending {
		return errors.New("an update approval is pending; stop helper recovery")
	}
	if target.State != update.Failed && target.State != update.Cancelled {
		return errors.New("an update still owns this machine; stop helper recovery")
	}
	return nil
}
