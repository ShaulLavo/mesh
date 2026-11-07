package update

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

type RecipientConsent struct {
	Schema        int    `json:"schema"`
	Revision      uint64 `json:"revision"`
	LocalHostID   string `json:"localHostId"`
	CoordinatorID string `json:"coordinatorId"`
	Membership    string `json:"membership"`
	Epoch         uint64 `json:"epoch"`
	Enabled       bool   `json:"enabled"`
}
type ConsentChange struct {
	LocalHostID   string
	CoordinatorID string
	Fleet         Fleet
	Epoch         uint64
	Enabled       bool
}

func (s *Store) recipientConsent() (RecipientConsent, error) {
	var consent RecipientConsent
	err := s.readAuthority("automatic-consent.json", &consent)
	if errors.Is(err, os.ErrNotExist) {
		return RecipientConsent{}, nil
	}
	if err != nil {
		return RecipientConsent{}, err
	}
	if consent.Schema != 1 || consent.Revision == 0 || consent.Epoch == 0 || len(consent.Membership) != 64 {
		return consent, errors.New("update: invalid automatic consent")
	}
	if _, err = PublicKey(consent.LocalHostID); err != nil {
		return consent, err
	}
	if _, err = PublicKey(consent.CoordinatorID); err != nil {
		return consent, err
	}
	return consent, nil
}
func (s *Store) RecipientConsent(ctx context.Context) (RecipientConsent, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return RecipientConsent{}, err
	}
	defer unlock()
	return s.recipientConsent()
}
func (s *Store) SetRecipientConsent(ctx context.Context, expected uint64, change ConsentChange) (RecipientConsent, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return RecipientConsent{}, err
	}
	defer unlock()
	prior, err := s.recipientConsent()
	if err != nil {
		return prior, err
	}
	if prior.Revision != expected || expected == math.MaxUint64 {
		return prior, problem("automatic.conflict", "recipient consent revision changed")
	}
	if !change.Enabled {
		if prior.Revision == 0 {
			return prior, errors.New("update: no automatic recipient consent to revoke")
		}
		prior.Enabled = false
		prior.Revision++
		return prior, s.writeAuthority("automatic-consent.json", prior)
	}
	if err = validateConsentReplacement(prior, change); err != nil {
		return prior, err
	}

	if err = s.consentReplacementSettled(); err != nil {
		return prior, err
	}
	next := RecipientConsent{1, expected + 1, change.LocalHostID, change.CoordinatorID, change.Fleet.Digest(), change.Epoch, true}
	return next, s.writeAuthority("automatic-consent.json", next)
}
func (s *Store) consentReplacementSettled() error {
	status, err := updateinstall.Read(s.stateDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("update: inspect consent replacement: %w", err)
	}
	if status.Request.Automatic == nil {
		return nil
	}
	if status.Phase == updateinstall.Committed || status.Phase == updateinstall.Cancelled || status.Phase == updateinstall.RolledBack || status.Phase == updateinstall.Failed {
		return nil
	}
	return problem("automatic.conflict", "automatic installation still has unresolved work")
}
func (s *Store) AuthorizeAcceptance(actor VerifiedActor, trusted func(string) bool) updateinstall.Acceptance {
	return func(ctx context.Context, request updateinstall.Request, kind updateinstall.AcceptanceKind, commit func() error) error {
		if request.Automatic == nil {
			return commit()
		}
		if err := RequireAutomaticActor(ctx, actor); err != nil {
			return err
		}
		unlock, err := s.lockContext(ctx)
		if err != nil {
			return err
		}
		defer unlock()
		consent, err := s.recipientConsent()
		if err != nil {
			return err
		}
		ref := request.Automatic
		if trusted == nil || !trusted(actor.id) || actor.id != ref.CoordinatorID {
			return problem("automatic.consent_revoked", "automatic coordinator is no longer an update administrator")
		}
		if !consent.Enabled || consent.LocalHostID != request.TargetID || consent.CoordinatorID != actor.id || consent.Epoch != ref.Epoch || consent.Membership != ref.Membership || consent.Revision != ref.ConsentRevision {
			return problem("automatic.consent_revoked", "automatic recipient consent was withdrawn or changed; inspect consent locally")
		}
		return commit()
	}
}
func (c *Coordinator) observeConsent(ctx context.Context, host Host) MemberConsent {
	observation := MemberConsent{ObservedAt: time.Now().UTC()}
	inspect, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Remote.Call(inspect, host, "automatic.consent", nil, &observation.Consent); err != nil {
		observation.Problem = boundedProblem(err.Error())
		observation.Consent.LocalHostID = host.ID
	}
	if observation.Consent.LocalHostID != host.ID {
		observation.Problem = "recipient consent identity mismatch"
	}
	return observation
}
func (c *Coordinator) checkAutomaticScope(ctx context.Context) error {
	unlock, err := c.Store.lockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := c.Store.readAutomatic()
	if err != nil || r == nil {
		return err
	}
	return c.Store.fenceAutomatic(r, c.automaticScope())
}

func (s *Store) stateDir() string { return filepath.Dir(filepath.Dir(s.directory)) }

func validateConsentReplacement(prior RecipientConsent, change ConsentChange) error {
	if prior.Enabled {
		return problem("automatic.conflict", "revoke the current automatic binding before authorizing a replacement")
	}
	if change.Epoch == 0 {
		return errors.New("update: exact pending enrollment epoch required")
	}
	if _, err := PublicKey(change.CoordinatorID); err != nil {
		return err
	}
	if err := change.Fleet.Validate(); err != nil {
		return err
	}
	local, coordinator := false, false
	for _, host := range change.Fleet.Members {
		local = local || host.ID == change.LocalHostID
		coordinator = coordinator || host.ID == change.CoordinatorID
	}
	if !local || !coordinator {
		return errors.New("update: local recipient and coordinator must be exact saved fleet members")
	}
	return nil
}
