package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"golang.org/x/sys/unix"

	"github.com/shaul/mesh/internal/updateinstall"
)

// AdoptionAbandonment is the durable record an abandon leaves behind in place of
// host-adoption.json. It keeps the whole envelope so the abandoned operation
// stays inspectable.
type AdoptionAbandonment struct {
	Schema    int           `json:"schema"`
	Operation string        `json:"operation"`
	Pin       string        `json:"pin"`
	Version   string        `json:"version"`
	Phase     AdoptionPhase `json:"phase"`
	At        time.Time     `json:"at"`
	// Policy is the host policy in force after abandon. PolicyWithdrawn says
	// abandon replaced the adoption's automatic grant with manual mode.
	Policy          HostPolicy                        `json:"policy"`
	PolicyWithdrawn bool                              `json:"policyWithdrawn"`
	Helper          *updateinstall.HelperInstallation `json:"helper,omitempty"`
	HelperRestored  bool                              `json:"helperRestored"`
	Adoption        HostAdoption                      `json:"adoption"`
}

// AbandonAdoption withdraws an adoption that has changed nothing outside this
// host's own update records: it never activated the local daemon and no remote
// deploy can have reached SSH. restoreHelper undoes an unjournaled adoption's
// initial helper; a journaled adoption keeps its ordinarily promoted helper,
// which the next adoption promotes forward.
func AbandonAdoption(ctx context.Context, store *Store, restoreHelper func(context.Context, Adoption) (bool, error), now time.Time) (AdoptionAbandonment, error) {
	unlock, err := store.lockAdoptionPass(ctx)
	if err != nil {
		return AdoptionAbandonment{}, fmt.Errorf("update: adoption pass is running; retry abandon: %w", err)
	}
	defer unlock()
	envelope, operation, err := store.AdoptionStatus(ctx)
	if err != nil {
		return AdoptionAbandonment{}, err
	}
	if err := checkAdoptionAbandonable(envelope, operation); err != nil {
		return AdoptionAbandonment{}, err
	}
	// The restore runs before finishAbandon re-checks the record. Every writer
	// that can change a recorded or waiting adoption runs under the pass lock
	// taken above (beginAdoption cannot replace one), so the record cannot change
	// in between. If it somehow did, abandon fails and running it again finishes,
	// because a restored helper has no checkpoint left.
	restored := false
	if operation.Unjournaled {
		if restoreHelper == nil {
			return AdoptionAbandonment{}, errors.New("update: unjournaled adoption requires its initial helper restore")
		}
		restored, err = restoreHelper(ctx, operation)
		if err != nil {
			return AdoptionAbandonment{}, fmt.Errorf("update: restore initial adoption helper: %w", err)
		}
	}
	record := AdoptionAbandonment{Schema: 1, Operation: operation.ID, Pin: operation.Pin, Version: operation.Version, Phase: operation.Phase, At: now.UTC(), HelperRestored: restored, Adoption: envelope}
	if !operation.Unjournaled {
		record.Helper = operation.Helper
	}
	return record, store.finishAbandon(ctx, operation, &record)
}

func checkAdoptionAbandonable(envelope HostAdoption, operation Adoption) error {
	if envelope.Confirmed {
		return errors.New("update: adoption is already confirmed")
	}
	if operation.Phase != AdoptionRecorded && operation.Phase != AdoptionWaiting {
		return fmt.Errorf("update: adoption %s is %s; only a recorded or waiting adoption can be abandoned", operation.ID, operation.Phase)
	}
	if operation.Notice != nil && !operation.Notice.DeliveredAt.IsZero() {
		return errors.New("update: adoption notice was delivered after every remote adopted")
	}
	for _, remote := range operation.Remotes {
		if adoptionRemoteTouched(remote) {
			return fmt.Errorf("update: adoption remote %s (%s) is %s and may have been changed; abandon refuses once a remote deploy can have reached SSH", remote.Host.ID, remote.SSH, remote.State)
		}
	}
	return nil
}

// adoptionRemoteTouched is conservative. Only "pending" proves no deploy began.
// A "blocked" remote with no recorded platform failed at its artifact lookup:
// no remote command exists for an empty platform, so SSH never ran. Any other
// state, including a blocked remote with a platform, may have reached SSH.
func adoptionRemoteTouched(remote AdoptionRemote) bool {
	switch remote.State {
	case AdoptionRemotePending:
		return false
	case AdoptionRemoteBlocked:
		return completePlatform(remote.Host.Platform)
	default:
		return true
	}
}

// finishAbandon withdraws the adoption's automatic policy before it removes the
// hold, so no updater sees an unfenced automatic grant the owner never kept.
// A crash between steps leaves the hold in place, and abandon can run again.
func (s *Store) finishAbandon(ctx context.Context, operation Adoption, record *AdoptionAbandonment) error {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	_, actual, err := s.adoption()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, operation) {
		return errors.New("update: adoption changed during abandon")
	}
	policy, err := s.hostPolicy()
	if err != nil {
		return err
	}
	if reflect.DeepEqual(policy, operation.Policy) {
		policy = HostPolicy{Schema: 1, Revision: policy.Revision + 1, Mode: HostManual}
		if err := s.writeAuthority("host-policy.json", policy); err != nil {
			return err
		}
		record.PolicyWithdrawn = true
	}
	record.Policy = policy
	if err := s.writeAuthority("adoption-abandoned-"+operation.ID+".json", record); err != nil {
		return err
	}
	return s.removeAuthority("host-adoption.json")
}

func (s *Store) removeAuthority(name string) error {
	dir, err := s.authorityDirectory()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("update: remove authority record: %w", err)
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("update: sync authority removal: %w", err)
	}
	return nil
}

// RestoreAdoptionHelper is the production helper restore for AbandonAdoption.
func RestoreAdoptionHelper(ctx context.Context, operation Adoption, executable string) (bool, error) {
	restored, err := updateinstall.AbandonAdoptionHelper(ctx, adoptionHelperConfig(operation, executable), operation.ID, operation.Pin)
	return restored, hostError(err)
}
