package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/bootstrap"
	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
	installscript "github.com/shaul/mesh/scripts/install"
)

func (a *application) startFirstCoordinatorSetup(ctx context.Context, environment updateEnvironment, store *update.Store, run update.Run, options updateOptions, output updateOutput) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cache, err := update.CacheDir()
	if err != nil {
		return err
	}
	build := release.Current()
	run, err = store.Change(run.ID, func(current *update.Run) error {
		current.CoordinatorSetup = true
		current.SetupExecutable, current.SetupBuild = executable, &build
		current.SetupCacheDir, current.SetupRequiredMount = cache, os.Getenv("MESH_UPDATE_REQUIRED_MOUNT")
		return nil
	})
	if err != nil {
		return err
	}
	setup := a.dependencies.UpdateCoordinatorSetup
	if setup == nil {
		setup = installCoordinatorSetupHelper
	}
	if err = setup(ctx, environment.stateDir); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if printErr := printUpdateRun(output.out, run, options.json); printErr != nil {
				return printErr
			}
			return statusError{code: 2}
		}
		_, recordErr := store.Change(run.ID, func(current *update.Run) error {
			current.Stopped = true
			current.Problem = "Coordinator setup: " + err.Error()
			return nil
		})
		return errors.Join(err, recordErr)
	}
	return observeUpdate(ctx, environment, run, options.json, output)
}

func coordinatorSetupRun(stateDir string) (*update.Store, update.Run, error) {
	local, err := identity.Load(stateDir)
	if err != nil {
		return nil, update.Run{}, err
	}
	store, err := update.OpenStore(stateDir)
	if err != nil {
		return nil, update.Run{}, err
	}
	runs, err := store.List()
	if err != nil {
		return store, update.Run{}, err
	}
	var selected update.Run
	for _, run := range runs {
		if !run.CoordinatorSetup || !run.CoordinatorBootstrap || run.Coordinator != local.ID || run.Done() || run.Stopped || run.Cancel {
			continue
		}
		index := coordinatorTargetIndex(run, local.ID)
		if index < 0 || !run.Targets[index].Grant || run.Targets[index].Generation == 0 {
			continue
		}
		if selected.ID != "" {
			return store, selected, errors.New("multiple coordinator setup approvals require inspection")
		}
		selected = run
	}
	if selected.ID == "" {
		return store, selected, os.ErrNotExist
	}
	return store, selected, nil
}

func installCoordinatorSetupHelper(ctx context.Context, stateDir string) error {
	_, run, err := coordinatorSetupRun(stateDir)
	if err != nil {
		return err
	}
	if run.Cancel || run.Stopped {
		return errors.New("coordinator setup is stopped")
	}
	if err = validateCoordinatorSetupSource(run); err != nil {
		return err
	}
	if err = update.Trust(stateDir, run.Coordinator, true); err != nil {
		return err
	}
	spec, err := coordinatorSetupService()
	if err != nil {
		return err
	}
	_, err = updateinstall.InstallHelper(ctx, updateinstall.HelperConfig{StateDir: stateDir, Executable: run.SetupExecutable, Kind: spec.Kind, Domain: spec.Domain})
	return err
}

func validateCoordinatorSetupSource(run update.Run) error {
	if run.SetupBuild == nil || !filepath.IsAbs(run.SetupExecutable) || !filepath.IsAbs(run.SetupCacheDir) {
		return errors.New("coordinator setup approval lacks an exact executable and cache path")
	}
	if err := run.Release.Allows(*run.SetupBuild); err != nil {
		return err
	}
	if err := coordinatorSetupMigration(run.SetupExecutable, *run.SetupBuild, run.Release); err != nil {
		return err
	}
	file, err := os.Open(run.SetupExecutable) //nolint:gosec // executable path is persisted in the explicitly approved setup run
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, (128<<20)+1))
	if err != nil {
		return err
	}
	if size > 128<<20 || hex.EncodeToString(hash.Sum(nil)) != run.SetupBuild.Digest {
		return errors.New("coordinator setup executable changed after approval")
	}
	return nil
}

func resumePendingCoordinatorSetup(ctx context.Context, stateDir string) (bool, error) {
	if err := cancelPendingCoordinatorSetups(stateDir); err != nil {
		return false, err
	}
	store, run, err := coordinatorSetupRun(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if run.Cancel {
		_, err = store.Change(run.ID, func(current *update.Run) error {
			index := coordinatorTargetIndex(*current, current.Coordinator)
			current.Targets[index].Grant = false
			current.Targets[index].State = update.Cancelled
			current.CoordinatorSetup = false
			return nil
		})
		return true, err
	}
	if run.Stopped {
		return true, nil
	}
	if err = validateCoordinatorSetupSource(run); err != nil {
		return true, stopCoordinatorSetup(store, run.ID, err)
	}
	if localDaemonAbsent(ctx, stateDir) {
		if err = ensureCoordinatorDaemon(ctx, stateDir, run); err != nil {
			return true, stopCoordinatorSetup(store, run.ID, err)
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var observed updatebootstrap.Observation
	for probeCtx.Err() == nil {
		observed, err = updatebootstrap.Inspect(probeCtx, stateDir)
		if err == nil {
			break
		}
		select {
		case <-probeCtx.Done():
			return true, err
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err != nil {
		return true, err
	}
	return true, advanceCoordinatorSetup(ctx, stateDir, store, run, observed.Health, updatebootstrap.Run)
}

func advanceCoordinatorSetup(ctx context.Context, stateDir string, store *update.Store, run update.Run, health updateinstall.Health, bootstrap func(context.Context, updatebootstrap.Request, updatebootstrap.Config) (updateinstall.Status, error)) error {
	if health.HostID != run.Coordinator {
		return stopCoordinatorSetup(store, run.ID, errors.New("started coordinator identity differs from approval"))
	}
	if err := run.Release.Allows(health.Build); err != nil {
		return stopCoordinatorSetup(store, run.ID, err)
	}
	artifact, artifactErr := run.Release.Artifact(health.Build.Platform)
	if artifactErr != nil {
		return stopCoordinatorSetup(store, run.ID, artifactErr)
	}
	if health.Build.Digest != artifact.BinarySHA256 {
		index := coordinatorTargetIndex(run, run.Coordinator)
		target := run.Targets[index]
		request := updatebootstrap.Request{ID: run.ID, TargetID: run.Coordinator, CoordinatorID: run.Coordinator, Generation: target.Generation, RetryToken: target.BootstrapRetryToken, Manifest: run.Release}
		status, err := bootstrap(ctx, request, updatebootstrap.Config{StateDir: stateDir, CacheDir: run.SetupCacheDir, RequiredMount: run.SetupRequiredMount, Enroll: func(stateDir, id string) error { return update.Trust(stateDir, id, true) }})
		if err != nil {
			return stopCoordinatorSetup(store, run.ID, err)
		}
		if status.Request.ID != run.ID || status.Request.TargetID != run.Coordinator || status.Request.Generation != target.Generation || status.Request.Manifest.Digest() != run.ReleaseDigest {
			return stopCoordinatorSetup(store, run.ID, errors.New("coordinator setup bootstrap returned an unrelated installation receipt"))
		}
	}
	return finishCoordinatorSetup(store, run, health)
}

func cancelPendingCoordinatorSetups(stateDir string) error {
	store, err := update.OpenStore(stateDir)
	if err != nil {
		return err
	}
	runs, err := store.List()
	if err != nil {
		return err
	}
	for _, run := range runs {
		if !run.CoordinatorSetup || !run.Cancel {
			continue
		}
		_, err = store.Change(run.ID, func(current *update.Run) error {
			index := coordinatorTargetIndex(*current, current.Coordinator)
			if index >= 0 {
				current.Targets[index].Grant = false
				current.Targets[index].State = update.Cancelled
			}
			current.CoordinatorSetup = false
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func coordinatorSetupMigration(executable string, build release.Build, manifest release.Manifest) error {
	artifact, err := manifest.Artifact(build.Platform)
	if err != nil {
		return err
	}
	if build.Digest == artifact.BinarySHA256 {
		return nil
	}
	if err := updateinstall.ValidateInstallationPath(executable); err != nil {
		return fmt.Errorf("one-time installation migration required before coordinator setup: %w; install the approved release into ~/.local/bin/mesh with the official Mesh installer, then review and point any existing daemon service at that Mesh-managed executable", err)
	}
	return nil
}

func finishCoordinatorSetup(store *update.Store, run update.Run, health updateinstall.Health) error {
	if health.HostID != run.Coordinator {
		return stopCoordinatorSetup(store, run.ID, errors.New("started coordinator identity differs from approval"))
	}
	if err := run.Release.Allows(health.Build); err != nil {
		return stopCoordinatorSetup(store, run.ID, err)
	}
	artifact, err := run.Release.Artifact(health.Build.Platform)
	if err != nil {
		return err
	}
	_, err = store.Change(run.ID, func(current *update.Run) error {
		if current.Cancel || current.Stopped {
			return nil
		}
		current.CoordinatorSetup = false
		target := &current.Targets[coordinatorTargetIndex(*current, current.Coordinator)]
		target.Build, target.Workers = &health.Build, health.Workers
		if health.Build.Digest == artifact.BinarySHA256 {
			target.State, target.Grant, target.Problem = update.Updated, false, ""
		}
		return nil
	})
	return err
}

func stopCoordinatorSetup(store *update.Store, id string, cause error) error {
	_, err := store.Change(id, func(run *update.Run) error {
		run.Stopped = true
		run.Problem = "Coordinator setup: " + cause.Error()
		return nil
	})
	return errors.Join(cause, err)
}

func coordinatorSetupService() (updateinstall.ServiceSpec, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return updateinstall.ServiceSpec{}, err
	}
	if runtime.GOOS == "linux" {
		return updateinstall.ServiceSpec{Kind: "systemd", Name: "mesh.service", ConfigPath: filepath.Join(home, ".config", "systemd", "user", "mesh.service")}, nil
	}
	if runtime.GOOS != "darwin" {
		return updateinstall.ServiceSpec{}, errors.New("unsupported coordinator service platform")
	}
	domain, err := coordinatorLaunchDomain(context.Background())
	if err != nil {
		return updateinstall.ServiceSpec{}, err
	}
	return updateinstall.ServiceSpec{Kind: "launchd", Name: "dev.shaulavo.mesh", Domain: domain, ConfigPath: filepath.Join(home, "Library", "LaunchAgents", "dev.shaulavo.mesh.plist")}, nil
}

func coordinatorLaunchDomain(ctx context.Context) (string, error) {
	domains := []string{fmt.Sprintf("gui/%d", os.Getuid()), fmt.Sprintf("user/%d", os.Getuid())}
	for _, domain := range domains {
		if _, err := coordinatorSetupCommand(ctx, "launchctl", "print", domain+"/dev.shaulavo.mesh"); err == nil {
			return domain, nil
		}
	}
	for _, domain := range domains {
		if _, err := coordinatorSetupCommand(ctx, "launchctl", "print", domain); err == nil {
			return domain, nil
		}
	}
	return "", errors.New("no launchd user domain for coordinator setup")
}

func ensureCoordinatorDaemon(ctx context.Context, stateDir string, run update.Run) error {
	spec, err := coordinatorSetupService()
	if err != nil {
		return err
	}
	if spec.Kind == "systemd" {
		fragment, inspectErr := coordinatorSetupCommand(ctx, "systemctl", "--user", "show", spec.Name, "--property=FragmentPath", "--value")
		if inspectErr != nil {
			return inspectErr
		}
		fragment = strings.TrimSpace(fragment)
		if fragment != "" {
			if !filepath.IsAbs(fragment) {
				return errors.New("service manager returned a relative unit path")
			}
			spec.ConfigPath = fragment
		}
	}
	service, err := renderCoordinatorService(runtime.GOOS, stateDir, run)
	if err != nil {
		return err
	}
	if err = publishCoordinatorService(spec.ConfigPath, []byte(service)); err != nil {
		return err
	}
	if spec.Kind == "systemd" {
		if _, err = coordinatorSetupCommand(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if _, err = coordinatorSetupCommand(ctx, "systemctl", "--user", "enable", spec.Name); err != nil {
			return err
		}
	}
	manager, err := updateinstall.NewSystemService(spec)
	if err != nil {
		return err
	}
	if err = manager.Preflight(ctx); err != nil {
		return err
	}
	return manager.Start(ctx)
}

func renderCoordinatorService(goos, stateDir string, run update.Run) (string, error) {
	if strings.ContainsAny(stateDir+run.SetupExecutable+run.SetupCacheDir+run.SetupRequiredMount, "\x00\r\n") {
		return "", errors.New("invalid coordinator service path")
	}
	service, err := installscript.RenderService(goos, installscript.ServiceOptions{DaemonPort: bootstrap.DefaultPort, SSHPort: bootstrap.DefaultSSHPort, WebSocketPath: bootstrap.DefaultWebSocketPath})
	if err != nil {
		return "", err
	}
	if goos == "linux" {
		service = strings.Replace(service, "ExecStart=%h/.local/bin/mesh ", "ExecStart="+quoteCoordinatorSystemd(run.SetupExecutable)+" ", 1)
		environment := "Environment=" + quoteCoordinatorSystemd("MESH_STATE_DIR="+stateDir) + "\nEnvironment=" + quoteCoordinatorSystemd("MESH_UPDATE_CACHE_DIR="+run.SetupCacheDir) + "\nEnvironment=" + quoteCoordinatorSystemd("MESH_UPDATE_REQUIRED_MOUNT="+run.SetupRequiredMount) + "\n"
		return strings.Replace(service, "[Service]\n", "[Service]\n"+environment, 1), nil
	}
	command := "exec env MESH_STATE_DIR=" + quoteCoordinatorShell(stateDir) + " MESH_UPDATE_CACHE_DIR=" + quoteCoordinatorShell(run.SetupCacheDir) + " MESH_UPDATE_REQUIRED_MOUNT=" + quoteCoordinatorShell(run.SetupRequiredMount) + " " + quoteCoordinatorShell(run.SetupExecutable)
	var encoded bytes.Buffer
	if err = xml.EscapeText(&encoded, []byte(command)); err != nil {
		return "", err
	}
	return strings.Replace(service, "exec &quot;${HOME}/.local/bin/mesh&quot;", encoded.String(), 1), nil
}

func quoteCoordinatorSystemd(value string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%", "$", "$$").Replace(value) + "\""
}
func quoteCoordinatorShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func publishCoordinatorService(path string, data []byte) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".mesh-service-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Chmod(0644); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Link(file.Name(), path); err != nil && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	dir, err := os.Open(filepath.Dir(path)) //nolint:gosec // platform-defined user service directory
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func coordinatorSetupCommand(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed platform service commands and structured arguments
	data, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("coordinator service %s: %w: %s", name, err, strings.TrimSpace(string(data)))
	}
	return string(data), nil
}
