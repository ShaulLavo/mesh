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
func (s *Store) ConfigurationGrant(ctx context.Context) (ConfigurationGrant, error) {
	unlock, err := s.lockContext(ctx)
	if err != nil {
		return ConfigurationGrant{}, err
	}
	defer unlock()
	return s.configurationGrant()
}

func (s *Store) SetConfigurationGrant(ctx context.Context, expected uint64, hostID, coordinatorID string, allow bool, domain bool) (ConfigurationGrant, error) {
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
	if allow || domain {
		if _, err = PublicKey(coordinatorID); err != nil {
			return grant, err
		}
		grant.CoordinatorID = coordinatorID
	}
	if grant.CoordinatorID == "" {
		return grant, errors.New("configuration: no grant to revoke")
	}
	grant.Schema, grant.LocalHostID, grant.Revision, grant.UsageFeed = 1, hostID, expected+1, allow
	grant.DomainPolicy = domain
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
	if receipt.Schema != 1 || !ValidRunID(receipt.ChangeID) || len(receipt.ProposalDigest) != 64 || !slices.Contains([]string{configurationPrepared, configurationSaved, configurationConflict}, receipt.State) {
		return nil, errors.New("configuration: invalid component receipt")
	}
	if err = validateComponentReceipt(receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}
func receiptRevision(receipt *ConfigurationReceipt) uint64 {
	if receipt == nil {
		return 0
	}
	if feedReceiptState(receipt) == configurationSaved {
		return receipt.Revision
	}
	return receipt.Expected.Revision
}
func (s *Store) ConfigurationSnapshot(ctx context.Context, hostID string, writer ConfigurationWriter, observations []ConfigurationObservation, domains ...DomainWriter) (ConfigurationSnapshot, error) {
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
	if err != nil {
		return result, err
	}
	revision := uint64(0)
	if receipt != nil && receipt.Domain != nil {
		revision = domainReceiptRevision(receipt.Domain)
	}
	if len(domains) == 1 && domains[0] != nil {
		err = domains[0](ctx, func(file DomainFile) error {
			value := file.Value()
			result.Domain = &DomainExpectation{Revision: revision, Value: value, Digest: value.Digest()}
			return nil
		})
	}
	return result, err
}
func (s *Store) PutConfiguration(ctx context.Context, actor VerifiedActor, hostID string, trusted func(string) bool, writer ConfigurationWriter, request ConfigurationPut, domains ...DomainWriter) (ConfigurationReceipt, error) {
	if err := validateConfigurationPut(ctx, actor, hostID, request); err != nil {
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
	if prior != nil && prior.ChangeID == request.ChangeID && (prior.ProposalDigest != request.ProposalDigest || prior.Membership != request.Membership || prior.CoordinatorID != request.CoordinatorID || prior.GrantRevision != request.Recipient.GrantRevision) {
		return ConfigurationReceipt{}, problem("configuration.conflict", "change identity names different approved delivery")
	}
	if prior != nil && prior.State == configurationPrepared && prior.ChangeID != request.ChangeID && prior.GrantRevision == request.Recipient.GrantRevision {
		return ConfigurationReceipt{}, problem("configuration.conflict", "earlier component intent remains unresolved")
	}
	return s.applyConfigurationComponents(ctx, writer, request, prior, domains)
}
func (s *Store) applyConfigurationComponents(ctx context.Context, writer ConfigurationWriter, request ConfigurationPut, prior *ConfigurationReceipt, domains []DomainWriter) (ConfigurationReceipt, error) {
	var err error
	result := receiptForRequest(prior, request)
	if request.Recipient.Domain != nil {
		if len(domains) != 1 || domains[0] == nil {
			return result, errors.New("configuration: domain writer unavailable")
		}
		err = domains[0](ctx, func(file DomainFile) error { return s.putDomainConfiguration(file, prior, request, &result) })
		if err != nil {
			return result, err
		}
		prior = &result
	}
	if request.Recipient.Feed == nil {
		return result, nil
	}
	if writer == nil {
		return result, errors.New("configuration: recipient writer unavailable")
	}
	err = writer(ctx, func(file FeedFile) error {
		receipt, err := s.prepareConfiguration(prior, file.Value(), request)
		if err != nil {
			return err
		}
		result = receipt
		if feedReceiptState(&receipt) == configurationConflict {
			return nil
		}
		return s.saveConfiguration(file, &result, request.Recipient)
	})
	return result, err
}

func (s *Store) acceptConfiguration(actor VerifiedActor, hostID string, trusted func(string) bool, request ConfigurationPut) error {
	grant, err := s.configurationGrant()
	if err != nil {
		return err
	}
	if trusted == nil || !trusted(actor.id) || (request.Recipient.Feed != nil && !grant.UsageFeed || request.Recipient.Domain != nil && !grant.DomainPolicy) || grant.LocalHostID != hostID || grant.CoordinatorID != actor.id || grant.Revision != request.Recipient.GrantRevision {
		return problem("configuration.denied", "configuration grant or coordinator admission was withdrawn; inspect the target-local grant")
	}
	return nil
}
func (s *Store) prepareConfiguration(prior *ConfigurationReceipt, value FeedValue, request ConfigurationPut) (ConfigurationReceipt, error) {
	desired := FeedValue{Present: true, URL: request.Recipient.Feed.URL}
	if request.Recipient.Expected == nil {
		return ConfigurationReceipt{}, problem("configuration.invalid", "feed expectation required")
	}
	expected := *request.Recipient.Expected
	if expected.Digest != expected.Value.Digest() || expected.Revision == math.MaxUint64 {
		return ConfigurationReceipt{}, problem("configuration.invalid", "invalid configuration expectation")
	}
	receipt := receiptForRequest(prior, request)
	receipt.Expected, receipt.Desired, receipt.Revision, receipt.State, receipt.FeedState = expected, desired, expected.Revision+1, configurationPrepared, configurationPrepared
	switch {
	case prior != nil && prior.ChangeID == request.ChangeID && prior.Desired == desired && prior.Expected == expected:
		comparison := *prior
		comparison.State, comparison.Problem, comparison.FeedState = configurationPrepared, "", configurationPrepared
		if configurationDigest(comparison) != configurationDigest(receipt) {
			return receipt, problem("configuration.conflict", "change identity already names different selected values")
		}
		receipt = *prior
	case prior != nil && feedReceiptState(prior) == configurationPrepared && prior.GrantRevision == request.Recipient.GrantRevision:
		return receipt, problem("configuration.conflict", "an earlier component intent remains unresolved")
	case receiptRevision(prior) != expected.Revision || value.Digest() != expected.Digest:
		if request.Recipient.Domain != nil {
			receipt.FeedState, receipt.Problem = configurationConflict, "selected usage feed changed after domain publication"
			receipt.State = selectedConfigurationState(receipt, request.Recipient)
			return receipt, s.writeAuthority("config-receipt.json", receipt)
		}
		return receipt, problem("configuration.conflict", "selected usage feed changed after preview; request a fresh preview")
	}
	if feedReceiptState(&receipt) == configurationSaved && value != desired {
		return receipt, problem("configuration.conflict", "saved feed was edited locally")
	}
	if value != expected.Value && value != desired {
		receipt.FeedState, receipt.Problem = configurationConflict, "selected usage feed changed during delivery"
		receipt.State = selectedConfigurationState(receipt, request.Recipient)
	}
	return receipt, s.writeAuthority("config-receipt.json", receipt)
}
func (s *Store) saveConfiguration(file FeedFile, receipt *ConfigurationReceipt, selected RecipientConfiguration) error {
	if file.Value() != receipt.Desired {
		if err := file.Save(receipt.Desired); err != nil {
			return fmt.Errorf("configuration: save usage feed: %w", err)
		}
	}
	if err := file.Settle(); err != nil {
		return fmt.Errorf("configuration publication settlement: %w", err)
	}
	receipt.Problem, receipt.FeedState = "", configurationSaved
	receipt.State = selectedConfigurationState(*receipt, selected)
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
	DomainDigest        string     `json:"domainDigest,omitempty"`
	CatalogGeneration   uint64     `json:"catalogGeneration,omitempty"`
	ServiceVerification string     `json:"serviceVerification,omitempty"`
	Publication         uint64     `json:"publication"`
	InstanceID          string     `json:"instanceId"`
	HostID              string     `json:"hostId"`
	Generation          uint64     `json:"generation"`
	Enabled             bool       `json:"enabled"`
	State               string     `json:"state"`
	Digest              string     `json:"digest"`
	FeedRevision        uint64     `json:"feedRevision"`
	Failing             bool       `json:"failing"`
	Problem             string     `json:"problem,omitempty"`
	HTTPAt              time.Time  `json:"httpAt"`
	PublishedAt         time.Time  `json:"publishedAt"`
	UpstreamAt          *time.Time `json:"upstreamAt"`
	RenewedAt           time.Time  `json:"renewedAt"`
}

func feedReceiptState(receipt *ConfigurationReceipt) string {
	if receipt.FeedState != "" {
		return receipt.FeedState
	}
	if receipt.Domain == nil {
		return receipt.State
	}
	return ""
}
func domainReceiptRevision(receipt *DomainReceipt) uint64 {
	if receipt == nil {
		return 0
	}
	if receipt.State == configurationSaved {
		return receipt.Revision
	}
	return receipt.Expected.Revision
}
func validateComponentReceipt(receipt ConfigurationReceipt) error {
	if receipt.Revision != 0 {
		if receipt.Expected.Revision == math.MaxUint64 || receipt.Revision != receipt.Expected.Revision+1 || receipt.Expected.Digest != receipt.Expected.Value.Digest() {
			return errors.New("configuration: invalid feed receipt revision")
		}
		if err := validateFeedValue(receipt.Desired); err != nil {
			return err
		}
	}
	if receipt.Domain == nil {
		if receipt.Revision == 0 {
			return errors.New("configuration: empty component receipt")
		}
		return nil
	}
	domain := receipt.Domain
	if domain.Expected.Revision == math.MaxUint64 || domain.Revision != domain.Expected.Revision+1 || domain.Expected.Digest != domain.Expected.Value.Digest() || !domain.Desired.Present || !slices.Contains([]string{configurationPrepared, configurationSaved, configurationConflict}, domain.State) {
		return errors.New("configuration: invalid domain receipt")
	}
	if err := domain.Desired.Policy.Validate(); err != nil {
		return fmt.Errorf("validate saved domain receipt: %w", err)
	}
	return nil
}
func receiptForRequest(prior *ConfigurationReceipt, request ConfigurationPut) ConfigurationReceipt {
	receipt := ConfigurationReceipt{Schema: 1, ChangeID: request.ChangeID, ProposalDigest: request.ProposalDigest, CoordinatorID: request.CoordinatorID, HostID: request.Recipient.Host.ID, GrantRevision: request.Recipient.GrantRevision, Membership: request.Membership}
	if prior != nil {
		receipt.Expected, receipt.Desired, receipt.Revision, receipt.FeedState = prior.Expected, prior.Desired, prior.Revision, feedReceiptState(prior)
		receipt.Domain = prior.Domain
	}
	if request.Recipient.Feed != nil && (prior == nil || prior.ChangeID != request.ChangeID) {
		receipt.Expected = *request.Recipient.Expected
		receipt.Desired = FeedValue{Present: true, URL: request.Recipient.Feed.URL}
		receipt.Revision, receipt.FeedState = receipt.Expected.Revision+1, configurationPrepared
	}
	return receipt
}
func (s *Store) putDomainConfiguration(file DomainFile, prior *ConfigurationReceipt, request ConfigurationPut, result *ConfigurationReceipt) error {
	domain, err := prepareDomainReceipt(prior, file.Value(), request)
	if err != nil {
		return err
	}
	value, desired, expected := file.Value(), domain.Desired, domain.Expected
	result.Domain = &domain
	if domain.State == configurationConflict {
		result.State = selectedConfigurationState(*result, request.Recipient)
		return s.writeAuthority("config-receipt.json", result)
	}
	if value.Digest() != expected.Digest && value.Digest() != desired.Digest() || domain.State == configurationSaved && value.Digest() != desired.Digest() {
		domain.State, domain.Problem = configurationConflict, "selected domain policy changed during delivery"
		result.State = selectedConfigurationState(*result, request.Recipient)
		return s.writeAuthority("config-receipt.json", result)
	}
	result.State = configurationPrepared
	if err := s.writeAuthority("config-receipt.json", result); err != nil {
		return err
	}
	generation, err := file.Prepare(desired.Digest())
	if err != nil {
		return fmt.Errorf("prepare catalog invalidation: %w", err)
	}
	domain.Generation = generation
	if err := s.writeAuthority("config-receipt.json", result); err != nil {
		return err
	}
	if value.Digest() != desired.Digest() || !value.Present {
		if err := file.Save(desired); err != nil {
			return fmt.Errorf("save domain policy: %w", err)
		}
	}
	if err := file.Settle(); err != nil {
		return fmt.Errorf("settle domain publication: %w", err)
	}
	domain.State, domain.Problem = configurationSaved, ""
	result.State = selectedConfigurationState(*result, request.Recipient)
	return s.writeAuthority("config-receipt.json", result)
}

func validateConfigurationPut(ctx context.Context, actor VerifiedActor, hostID string, request ConfigurationPut) error {
	if err := requireConfigurationActor(ctx, actor, false, hostID); err != nil {
		return err
	}
	if !ValidRunID(request.ChangeID) || len(request.ProposalDigest) != 64 || len(request.Membership) != 64 || request.CoordinatorID != actor.id || request.Recipient.Host.ID != hostID {
		return problem("configuration.invalid", "configuration delivery identity differs")
	}
	if request.Recipient.Feed == nil && request.Recipient.Domain == nil {
		return problem("configuration.invalid", "explicit component required")
	}
	if request.Recipient.Feed != nil {
		if request.Recipient.Expected == nil {
			return problem("configuration.invalid", "selected feed requires expectation")
		}
		if err := validateRecipientFeed(request.Recipient); err != nil {
			return err
		}
	}
	if err := validateDomainSelection(request.Recipient.Domain, request.Recipient.DomainExpected); err != nil {
		return err
	}
	return nil
}

func prepareDomainReceipt(prior *ConfigurationReceipt, value DomainValue, request ConfigurationPut) (DomainReceipt, error) {
	expected := *request.Recipient.DomainExpected
	desired := domainDesired(*request.Recipient.Domain, expected.Value)
	domain := DomainReceipt{Expected: expected, Desired: desired, Revision: expected.Revision + 1, State: configurationPrepared}
	if prior != nil && prior.ChangeID == request.ChangeID {
		return replayDomainReceipt(prior.Domain, domain)
	}
	var previous *DomainReceipt
	if prior != nil {
		previous = prior.Domain
	}
	if domainReceiptRevision(previous) != expected.Revision {
		return domain, problem("configuration.conflict", "domain receipt revision changed after preview")
	}
	if value.Digest() != expected.Digest || value.Present != expected.Value.Present {
		return domain, problem("configuration.conflict", "selected domain policy changed after preview")
	}
	return domain, nil
}
func replayDomainReceipt(prior *DomainReceipt, desired DomainReceipt) (DomainReceipt, error) {
	if prior == nil || configurationDigest(prior.Expected) != configurationDigest(desired.Expected) || configurationDigest(prior.Desired) != configurationDigest(desired.Desired) {
		return desired, problem("configuration.conflict", "domain intent differs from approved values")
	}
	return *prior, nil
}

func selectedConfigurationState(receipt ConfigurationReceipt, selected RecipientConfiguration) string {
	feed, domain := "", ""
	if selected.Feed != nil {
		feed = feedReceiptState(&receipt)
	}
	if selected.Domain != nil {
		domain = receipt.Domain.State
	}
	if feed == configurationPrepared || domain == configurationPrepared {
		return configurationPrepared
	}
	if feed == configurationConflict || domain == configurationConflict {
		return configurationConflict
	}
	return configurationSaved
}
