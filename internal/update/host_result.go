package update

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/shaul/mesh/internal/updateinstall"
)

// PublishHostResult settles the helper's durable terminal journal before helper
// promotion can restart it. Discovery remains with the daemon's periodic loop.
func PublishHostResult(ctx context.Context, stateDir string) error {
	store, err := OpenStore(stateDir)
	if err != nil {
		return err
	}
	unlock, err := store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := store.hostUpdate()
	if err != nil || state.Owned == nil {
		return err
	}
	journal, err := updateinstall.ReadContext(ctx, stateDir)
	if err != nil {
		return fmt.Errorf("read completed host installation: %w", err)
	}
	if journal.Phase != updateinstall.Committed && journal.Phase != updateinstall.RolledBack {
		return nil
	}
	if err := state.Owned.matches(journal.Request); err != nil {
		return err
	}
	if state.Owned.Settings != journal.Settings {
		return errors.New("update: completed installation settings differ from owned request")
	}
	u := &HostUpdater{Store: store, ID: journal.Request.TargetID}
	_, _, err = u.recordFinalLocked(state, journal)
	return err
}

func sameHostFinalization(a, b HostUpdate) bool {
	return a.LastRequest == b.LastRequest && a.LastGeneration == b.LastGeneration && a.LastPhase == b.LastPhase && reflect.DeepEqual(a.Owned, b.Owned)
}
