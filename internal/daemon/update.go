package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/shaul/mesh/internal/hostprofile"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
	"github.com/shaul/mesh/internal/updatenotice"
	"path/filepath"
)

type updateController struct {
	stateDir     string
	id           string
	coordinator  *update.Coordinator
	authority    *update.Authority
	fleetProfile *fleetProfileController
}

func executingBuild() *release.Build { build := release.Current(); return &build }

func newUpdateController(stateDir, id string, key ed25519.PrivateKey) (*updateController, error) {
	runs, err := update.OpenStore(stateDir)
	if err != nil {
		return nil, err
	}
	cache, err := update.CacheDir()
	if err != nil {
		return nil, err
	}
	controller := &updateController{stateDir: stateDir, id: id}
	controller.coordinator = &update.Coordinator{ID: id, Store: runs, Remote: update.Client{ID: id, Key: key}, CacheDir: cache}
	controller.authority = &update.Authority{StateDir: stateDir, ID: id, Key: key, Handle: controller.handle}
	controller.coordinator.Trusted = controller.authority.IsAdministrator
	notices := updatenotice.New(updatenotice.Config{Directory: filepath.Join(stateDir, "update-notice"), Current: release.Metadata()})
	controller.coordinator.CheckedRelease = notices.CheckedRelease
	return controller, nil
}

func (u *updateController) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	return u.authority.HandleControl(ctx, request)
}

func (u *updateController) handle(ctx context.Context, actor update.VerifiedActor, action string, data json.RawMessage) (any, error) {
	switch action {
	case "automatic.status", "automatic.consent", "automatic.change":
		return u.automatic(ctx, actor, action, data)
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
		return u.operation(ctx, actor, action, data)
	case "stage", "grant", "install-status", "install-cancel", "install-retry":
		return u.install(ctx, actor, action, data)
	default:
		return nil, fmt.Errorf("unknown update action %q", action)
	}
}

func (u *updateController) operation(ctx context.Context, actor update.VerifiedActor, action string, data json.RawMessage) (any, error) {
	var operation update.Operation
	if err := json.Unmarshal(data, &operation); err != nil {
		return nil, err
	}
	run, err := u.coordinator.Store.Read(operation.ID)
	if err != nil {
		return nil, fmt.Errorf("read update operation: %w", err)
	}
	if run.Origin != nil && action != "status" {
		return u.automaticOperation(ctx, actor, action, operation)
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

func (u *updateController) install(ctx context.Context, actor update.VerifiedActor, action string, data json.RawMessage) (any, error) {
	if action == "install-status" {
		return updateinstall.Read(u.stateDir)
	}
	engine, err := update.NewInstallation(u.stateDir, false, update.LocalProbe(u.stateDir))
	if err != nil {
		return nil, err
	}
	engine = engine.WithAcceptance(u.coordinator.Store.AuthorizeAcceptance(actor, u.authority.IsAdministrator))
	if action == "stage" {
		var request updateinstall.Request
		if err = json.Unmarshal(data, &request); err != nil {
			return nil, err
		}
		if request.TargetID != u.id {
			return nil, errors.New("installation identity mismatch")
		}
		if err = u.ensureInstallationHelper(ctx, engine, request); err != nil {
			return nil, err
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
	info := update.Info{Health: observed.Health, AcceptsIdentityFleet: true}
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

func (u *updateController) automaticOperation(ctx context.Context, actor update.VerifiedActor, action string, operation update.Operation) (any, error) {
	if err := update.RequireAutomaticActor(ctx, actor); err != nil {
		return nil, fmt.Errorf("automatic operation actor: %w", err)
	}
	if operation.ExpectedPolicyRevision == nil {
		return nil, errors.New("automatic operation requires --expected-policy-revision")
	}
	command := update.AutomaticCommand{ID: operation.CommandID, Action: action, RunID: operation.ID, ExpectedRevision: *operation.ExpectedPolicyRevision}
	status, err := u.coordinator.ChangeAutomatic(ctx, actor, command)
	if err != nil {
		return nil, fmt.Errorf("change automatic operation: %w", err)
	}
	if status.Record == nil || status.Record.Current == nil {
		return nil, errors.New("automatic run no longer current")
	}
	return *status.Record.Current, nil
}
func (u *updateController) automatic(ctx context.Context, actor update.VerifiedActor, action string, data json.RawMessage) (any, error) {
	var result any
	var err error
	switch action {
	case "automatic.status":
		result, err = u.coordinator.AutomaticStatus(ctx)
	case "automatic.consent":
		result, err = u.coordinator.Store.RecipientConsent(ctx)
	case "automatic.change":
		var command update.AutomaticCommand
		if err = update.DecodeAutomatic(data, &command); err == nil {
			result, err = u.coordinator.ChangeAutomatic(ctx, actor, command)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("automatic update control: %w", err)
	}
	return result, nil
}

func (u *updateController) ensureInstallationHelper(ctx context.Context, engine *updateinstall.Engine, request updateinstall.Request) error {
	if request.Automatic != nil {
		if err := engine.RequireAutomaticHelper(ctx); err != nil {
			return fmt.Errorf("automatic helper prerequisite: %w", err)
		}
		return nil
	}
	if _, err := update.EnsureHelper(ctx, u.stateDir); err != nil {
		return fmt.Errorf("ensure installation helper: %w", err)
	}
	return nil
}
