package updatebootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/shaul/mesh/internal/identity"
	"github.com/shaul/mesh/internal/updateinstall"
)

const installationSystemd = "systemd"
const installationLaunchd = "launchd"

type ServiceSnapshot struct {
	Definition  string            `json:"definition"`
	Arguments   string            `json:"arguments"`
	Environment string            `json:"environment"`
	Files       map[string]string `json:"files"`
}

type Installation struct {
	Observation Observation
	Settings    updateinstall.Settings
	Service     ServiceSnapshot
	ConfigDir   string
	Journal     bool
	PackagePath string
}

type processInvocation struct {
	Arguments   []string
	Environment map[string]string
}

// InspectInstallation binds the connected daemon to its account's actual
// service and invocation. It never serializes environment values or credentials.
func InspectInstallation(ctx context.Context, stateDir string) (Installation, error) {
	return inspectInstallation(ctx, stateDir, nil)
}

func InspectMigrationInstallation(ctx context.Context, stateDir string, migration ManagedMigration) (Installation, error) {
	return inspectInstallation(ctx, stateDir, &migration)
}

func inspectInstallation(ctx context.Context, stateDir string, migration *ManagedMigration) (Installation, error) {
	observed, err := Inspect(ctx, stateDir)
	if err != nil {
		return Installation{}, err
	}
	host, err := identity.LoadOwned(stateDir)
	if err != nil || host.ID != observed.Health.HostID {
		return Installation{}, errors.Join(errors.New("adoption requires the running daemon's own account identity"), err)
	}
	invocation, err := inspectInvocation(ctx, observed.PID)
	if err != nil {
		return Installation{}, err
	}
	settings, configDir, err := invocationSettings(stateDir, observed, invocation)
	if err != nil {
		return Installation{}, err
	}
	snapshot, err := inspectDaemonService(ctx, observed.PID, &settings, invocation)
	if err != nil {
		return Installation{}, err
	}
	settings, journal, err := inspectInstallationJournal(ctx, settings, invocation, migration, snapshot)
	if err != nil {
		return Installation{}, err
	}
	installation := Installation{Observation: observed, Settings: settings, Service: snapshot, ConfigDir: configDir, Journal: journal}
	if updateinstall.ValidateInstallationPath(observed.Executable) != nil {
		installation.PackagePath = observed.Executable
	}
	return installation, nil
}

func inspectInstallationJournal(ctx context.Context, settings updateinstall.Settings, invocation processInvocation, migration *ManagedMigration, snapshot ServiceSnapshot) (updateinstall.Settings, bool, error) {
	status, err := updateinstall.ReadContext(ctx, settings.StateDir)
	if errors.Is(err, os.ErrNotExist) {
		return settings, false, nil
	}
	if err != nil {
		return settings, false, fmt.Errorf("inspect existing installation journal: %w", err)
	}
	prior := status.Settings
	if migration != nil && prior == migration.PriorSettings && settings.Executable == migration.Managed && snapshotsEqual(snapshot, migration.Next) {
		return migration.Settings, true, nil
	}
	if prior.StateDir != settings.StateDir || prior.Executable != settings.Executable || prior.ClientOnly || prior.Service != settings.Service {
		return settings, true, errors.New("running daemon service differs from its journal settings")
	}
	if cache := invocation.Environment["MESH_UPDATE_CACHE_DIR"]; cache != "" && cache != prior.CacheDir {
		return settings, true, errors.New("explicit daemon cache differs from its journal settings")
	}
	if mount := invocation.Environment["MESH_UPDATE_REQUIRED_MOUNT"]; mount != "" && mount != prior.RequiredMount {
		return settings, true, errors.New("explicit daemon required mount differs from its journal settings")
	}
	return prior, true, nil
}

func snapshotsEqual(first, second ServiceSnapshot) bool {
	left, _ := json.Marshal(first)
	right, _ := json.Marshal(second)
	return string(left) == string(right)
}

func invocationSettings(stateDir string, observed Observation, invocation processInvocation) (updateinstall.Settings, string, error) {
	env := invocation.Environment
	home := env["HOME"]
	if !filepath.IsAbs(home) || len(invocation.Arguments) < 2 || invocation.Arguments[1] != "daemon" {
		return updateinstall.Settings{}, "", errors.New("cannot identify the daemon's actual home or invocation")
	}
	state := env["MESH_STATE_DIR"]
	if state == "" {
		state = filepath.Join(environmentDefault(env, "XDG_STATE_HOME", filepath.Join(home, ".local", "state")), "mesh")
	}
	if state != stateDir {
		return updateinstall.Settings{}, "", errors.New("daemon environment selects another state directory")
	}
	cacheDefault := filepath.Join(home, ".cache")
	if runtime.GOOS == "darwin" {
		cacheDefault = filepath.Join(home, "Library", "Caches")
	}
	cache := environmentDefault(env, "MESH_UPDATE_CACHE_DIR", filepath.Join(environmentDefault(env, "XDG_CACHE_HOME", cacheDefault), "mesh", "updates"))
	config := environmentDefault(env, "MESH_CONFIG_DIR", filepath.Join(environmentDefault(env, "XDG_CONFIG_HOME", filepath.Join(home, ".config")), "mesh"))
	if !filepath.IsAbs(cache) || !filepath.IsAbs(config) {
		return updateinstall.Settings{}, "", errors.New("daemon cache and configuration paths must be absolute")
	}
	settings := updateinstall.Settings{StateDir: state, Executable: observed.Executable, CacheDir: cache, RequiredMount: env["MESH_UPDATE_REQUIRED_MOUNT"], HealthTimeout: 30 * time.Second}
	return settings, config, nil
}

func environmentDefault(environment map[string]string, name, fallback string) string {
	if value := environment[name]; value != "" {
		return value
	}
	return fallback
}

func invocationSnapshot(invocation processInvocation) (string, string) {
	args := invocation.Arguments[1:]
	keys := make([]string, 0, len(invocation.Environment))
	for key := range invocation.Environment {
		if !volatileServiceVariable(key) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	values := make([]string, len(keys))
	for i, key := range keys {
		values[i] = key + "=" + invocation.Environment[key]
	}
	arguments, _ := json.Marshal(args)
	environment, _ := json.Marshal(values)
	return installationDigest(arguments), installationDigest(environment)
}

func volatileServiceVariable(key string) bool {
	return key == "INVOCATION_ID" || key == "JOURNAL_STREAM" || key == "MAINPID" || key == "MANAGERPID" || key == "LISTEN_PID"
}

func installationDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func serviceFilesDigest(files map[string]string) string {
	data, _ := json.Marshal(files)
	return installationDigest(data)
}

type inspectionOutput struct{ bytes []byte }

func (output *inspectionOutput) Write(data []byte) (int, error) {
	if len(data) > (1<<20)-len(output.bytes) {
		return 0, errors.New("service inspection exceeds 1 MiB")
	}
	output.bytes = append(output.bytes, data...)
	return len(data), nil
}

func inspectServiceCommand(ctx context.Context, command *exec.Cmd) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("inspect daemon service: %w", err)
	}
	var output inspectionOutput
	command.Stdout = &output
	command.WaitDelay = 100 * time.Millisecond
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("inspect daemon service using %s: %w", filepath.Base(command.Path), err)
	}
	return output.bytes, nil
}

func serviceFileDigest(path string) (string, error) {
	_, digest, err := serviceFileIdentity(path)
	return digest, err
}

func serviceFileIdentity(path string) (string, string, error) {
	if !filepath.IsAbs(path) {
		return "", "", errors.New("daemon service definition requires an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", fmt.Errorf("resolve daemon service definition: %w", err)
	}
	root, err := os.OpenRoot(filepath.Dir(resolved))
	if err != nil {
		return "", "", fmt.Errorf("open daemon service directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(filepath.Base(resolved), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", "", fmt.Errorf("open resolved daemon service definition: %w", err)
	}
	defer func() { _ = file.Close() }()
	before, err := file.Stat()
	if err != nil {
		return "", "", fmt.Errorf("inspect daemon service definition: %w", err)
	}
	if err := validateServiceDefinition(before); err != nil {
		return "", "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return "", "", errors.Join(errors.New("read bounded daemon service definition"), err)
	}
	if err := settleServiceDefinition(path, resolved, file, before); err != nil {
		return "", "", err
	}
	return resolved, installationDigest(data), nil
}

func validateServiceDefinition(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 1<<20 {
		return errors.New("daemon service definition requires a bounded regular file without other writers")
	}
	if stat.Uid != 0 && int64(stat.Uid) != int64(os.Geteuid()) {
		return errors.New("daemon service definition belongs to another account")
	}
	return nil
}

func settleServiceDefinition(path, resolved string, file *os.File, before os.FileInfo) error {
	current, err := filepath.EvalSymlinks(path)
	if err != nil || current != resolved {
		return errors.Join(errors.New("daemon service definition link changed during inspection"), err)
	}
	linked, err := os.Stat(path)
	if err != nil || !os.SameFile(before, linked) {
		return errors.Join(errors.New("daemon service definition identity changed during inspection"), err)
	}
	after, err := file.Stat()
	if err != nil {
		return fmt.Errorf("recheck daemon service definition: %w", err)
	}
	if err := validateServiceDefinition(after); err != nil {
		return err
	}
	priorOwner := before.Sys().(*syscall.Stat_t)
	currentOwner := after.Sys().(*syscall.Stat_t)
	if priorOwner.Uid != currentOwner.Uid || priorOwner.Gid != currentOwner.Gid {
		return errors.New("daemon service definition ownership changed during inspection")
	}
	if after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		return errors.New("daemon service definition changed during inspection")
	}
	return nil
}

func captureServiceFiles(paths []string) (map[string]string, error) {
	files := make(map[string]string, len(paths))
	for _, path := range paths {
		resolved, digest, err := serviceFileIdentity(path)
		if err != nil {
			return nil, err
		}
		files[resolved] = digest
	}
	return files, nil
}

func parseInvocation(arguments, environment []byte) (processInvocation, error) {
	var invocation processInvocation
	invocation.Arguments = strings.Split(strings.TrimRight(string(arguments), "\x00"), "\x00")
	invocation.Environment = make(map[string]string)
	for _, value := range strings.Split(strings.TrimRight(string(environment), "\x00"), "\x00") {
		key, content, ok := strings.Cut(value, "=")
		if ok && key != "" {
			invocation.Environment[key] = content
		}
	}
	if len(invocation.Arguments) == 0 || invocation.Arguments[0] == "" {
		return invocation, errors.New("daemon has an empty invocation")
	}
	return invocation, nil
}
