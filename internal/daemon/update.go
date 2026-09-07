package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updatebootstrap"
	"github.com/shaul/mesh/internal/updateinstall"
)

type updateController struct {
	stateDir    string
	id          string
	coordinator *update.Coordinator
	authority   *update.Authority
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
	return controller, nil
}

func (u *updateController) HandleControl(ctx context.Context, request protocol.Control) (protocol.Control, bool, error) {
	return u.authority.HandleControl(ctx, request)
}

func (u *updateController) handle(ctx context.Context, action string, data json.RawMessage) (any, error) {
	switch action {
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
	if action == "stage" {
		var request updateinstall.Request
		if err = json.Unmarshal(data, &request); err != nil {
			return nil, err
		}
		if request.TargetID != u.id {
			return nil, errors.New("installation identity mismatch")
		}
		if _, err = update.EnsureHelper(ctx, u.stateDir); err != nil {
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
	info := update.Info{Health: observed.Health}
	status, err := updateinstall.Read(u.stateDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return info, err
	}
	if err == nil {
		info.Installation = &status
	}
	return info, nil
}
