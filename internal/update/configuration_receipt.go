package update

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/transport"
)

func (s *Store) configurationGrant() (ConfigurationGrant, error) {
	var grant ConfigurationGrant
	err := s.readAuthority("config-grant.json", &grant)
	if errors.Is(err, os.ErrNotExist) {
		return grant, nil
	}
	if err != nil {
		return grant, err
	}
	if grant.Schema != 1 || grant.Revision == 0 {
		return grant, errors.New("configuration: invalid grant")
	}
	if _, err = PublicKey(grant.LocalHostID); err != nil {
		return grant, err
	}
	_, err = PublicKey(grant.CoordinatorID)
	return grant, err
}
func (s *Store) SetConfigurationGrant(ctx context.Context, expected uint64, hostID, coordinatorID string, allow bool) (ConfigurationGrant, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return ConfigurationGrant{}, err
	}
	defer unlock()
	grant, err := s.configurationGrant()
	if err != nil {
		return grant, err
	}
	if grant.Revision != expected || expected == math.MaxUint64 {
		return grant, problem("configuration.conflict", "configuration grant revision changed")
	}
	if _, err = PublicKey(hostID); err != nil {
		return grant, err
	}
	if allow {
		if _, err = PublicKey(coordinatorID); err != nil {
			return grant, err
		}
		grant.CoordinatorID = coordinatorID
	}
	if grant.CoordinatorID == "" {
		return grant, errors.New("configuration: no grant to revoke")
	}
	grant.Schema, grant.LocalHostID, grant.Revision, grant.UsageFeed = 1, hostID, expected+1, allow
	return grant, s.writeAuthority("config-grant.json", grant)
}
func (s *Store) configurationReceipt() (*ConfigurationReceipt, error) {
	var receipt ConfigurationReceipt
	err := s.readAuthority("config-receipt.json", &receipt)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if receipt.Schema != 1 || !ValidRunID(receipt.ChangeID) || len(receipt.ProposalDigest) != 64 || receipt.Revision == 0 || receipt.Expected.Revision == math.MaxUint64 || receipt.Revision != receipt.Expected.Revision+1 || receipt.Expected.Digest != receipt.Expected.Value.Digest() || !slices.Contains([]string{configurationPrepared, configurationSaved, configurationConflict}, receipt.State) {
		return nil, errors.New("configuration: invalid component receipt")
	}
	if err = validateFeedValue(receipt.Desired); err != nil {
		return nil, err
	}
	return &receipt, nil
}
func receiptRevision(receipt *ConfigurationReceipt) uint64 {
	if receipt == nil {
		return 0
	}
	if receipt.State == configurationSaved {
		return receipt.Revision
	}
	return receipt.Expected.Revision
}
func (s *Store) ConfigurationSnapshot(ctx context.Context, hostID string, writer ConfigurationWriter, observations []ConfigurationObservation) (ConfigurationSnapshot, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	defer unlock()
	grant, err := s.configurationGrant()
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	receipt, err := s.configurationReceipt()
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	result := ConfigurationSnapshot{HostID: hostID, Grant: grant, Receipt: receipt, Observers: observations}
	if writer == nil {
		return result, errors.New("configuration: recipient writer unavailable")
	}
	err = writer(ctx, func(file FeedFile) error {
		value := file.Value()
		result.Feed = ComponentExpectation{Revision: receiptRevision(receipt), Value: value, Digest: value.Digest()}
		return nil
	})
	return result, err
}
func (s *Store) PutConfiguration(ctx context.Context, actor VerifiedActor, hostID string, trusted func(string) bool, writer ConfigurationWriter, request ConfigurationPut) (ConfigurationReceipt, error) {
	if err := requireConfigurationActor(ctx, actor, false, hostID); err != nil {
		return ConfigurationReceipt{}, err
	}
	if !ValidRunID(request.ChangeID) || len(request.ProposalDigest) != 64 || len(request.Membership) != 64 || request.CoordinatorID != actor.id || request.Recipient.Host.ID != hostID {
		return ConfigurationReceipt{}, problem("configuration.invalid", "configuration delivery identity differs")
	}
	if err := ValidateDeliveredFeed(request.Recipient.Feed.URL); err != nil {
		return ConfigurationReceipt{}, err
	}
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return ConfigurationReceipt{}, err
	}
	defer unlock()
	if err = s.acceptConfiguration(actor, hostID, trusted, request); err != nil {
		return ConfigurationReceipt{}, err
	}
	prior, err := s.configurationReceipt()
	if err != nil {
		return ConfigurationReceipt{}, err
	}
	if writer == nil {
		return ConfigurationReceipt{}, errors.New("configuration: recipient writer unavailable")
	}
	var result ConfigurationReceipt
	err = writer(ctx, func(file FeedFile) error {
		receipt, err := s.prepareConfiguration(prior, file.Value(), request)
		if err != nil {
			return err
		}
		result = receipt
		if receipt.State == configurationConflict {
			return nil
		}
		return s.saveConfiguration(file, &result)
	})
	return result, err
}
func (s *Store) acceptConfiguration(actor VerifiedActor, hostID string, trusted func(string) bool, request ConfigurationPut) error {
	grant, err := s.configurationGrant()
	if err != nil {
		return err
	}
	if trusted == nil || !trusted(actor.id) || !grant.UsageFeed || grant.LocalHostID != hostID || grant.CoordinatorID != actor.id || grant.Revision != request.Recipient.GrantRevision {
		return problem("configuration.denied", "configuration grant or coordinator admission was withdrawn; inspect the target-local grant")
	}
	return nil
}
func (s *Store) prepareConfiguration(prior *ConfigurationReceipt, value FeedValue, request ConfigurationPut) (ConfigurationReceipt, error) {
	desired := FeedValue{Present: true, URL: request.Recipient.Feed.URL}
	expected := request.Recipient.Expected
	if expected.Digest != expected.Value.Digest() || expected.Revision == math.MaxUint64 {
		return ConfigurationReceipt{}, problem("configuration.invalid", "invalid configuration expectation")
	}
	receipt := ConfigurationReceipt{Schema: 1, ChangeID: request.ChangeID, ProposalDigest: request.ProposalDigest, CoordinatorID: request.CoordinatorID, HostID: request.Recipient.Host.ID, GrantRevision: request.Recipient.GrantRevision, Membership: request.Membership, Expected: expected, Desired: desired, Revision: expected.Revision + 1, State: configurationPrepared}
	switch {
	case prior != nil && prior.ChangeID == request.ChangeID:
		comparison := *prior
		comparison.State, comparison.Problem = configurationPrepared, ""
		if configurationDigest(comparison) != configurationDigest(receipt) {
			return receipt, problem("configuration.conflict", "change identity already names different selected values")
		}
		receipt = *prior
	case prior != nil && prior.State == configurationPrepared && prior.GrantRevision == request.Recipient.GrantRevision:
		return receipt, problem("configuration.conflict", "an earlier component intent remains unresolved")
	case receiptRevision(prior) != expected.Revision || value.Digest() != expected.Digest:
		return receipt, problem("configuration.conflict", "selected usage feed changed after preview; request a fresh preview")
	}
	if receipt.State == configurationSaved && value != desired {
		return receipt, problem("configuration.conflict", "saved feed was edited locally")
	}
	if value != expected.Value && value != desired {
		receipt.State, receipt.Problem = configurationConflict, "selected usage feed changed during delivery"
	}
	return receipt, s.writeAuthority("config-receipt.json", receipt)
}
func (s *Store) saveConfiguration(file FeedFile, receipt *ConfigurationReceipt) error {
	if file.Value() != receipt.Desired {
		if err := file.Save(receipt.Desired); err != nil {
			return fmt.Errorf("configuration: save usage feed: %w", err)
		}
	}
	if err := file.Settle(); err != nil {
		return fmt.Errorf("configuration publication settlement: %w", err)
	}
	receipt.State, receipt.Problem = configurationSaved, ""
	return s.writeAuthority("config-receipt.json", receipt)
}
func requireConfigurationActor(ctx context.Context, actor VerifiedActor, local bool, hostID string) error {
	if actor.id == "" {
		return problem("configuration.denied", "verified configuration actor required")
	}
	peer, remote := transport.Peer(ctx)
	if remote && peer.Identity != actor.id {
		return problem("configuration.denied", "configuration signer differs from transport peer")
	}
	if local && (remote || !actor.local || actor.id != hostID) {
		return problem("configuration.denied", "dashboard observation and retirement require the local signed identity")
	}
	return nil
}

const ConfigurationObserverLease = 30 * time.Second

type ConfigurationObservation struct {
	Publication  uint64     `json:"publication"`
	InstanceID   string     `json:"instanceId"`
	HostID       string     `json:"hostId"`
	Generation   uint64     `json:"generation"`
	Enabled      bool       `json:"enabled"`
	State        string     `json:"state"`
	Digest       string     `json:"digest"`
	FeedRevision uint64     `json:"feedRevision"`
	Failing      bool       `json:"failing"`
	Problem      string     `json:"problem,omitempty"`
	HTTPAt       time.Time  `json:"httpAt"`
	PublishedAt  time.Time  `json:"publishedAt"`
	UpstreamAt   *time.Time `json:"upstreamAt"`
	RenewedAt    time.Time  `json:"renewedAt"`
}
