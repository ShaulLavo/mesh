package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shaul/mesh/internal/domainpolicy"
	"os"
	"time"

	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

type updateController struct {
	hostInstallation       string
	domainWriter           update.DomainWriter
	instanceID             string
	configurationWriter    update.ConfigurationWriter
	configurationObservers configurationObservers
	stateDir               string
	id                     string
	coordinator            *update.Coordinator
	authority              *update.Authority
	fleetProfile           *fleetProfileController
	hostUpdater            *update.HostUpdater
}

func executingBuild() *release.Build { build := release.Current(); return &build }

func newUpdateController(ctx context.Context, stateDir, id string, key ed25519.PrivateKey) (*updateController, error) {
	runs, err := update.OpenStore(stateDir)
	if err != nil {
		return nil, hostControllerError(err)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate host installation: %w", err)
	}
	installation := hostDefaultInstallation(ctx, stateDir, executable, release.Current())
	runs = runs.WithHostInstallation(installation)
	cache, err := update.CacheDir()
	if err != nil {
		return nil, hostControllerError(err)
	}
	instanceID, err := update.NewCommandID()
	if err != nil {
		return nil, fmt.Errorf("allocate daemon policy instance: %w", err)
	}
	controller := &updateController{stateDir: stateDir, id: id, instanceID: instanceID, hostInstallation: installation}
	controller.coordinator = &update.Coordinator{ID: id, Store: runs, Remote: update.Client{ID: id, Key: key}, CacheDir: cache}
	controller.authority = &update.Authority{StateDir: stateDir, ID: id, Key: key, Handle: controller.handle}
	controller.coordinator.Trusted = controller.authority.IsAdministrator
	controller.hostUpdater = &update.HostUpdater{Store: runs, ID: id, Current: release.Current,
		Engine: func(ctx context.Context) (*updateinstall.Engine, error) { return hostInstallation(ctx, stateDir) },
		Order:  &update.HostOrder{Store: runs, Client: update.Client{ID: id, Key: key}},
		Source: update.OfficialHostSource(stateDir), Probe: update.LocalProbe(stateDir),
		Helper: func(ctx context.Context, source release.Manifest) error { return hostHelper(ctx, stateDir, source) }}
	return controller, nil
}

func hostDefaultInstallation(ctx context.Context, stateDir, executable string, build release.Build) string {
	kind := updateinstall.InstallationKind(executable, build, true)
	if kind != updateinstall.InstallationManaged {
		return kind
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := updatebootstrap.InspectCurrentDaemonService(ctx, stateDir); err != nil {
		return updateinstall.InstallationUnknown
	}
	return kind
}

func hostInstallation(ctx context.Context, stateDir string) (*updateinstall.Engine, error) {
	status, err := updateinstall.ReadContext(ctx, stateDir)
	if errors.Is(err, os.ErrNotExist) {
		engine, err := update.NewInstallation(stateDir, false, update.LocalProbe(stateDir))
		return engine, hostControllerError(err)
	}
	if err != nil {
		return nil, hostControllerError(err)
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate executing host updater: %w", err)
	}
	if status.Settings.StateDir != stateDir || status.Settings.Executable != executable || status.Settings.ClientOnly {
		return nil, errors.New("host updater journal settings differ from executing installation")
	}
	config := status.Settings.Config()
	config.Probe = update.LocalProbe(stateDir)
	engine, err := updateinstall.New(config)
	return engine, hostControllerError(err)
}

func hostControllerError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("daemon host updater: %w", err)
}

func hostHelper(ctx context.Context, stateDir string, source release.Manifest) error {
	engine, err := hostInstallation(ctx, stateDir)
	if err != nil {
		return err
	}
	// The engine validates installation settings again at acceptance. The helper
	// uses the same preserved service kind and executable, never a new enrollment.
	settings := engine.Settings()
	cfg := updateinstall.HelperConfig{StateDir: stateDir, Executable: settings.Executable, Kind: settings.Service.Kind, Domain: settings.Service.Domain}
	cfg.Probe = updatebootstrap.HelperProcessProbe(cfg)
	cfg.State = func(ctx context.Context) (updateinstall.HelperProcessState, error) {
		return updatebootstrap.HelperServiceState(ctx, cfg)
	}
	return ensureHostHelper(ctx, cfg, source)
}

func ensureHostHelper(ctx context.Context, cfg updateinstall.HelperConfig, source release.Manifest) error {
	status, statusErr := updateinstall.ReadContext(ctx, cfg.StateDir)
	if statusErr != nil && !errors.Is(statusErr, os.ErrNotExist) {
		return hostControllerError(statusErr)
	}
	if errors.Is(statusErr, os.ErrNotExist) {
		_, err := updateinstall.EnsureInstalledHelper(ctx, cfg, source)
		return hostControllerError(err)
	}
	if !finishedHostJournal(status.Phase) {
		return inspectHostHelper(ctx, cfg, source)
	}
	_, err := updateinstall.EnsureCurrentHelper(ctx, cfg, source)
	return hostControllerError(err)
}

func inspectHostHelper(ctx context.Context, cfg updateinstall.HelperConfig, source release.Manifest) error {
	installed, err := updateinstall.InspectHelper(ctx, cfg)
	if err != nil {
		return hostControllerError(err)
	}
	artifact, err := source.Artifact(release.CurrentPlatform())
	if err != nil {
		return hostControllerError(err)
	}
	if installed.Digest != artifact.BinarySHA256 {
		return errors.New("unfinished host transaction requires its current helper image")
	}
	pid, err := cfg.Probe(ctx, installed)
	if err != nil || pid <= 0 {
		return errors.Join(errors.New("host helper is not running"), err)
	}
	confirmed, err := cfg.Probe(ctx, installed)
	if err != nil || confirmed != pid {
		return errors.Join(errors.New("host helper process changed during readiness"), err)
	}
	return nil
}

func finishedHostJournal(phase updateinstall.Phase) bool {
	return phase == updateinstall.Committed || phase == updateinstall.Failed || phase == updateinstall.RolledBack || phase == updateinstall.Cancelled
}

func (u *updateController) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	if request.Type == protocol.TypeUpdateEvidence {
		return u.evidence(ctx, request)
	}
	return u.authority.HandleControl(ctx, request)
}

func (u *updateController) handle(ctx context.Context, actor update.VerifiedActor, action string, data json.RawMessage) (any, error) {
	switch action {
	case "host-policy.show", "host-policy.set", "host-policy.forget":
		return u.hostPolicy(ctx, actor, action, data)
	case update.HostTargetPlanAction, update.HostTargetSubmitAction, update.HostTargetStatusAction, update.HostTargetCancelAction:
		return u.localTarget(ctx, actor, action, data)
	case "configuration.preview", "configuration.approve", "configuration.status", "configuration.retry", "configuration.cancel", "configuration.snapshot", "configuration.put", "configuration.observe":
		return u.configuration(ctx, actor, action, data)
	case hostprofile.ActionFleetPut:
		if u.fleetProfile == nil {
			return nil, errors.New("fleet profile controller unavailable")
		}
		return u.fleetProfile.Put(ctx, data)
	case "info":
		return u.info(ctx)
	case "runs":
		return u.coordinator.Store.List()
	case "plan":
		var plan update.Plan
		if err := json.Unmarshal(data, &plan); err != nil {
			return nil, err
		}
		return u.coordinator.Start(ctx, plan)
	case "status", "retry", "cancel":
		return u.operation(ctx, action, data)
	case "stage", "grant", "install-status", "install-cancel", "install-retry":
		return u.install(ctx, action, data)
	default:
		return nil, fmt.Errorf("unknown update action %q", action)
	}
}

func (u *updateController) operation(ctx context.Context, action string, data json.RawMessage) (any, error) {
	var operation update.Operation
	if err := json.Unmarshal(data, &operation); err != nil {
		return nil, err
	}
	switch action {
	case "retry":
		return u.coordinator.Retry(ctx, operation.ID)
	case "cancel":
		return u.coordinator.Store.Cancel(operation.ID)
	default:
		return u.coordinator.Store.Read(operation.ID)
	}
}

func (u *updateController) install(ctx context.Context, action string, data json.RawMessage) (any, error) {
	if action == "install-status" {
		return updateinstall.Read(u.stateDir)
	}
	engine, err := update.NewInstallation(u.stateDir, false, update.LocalProbe(u.stateDir))
	if err != nil {
		return nil, err
	}
	engine = engine.WithAcceptance(u.coordinator.Store.AuthorizeHostAcceptance())
	if action == "stage" {
		var request updateinstall.Request
		if err = json.Unmarshal(data, &request); err != nil {
			return nil, err
		}
		if request.TargetID != u.id {
			return nil, errors.New("installation identity mismatch")
		}
		if _, err = update.EnsureHelper(ctx, u.stateDir); err != nil {
			return nil, fmt.Errorf("ensure installation helper: %w", err)
		}
		return engine.Stage(ctx, request)
	}
	var operation update.Operation
	if err = json.Unmarshal(data, &operation); err != nil {
		return nil, err
	}
	switch action {
	case "grant":
		return engine.Grant(ctx, operation.ID, operation.Generation)
	case "install-retry":
		return engine.RetryWithToken(ctx, operation.ID, operation.Generation, operation.RetryToken)
	default:
		return engine.Cancel(ctx, operation.ID, operation.Generation)
	}
}

func (u *updateController) info(ctx context.Context) (update.Info, error) {
	observed, err := updatebootstrap.Inspect(ctx, u.stateDir)
	if err != nil {
		return update.Info{}, err
	}
	info := update.Info{Domain: &update.DomainObservation{Digest: domainpolicy.Current().Digest(), InstanceID: u.instanceID, ObservedAt: time.Now().UTC()}, Health: observed.Health, AcceptsIdentityFleet: true}
	home, _ := os.UserHomeDir()
	info.Commands = updateinstall.OtherCommands(observed.Executable, updateinstall.CommandSearchDirs(home, os.Getenv("PATH")))
	status, err := updateinstall.Read(u.stateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return info, err
	}
	if err == nil {
		info.Installation = &status
	}
	return info, nil
}
