package update

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/updateinstall"
)

type HostMode string

const (
	HostManual      HostMode = "manual"
	HostAutomatic   HostMode = "automatic"
	DefaultHostMode          = HostManual
)

type HostPolicy struct {
	Schema   int           `json:"schema"`
	Revision uint64        `json:"revision"`
	Mode     HostMode      `json:"mode"`
	After    []string      `json:"after,omitempty"`
	Soak     time.Duration `json:"soak,omitempty"`
}

func (s *Store) hostPolicy() (HostPolicy, error) {
	var policy HostPolicy
	err := s.readAuthority("host-policy.json", &policy)
	if errors.Is(err, os.ErrNotExist) {
		return HostPolicy{Schema: 1, Mode: DefaultHostMode}, nil
	}
	if err != nil {
		return policy, err
	}
	return policy, validateHostPolicy(policy)
}

func validateHostPolicy(policy HostPolicy) error {
	if policy.Schema != 1 || policy.Revision == 0 || policy.Mode != HostAutomatic && policy.Mode != HostManual || policy.Soak < 0 {
		return errors.New("update: invalid host policy")
	}
	for i, id := range policy.After {
		if _, err := PublicKey(id); err != nil {
			return err
		}
		if slices.Contains(policy.After[:i], id) {
			return errors.New("update: duplicate host predecessor")
		}
	}
	return nil
}

func (s *Store) HostPolicy(ctx context.Context) (HostPolicy, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return HostPolicy{}, err
	}
	defer unlock()
	return s.hostPolicy()
}

// SetHostPolicy is an in-process local boundary. Public controls must first
// authenticate a local Unix caller. Engine acceptance takes installation then
// authority; policy and ownership writers never take installation under authority.
func (s *Store) SetHostPolicy(ctx context.Context, expected uint64, policy HostPolicy) (HostPolicy, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return HostPolicy{}, err
	}
	defer unlock()
	prior, err := s.hostPolicy()
	if err != nil {
		return prior, err
	}
	if prior.Revision != expected || expected == math.MaxUint64 {
		return prior, errors.New("update: host policy revision changed")
	}
	policy.Schema, policy.Revision = 1, expected+1
	if err := validateHostPolicy(policy); err != nil {
		return prior, err
	}
	return policy, s.writeAuthority("host-policy.json", policy)
}

func (s *Store) AuthorizeHostAcceptance(manual updateinstall.Acceptance) updateinstall.Acceptance {
	return func(ctx context.Context, request updateinstall.Request, kind updateinstall.AcceptanceKind, commit func() error) error {
		if !updateinstall.IsHostAcceptance(ctx) {
			if manual != nil {
				return manual(ctx, request, kind, commit)
			}
			if request.Automatic != nil {
				return errors.New("update: coordinator acceptance unavailable")
			}
			return commit()
		}
		unlock, err := s.lockContext(ctx)
		if err != nil {
			return err
		}
		defer unlock()
		return s.authorizeHost(request, commit)
	}
}

func (s *Store) authorizeHost(request updateinstall.Request, commit func() error) error {
	policy, err := s.hostPolicy()
	if err != nil {
		return err
	}
	state, err := s.hostUpdate()
	if err != nil {
		return err
	}
	if policy.Mode != HostAutomatic || state.Owned == nil || state.Owned.PolicyRevision != policy.Revision {
		return errors.New("update: host authority withdrawn or missing")
	}
	if err := state.Owned.matches(request); err != nil {
		return err
	}
	if reason, err := s.hostYield(); err != nil || reason != "" {
		return fmt.Errorf("update: host acceptance held (%s): %w", reason, errors.Join(err, errors.New("another authority owns installation")))
	}
	return commit()
}

func (s *Store) hostYield() (string, error) {
	consent, err := s.recipientConsent()
	if err != nil {
		return "", err
	}
	if consent.Enabled {
		return "coordinator consent enabled", nil
	}
	var adoption HostAdoption
	err = s.readAuthority("host-adoption.json", &adoption)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := adoption.validate(); err != nil {
		return "", err
	}
	if !adoption.Confirmed {
		return "adoption awaiting confirmation", nil
	}
	return "", nil
}
